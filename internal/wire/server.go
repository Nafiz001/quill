package wire

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/Nafiz001/quill/internal/db"
	"github.com/Nafiz001/quill/internal/exec"
	"github.com/Nafiz001/quill/internal/sql"
	"github.com/Nafiz001/quill/internal/types"
)

// Server speaks the PostgreSQL protocol on behalf of a quill database.
type Server struct {
	db   *db.DB
	log  *slog.Logger
	addr string

	ln     net.Listener
	lnMu   sync.Mutex
	closed atomic.Bool
	wg     sync.WaitGroup

	nextPID atomic.Int32
}

// NewServer builds a server over an open database.
func NewServer(database *db.DB, addr string, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	return &Server{db: database, addr: addr, log: logger}
}

// Listen binds the address without accepting, so a test can learn the port.
func (s *Server) Listen() error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.addr, err)
	}
	s.lnMu.Lock()
	s.ln = ln
	s.lnMu.Unlock()
	return nil
}

// Addr returns the bound address.
func (s *Server) Addr() net.Addr {
	s.lnMu.Lock()
	defer s.lnMu.Unlock()
	if s.ln == nil {
		return nil
	}
	return s.ln.Addr()
}

// Serve accepts connections until the context is cancelled or Close is called.
func (s *Server) Serve(ctx context.Context) error {
	s.lnMu.Lock()
	bound := s.ln != nil
	s.lnMu.Unlock()
	if !bound {
		if err := s.Listen(); err != nil {
			return err
		}
	}

	s.log.Info("quill accepting connections",
		"addr", s.Addr().String(), "database", s.db.Path())

	go func() {
		<-ctx.Done()
		s.Close()
	}()

	for {
		conn, err := s.ln.Accept()
		if err != nil {
			if s.closed.Load() {
				break
			}
			s.log.Error("accept failed", "err", err)
			continue
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handle(conn)
		}()
	}
	s.wg.Wait()
	return nil
}

// Close stops accepting and waits for connections to finish.
func (s *Server) Close() {
	if !s.closed.CompareAndSwap(false, true) {
		return
	}
	s.lnMu.Lock()
	if s.ln != nil {
		_ = s.ln.Close()
	}
	s.lnMu.Unlock()
}

// conn is one client connection.
type conn struct {
	srv     *Server
	nc      net.Conn
	r       *reader
	w       *writer
	session *db.Session
	// failed marks a transaction that hit an error, so every later statement
	// in it is refused until ROLLBACK. PostgreSQL does this, and psql shows it
	// as the `!>` prompt.
	failed bool
	// prepared holds named statements from the extended query protocol.
	prepared map[string]*prepared
	portals  map[string]*prepared
}

// prepared is a parsed statement awaiting execution.
type prepared struct {
	query string
	stmt  sql.Statement
}

func (s *Server) handle(nc net.Conn) {
	defer nc.Close()

	c := &conn{
		srv:      s,
		nc:       nc,
		r:        newReader(nc),
		w:        newWriter(nc),
		session:  s.db.NewSession(),
		prepared: make(map[string]*prepared),
		portals:  make(map[string]*prepared),
	}

	if err := c.startup(); err != nil {
		if !errors.Is(err, io.EOF) {
			s.log.Debug("startup failed", "remote", nc.RemoteAddr(), "err", err)
		}
		return
	}

	s.log.Debug("client connected", "remote", nc.RemoteAddr())
	if err := c.loop(); err != nil && !errors.Is(err, io.EOF) {
		s.log.Debug("connection ended", "remote", nc.RemoteAddr(), "err", err)
	}
}

// startup performs the connection handshake.
func (c *conn) startup() error {
	for {
		params, version, err := c.r.readStartup()
		if err != nil {
			return err
		}

		switch version {
		case sslRequestCode, gssEncRequestCode:
			// A single 'N' means "no, plain text please". The client then
			// sends a real startup packet, so the loop goes round again.
			if _, err := c.nc.Write([]byte{'N'}); err != nil {
				return err
			}
			continue
		case cancelRequestCode:
			// Query cancellation is not implemented; the connection is simply
			// closed, which the client treats as the request having failed.
			return io.EOF
		case protocolVersion:
			c.srv.log.Debug("startup", "user", params["user"], "database", params["database"])
			return c.completeStartup()
		default:
			c.w.errorResponse("FATAL", "0A000",
				fmt.Sprintf("unsupported protocol version %d", version))
			c.w.flush()
			return fmt.Errorf("unsupported protocol version %d", version)
		}
	}
}

func (c *conn) completeStartup() error {
	c.w.authenticationOK()

	// These are what a client reads to configure itself. server_version
	// claims PostgreSQL 14 because clients gate features on it, and claiming
	// a version whose features quill does not have would be worse than
	// claiming an older one.
	c.w.parameterStatus("server_version", "14.0 (quill 0.1.0)")
	c.w.parameterStatus("server_encoding", "UTF8")
	c.w.parameterStatus("client_encoding", "UTF8")
	c.w.parameterStatus("DateStyle", "ISO, MDY")
	c.w.parameterStatus("integer_datetimes", "on")
	c.w.parameterStatus("standard_conforming_strings", "on")

	c.w.backendKeyData(c.srv.nextPID.Add(1), 0)
	c.w.readyForQuery(c.status())
	return c.w.flush()
}

// status reports transaction state for ReadyForQuery.
func (c *conn) status() byte {
	switch {
	case c.failed:
		return statusFailed
	case c.session.InTransaction():
		return statusInTxn
	default:
		return statusIdle
	}
}

// loop handles messages until the client disconnects.
func (c *conn) loop() error {
	for {
		msg, err := c.r.next()
		if err != nil {
			return err
		}

		switch msg.typ {
		case msgQuery:
			query, _, _ := readCString(msg.payload)
			c.simpleQuery(query)
			c.w.readyForQuery(c.status())
			if err := c.w.flush(); err != nil {
				return err
			}

		case msgParse:
			c.handleParse(msg.payload)
		case msgBind:
			c.handleBind(msg.payload)
		case msgDescribe:
			c.handleDescribe(msg.payload)
		case msgExecute:
			c.handleExecute(msg.payload)
		case msgClose:
			c.w.closeComplete()

		case msgSync:
			// Sync ends an extended-query batch and is the only thing that
			// sends ReadyForQuery in that flow.
			c.w.readyForQuery(c.status())
			if err := c.w.flush(); err != nil {
				return err
			}

		case msgFlush:
			if err := c.w.flush(); err != nil {
				return err
			}

		case msgTerminate:
			return io.EOF

		default:
			c.w.errorResponse("ERROR", "08P01",
				fmt.Sprintf("unsupported message type %q", string(msg.typ)))
			c.w.readyForQuery(c.status())
			if err := c.w.flush(); err != nil {
				return err
			}
		}
	}
}

// simpleQuery runs a query string, which may hold several statements.
func (c *conn) simpleQuery(query string) {
	if strings.TrimSpace(query) == "" {
		c.w.emptyQueryResponse()
		return
	}

	statements, err := sql.ParseMany(query)
	if err != nil {
		c.reportError(err)
		return
	}
	if len(statements) == 0 {
		c.w.emptyQueryResponse()
		return
	}

	for _, stmt := range statements {
		if !c.runStatement(stmt) {
			// A failed statement abandons the rest of the batch, which is
			// what PostgreSQL does for a simple query.
			return
		}
	}
}

// runStatement executes one statement and writes its results.
func (c *conn) runStatement(stmt sql.Statement) bool {
	// Inside a failed transaction only ROLLBACK is accepted, so a client
	// cannot half-apply a transaction it already knows is broken.
	if c.failed {
		if txn, ok := stmt.(*sql.TransactionStmt); !ok || txn.Action != "ROLLBACK" {
			c.w.errorResponse("ERROR", "25P02",
				"current transaction is aborted, commands ignored until end of transaction block")
			return false
		}
	}

	result, err := c.session.ExecuteStatement(stmt)
	if err != nil {
		c.reportError(err)
		if c.session.InTransaction() {
			c.failed = true
		}
		return false
	}
	if _, ok := stmt.(*sql.TransactionStmt); ok {
		c.failed = false
	}

	if len(result.Columns) > 0 {
		c.w.rowDescription(describeColumns(result))
		for _, row := range result.Rows {
			c.w.dataRow(encodeRow(row))
		}
	}
	c.w.commandComplete(result.Tag)
	return true
}

// describeColumns builds the RowDescription for a result.
//
// The type is inferred from the first row's values, because quill resolves
// expression types at execution rather than at plan time. With no rows there
// is nothing to infer from, so text is used -- every value has a text
// representation, and a client asked to parse it as text will not misread it.
func describeColumns(result *exec.Result) []field {
	fields := make([]field, len(result.Columns))
	for i, name := range result.Columns {
		oid := int32(oidText)
		if len(result.Rows) > 0 && i < len(result.Rows[0]) {
			oid = typeOID(result.Rows[0][i].Type)
		}
		fields[i] = field{name: name, oid: oid}
	}
	return fields
}

func typeOID(t types.Type) int32 {
	switch t {
	case types.TypeInt:
		return oidInt8
	case types.TypeFloat:
		return oidFloat8
	case types.TypeBool:
		return oidBool
	default:
		return oidText
	}
}

// encodeRow renders a row in the protocol's text format.
func encodeRow(row exec.Row) [][]byte {
	out := make([][]byte, len(row))
	for i, v := range row {
		if v.IsNull() {
			// nil becomes length -1 on the wire, which is SQL NULL. An empty
			// slice would be the empty string instead -- a different value.
			out[i] = nil
			continue
		}
		if v.Type == types.TypeBool {
			// PostgreSQL's text format for booleans is t and f, not true and
			// false, and clients parse it strictly.
			if v.Bool {
				out[i] = []byte("t")
			} else {
				out[i] = []byte("f")
			}
			continue
		}
		out[i] = []byte(v.String())
	}
	return out
}

// reportError maps a Go error onto a protocol error response.
func (c *conn) reportError(err error) {
	c.w.errorResponse("ERROR", sqlState(err), err.Error())
}

// sqlState guesses a SQLSTATE code from the error text.
//
// Clients branch on these -- a driver decides whether to retry, and psql
// decides what to highlight -- so a plausible code is worth more than a
// blanket "internal error". Deriving it from the message is a shortcut; the
// tidier design carries the code on the error itself, and is noted as future
// work.
func sqlState(err error) string {
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "does not exist") && strings.Contains(msg, "table"):
		return "42P01" // undefined_table
	case strings.Contains(msg, "column") && strings.Contains(msg, "does not exist"):
		return "42703" // undefined_column
	case strings.Contains(msg, "already exists"):
		return "42P07" // duplicate_table
	case strings.Contains(msg, "duplicate key"):
		return "23505" // unique_violation
	case strings.Contains(msg, "cannot be null"):
		return "23502" // not_null_violation
	case strings.Contains(msg, "division by zero"):
		return "22012"
	case strings.Contains(msg, "at position"), strings.Contains(msg, "expected"):
		return "42601" // syntax_error
	default:
		return "XX000" // internal_error
	}
}

// --- extended query protocol ---
//
// Drivers use this rather than the simple query flow, because it separates
// parsing from execution and is what carries bound parameters. quill parses
// and executes, but does not yet substitute parameters, so a statement with
// placeholders is refused rather than silently mis-run.

func (c *conn) handleParse(payload []byte) {
	name, rest, ok := readCString(payload)
	if !ok {
		c.reportError(fmt.Errorf("malformed Parse message"))
		return
	}
	query, _, ok := readCString(rest)
	if !ok {
		c.reportError(fmt.Errorf("malformed Parse message"))
		return
	}

	if strings.TrimSpace(query) == "" {
		c.prepared[name] = &prepared{query: query}
		c.w.parseComplete()
		return
	}
	if strings.Contains(query, "$1") {
		c.reportError(fmt.Errorf("bound parameters are not supported yet"))
		return
	}

	stmt, err := sql.Parse(query)
	if err != nil {
		c.reportError(err)
		return
	}
	c.prepared[name] = &prepared{query: query, stmt: stmt}
	c.w.parseComplete()
}

func (c *conn) handleBind(payload []byte) {
	portal, rest, ok := readCString(payload)
	if !ok {
		c.reportError(fmt.Errorf("malformed Bind message"))
		return
	}
	statement, _, ok := readCString(rest)
	if !ok {
		c.reportError(fmt.Errorf("malformed Bind message"))
		return
	}

	p, found := c.prepared[statement]
	if !found {
		c.reportError(fmt.Errorf("prepared statement %q does not exist", statement))
		return
	}
	c.portals[portal] = p
	c.w.bindComplete()
}

func (c *conn) handleDescribe(payload []byte) {
	if len(payload) < 1 {
		c.reportError(fmt.Errorf("malformed Describe message"))
		return
	}
	kind := payload[0]
	name, _, _ := readCString(payload[1:])

	var p *prepared
	if kind == 'S' {
		p = c.prepared[name]
		// A statement description also reports its parameter types; quill has
		// none, so the list is empty.
		c.w.parameterDescription(nil)
	} else {
		p = c.portals[name]
	}

	// Without executing, the column types are unknown, so the client is told
	// there is no row description rather than being given a wrong one. psql
	// tolerates this and reads the description that arrives with Execute.
	if p == nil || p.stmt == nil {
		c.w.noData()
		return
	}
	if _, isSelect := p.stmt.(*sql.SelectStmt); !isSelect {
		if _, isExplain := p.stmt.(*sql.ExplainStmt); !isExplain {
			c.w.noData()
			return
		}
	}
	c.w.noData()
}

func (c *conn) handleExecute(payload []byte) {
	portal, _, ok := readCString(payload)
	if !ok {
		c.reportError(fmt.Errorf("malformed Execute message"))
		return
	}
	p, found := c.portals[portal]
	if !found {
		c.reportError(fmt.Errorf("portal %q does not exist", portal))
		return
	}
	if p.stmt == nil {
		c.w.emptyQueryResponse()
		return
	}
	c.runStatement(p.stmt)
}
