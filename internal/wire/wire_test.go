package wire

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/Nafiz001/quill/internal/db"
)

// These tests drive the server the way a real client does: raw protocol bytes
// in, raw protocol bytes out. That is the only way to catch a wrong message
// length or a missing ReadyForQuery, which is exactly the class of bug that
// makes psql hang rather than complain.

func startTestServer(t *testing.T) (*Server, string) {
	t.Helper()

	database, err := db.Open(filepath.Join(t.TempDir(), "wire.db"), db.DefaultOptions())
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	srv := NewServer(database, "127.0.0.1:0", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := srv.Listen(); err != nil {
		t.Fatalf("listen: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = srv.Serve(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("server did not shut down")
		}
	})

	return srv, srv.Addr().String()
}

// client is a minimal protocol client.
type client struct {
	t  *testing.T
	nc net.Conn
}

func dial(t *testing.T, addr string) *client {
	t.Helper()
	nc, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { nc.Close() })
	_ = nc.SetDeadline(time.Now().Add(15 * time.Second))
	return &client{t: t, nc: nc}
}

// startup sends the startup packet and reads until ReadyForQuery.
func (c *client) startup() {
	c.t.Helper()

	var payload []byte
	payload = binary.BigEndian.AppendUint32(payload, protocolVersion)
	for _, kv := range [][2]string{{"user", "tester"}, {"database", "quill"}} {
		payload = append(payload, kv[0]...)
		payload = append(payload, 0)
		payload = append(payload, kv[1]...)
		payload = append(payload, 0)
	}
	payload = append(payload, 0)

	packet := binary.BigEndian.AppendUint32(nil, uint32(len(payload)+4))
	packet = append(packet, payload...)
	if _, err := c.nc.Write(packet); err != nil {
		c.t.Fatalf("write startup: %v", err)
	}

	// Everything up to and including ReadyForQuery.
	for {
		msg := c.read()
		if msg.typ == msgReadyForQuery {
			return
		}
		if msg.typ == msgErrorResponse {
			c.t.Fatalf("startup was rejected: %s", msg.payload)
		}
	}
}

func (c *client) read() message {
	c.t.Helper()
	var header [5]byte
	if _, err := io.ReadFull(c.nc, header[:]); err != nil {
		c.t.Fatalf("read message header: %v", err)
	}
	length := int(binary.BigEndian.Uint32(header[1:]))
	if length < 4 {
		c.t.Fatalf("message claims a length of %d", length)
	}
	payload := make([]byte, length-4)
	if _, err := io.ReadFull(c.nc, payload); err != nil {
		c.t.Fatalf("read message payload: %v", err)
	}
	return message{typ: header[0], payload: payload}
}

// query sends a simple query and collects every message up to ReadyForQuery.
func (c *client) query(sql string) []message {
	c.t.Helper()

	payload := append([]byte(sql), 0)
	packet := []byte{msgQuery}
	packet = binary.BigEndian.AppendUint32(packet, uint32(len(payload)+4))
	packet = append(packet, payload...)
	if _, err := c.nc.Write(packet); err != nil {
		c.t.Fatalf("write query: %v", err)
	}

	var out []message
	for {
		msg := c.read()
		out = append(out, msg)
		if msg.typ == msgReadyForQuery {
			return out
		}
	}
}

// find returns the first message of a type.
func find(msgs []message, typ byte) (message, bool) {
	for _, m := range msgs {
		if m.typ == typ {
			return m, true
		}
	}
	return message{}, false
}

func countOf(msgs []message, typ byte) int {
	n := 0
	for _, m := range msgs {
		if m.typ == typ {
			n++
		}
	}
	return n
}

// dataRowValues decodes a DataRow into its fields. A nil entry is SQL NULL.
func dataRowValues(m message) [][]byte {
	count := int(binary.BigEndian.Uint16(m.payload))
	values := make([][]byte, 0, count)
	pos := 2
	for i := 0; i < count; i++ {
		length := int32(binary.BigEndian.Uint32(m.payload[pos:]))
		pos += 4
		if length == -1 {
			values = append(values, nil)
			continue
		}
		values = append(values, m.payload[pos:pos+int(length)])
		pos += int(length)
	}
	return values
}

// rowDescriptionNames decodes the column names from a RowDescription.
func rowDescriptionNames(m message) []string {
	count := int(binary.BigEndian.Uint16(m.payload))
	names := make([]string, 0, count)
	rest := m.payload[2:]
	for i := 0; i < count; i++ {
		name, remainder, ok := readCString(rest)
		if !ok {
			break
		}
		names = append(names, name)
		// Each field carries 18 bytes of metadata after its name.
		rest = remainder[18:]
	}
	return names
}

func TestStartupHandshake(t *testing.T) {
	_, addr := startTestServer(t)
	c := dial(t, addr)

	var payload []byte
	payload = binary.BigEndian.AppendUint32(payload, protocolVersion)
	payload = append(payload, "user\x00tester\x00\x00"...)
	packet := binary.BigEndian.AppendUint32(nil, uint32(len(payload)+4))
	packet = append(packet, payload...)
	if _, err := c.nc.Write(packet); err != nil {
		t.Fatal(err)
	}

	// The order is fixed by the protocol: authentication, then parameters,
	// then the key data, then ReadyForQuery.
	first := c.read()
	if first.typ != msgAuthentication {
		t.Fatalf("first message was %q, want AuthenticationOK", string(first.typ))
	}
	if code := binary.BigEndian.Uint32(first.payload); code != 0 {
		t.Errorf("authentication code %d, want 0", code)
	}

	sawVersion := false
	for {
		msg := c.read()
		if msg.typ == msgParameterStatus {
			name, _, _ := readCString(msg.payload)
			if name == "server_version" {
				sawVersion = true
			}
		}
		if msg.typ == msgReadyForQuery {
			if msg.payload[0] != statusIdle {
				t.Errorf("transaction status %q, want idle", string(msg.payload[0]))
			}
			break
		}
	}
	if !sawVersion {
		t.Error("the server never reported server_version, which clients read")
	}
}

// A client asks for SSL before anything else; the server must decline with a
// single byte and then accept a normal startup.
func TestSSLRequestIsDeclined(t *testing.T) {
	_, addr := startTestServer(t)
	c := dial(t, addr)

	packet := binary.BigEndian.AppendUint32(nil, 8)
	packet = binary.BigEndian.AppendUint32(packet, sslRequestCode)
	if _, err := c.nc.Write(packet); err != nil {
		t.Fatal(err)
	}

	var reply [1]byte
	if _, err := io.ReadFull(c.nc, reply[:]); err != nil {
		t.Fatalf("read SSL reply: %v", err)
	}
	if reply[0] != 'N' {
		t.Fatalf("SSL reply was %q, want N", string(reply[0]))
	}

	// The connection has to remain usable for a plain startup afterwards.
	c.startup()
	msgs := c.query("SELECT 1")
	if _, ok := find(msgs, msgDataRow); !ok {
		t.Error("the connection was unusable after declining SSL")
	}
}

func TestSimpleQuery(t *testing.T) {
	_, addr := startTestServer(t)
	c := dial(t, addr)
	c.startup()

	c.query(`CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT, score FLOAT, ok BOOLEAN)`)
	c.query(`INSERT INTO t VALUES (1,'first',1.5,true),(2,'second',2.5,false),(3,NULL,NULL,NULL)`)

	msgs := c.query(`SELECT id, name, score, ok FROM t ORDER BY id`)

	desc, ok := find(msgs, msgRowDescription)
	if !ok {
		t.Fatal("no RowDescription was sent")
	}
	if got := rowDescriptionNames(desc); fmt.Sprint(got) != "[id name score ok]" {
		t.Errorf("column names: got %v", got)
	}

	if n := countOf(msgs, msgDataRow); n != 3 {
		t.Fatalf("got %d DataRow messages, want 3", n)
	}

	var dataRows []message
	for _, m := range msgs {
		if m.typ == msgDataRow {
			dataRows = append(dataRows, m)
		}
	}

	first := dataRowValues(dataRows[0])
	if string(first[0]) != "1" || string(first[1]) != "first" || string(first[2]) != "1.5" {
		t.Errorf("first row: %q", first)
	}
	// Booleans go out as t and f, which is what the text format specifies.
	if string(first[3]) != "t" {
		t.Errorf("boolean encoded as %q, want t", first[3])
	}
	second := dataRowValues(dataRows[1])
	if string(second[3]) != "f" {
		t.Errorf("false encoded as %q, want f", second[3])
	}

	// NULL is length -1, which decodes to nil -- distinct from an empty string.
	third := dataRowValues(dataRows[2])
	for i := 1; i < 4; i++ {
		if third[i] != nil {
			t.Errorf("NULL column %d came back as %q, not NULL", i, third[i])
		}
	}

	complete, ok := find(msgs, msgCommandComplete)
	if !ok {
		t.Fatal("no CommandComplete was sent")
	}
	if tag, _, _ := readCString(complete.payload); tag != "SELECT 3" {
		t.Errorf("command tag %q, want SELECT 3", tag)
	}
}

func TestCommandTags(t *testing.T) {
	_, addr := startTestServer(t)
	c := dial(t, addr)
	c.startup()

	tests := []struct{ query, want string }{
		{`CREATE TABLE t (a INTEGER)`, "CREATE TABLE"},
		{`INSERT INTO t VALUES (1)`, "INSERT 0 1"},
		{`INSERT INTO t VALUES (2),(3)`, "INSERT 0 2"},
		{`UPDATE t SET a = 9 WHERE a = 1`, "UPDATE 1"},
		{`DELETE FROM t WHERE a = 9`, "DELETE 1"},
		{`SELECT a FROM t`, "SELECT 2"},
		{`DROP TABLE t`, "DROP TABLE"},
	}
	for _, tc := range tests {
		msgs := c.query(tc.query)
		complete, ok := find(msgs, msgCommandComplete)
		if !ok {
			t.Errorf("%s: no CommandComplete", tc.query)
			continue
		}
		if tag, _, _ := readCString(complete.payload); tag != tc.want {
			t.Errorf("%s: tag %q, want %q", tc.query, tag, tc.want)
		}
	}
}

func TestErrorResponse(t *testing.T) {
	_, addr := startTestServer(t)
	c := dial(t, addr)
	c.startup()

	tests := []struct {
		query    string
		wantCode string
	}{
		{`SELECT * FROM nosuchtable`, "42P01"},
		{`SELCT 1`, "42601"},
	}
	for _, tc := range tests {
		msgs := c.query(tc.query)
		errMsg, ok := find(msgs, msgErrorResponse)
		if !ok {
			t.Errorf("%s: no ErrorResponse", tc.query)
			continue
		}
		// The payload is tagged fields; C is the SQLSTATE code.
		fields := parseErrorFields(errMsg.payload)
		if fields['C'] != tc.wantCode {
			t.Errorf("%s: SQLSTATE %q, want %q", tc.query, fields['C'], tc.wantCode)
		}
		if fields['M'] == "" {
			t.Errorf("%s: the error carried no message", tc.query)
		}
		if fields['S'] != "ERROR" {
			t.Errorf("%s: severity %q", tc.query, fields['S'])
		}
	}

	// A ReadyForQuery must still arrive after an error, or the client hangs.
	msgs := c.query(`SELECT * FROM nosuchtable`)
	if _, ok := find(msgs, msgReadyForQuery); !ok {
		t.Error("no ReadyForQuery after an error: a real client would hang here")
	}
}

func parseErrorFields(payload []byte) map[byte]string {
	fields := make(map[byte]string)
	rest := payload
	for len(rest) > 1 {
		tag := rest[0]
		value, remainder, ok := readCString(rest[1:])
		if !ok {
			break
		}
		fields[tag] = value
		rest = remainder
	}
	return fields
}

// The transaction status in ReadyForQuery is what psql puts in its prompt, so
// it has to track reality.
func TestTransactionStatusReporting(t *testing.T) {
	_, addr := startTestServer(t)
	c := dial(t, addr)
	c.startup()

	c.query(`CREATE TABLE t (a INTEGER)`)

	status := func(msgs []message) byte {
		ready, ok := find(msgs, msgReadyForQuery)
		if !ok {
			t.Fatal("no ReadyForQuery")
		}
		return ready.payload[0]
	}

	if got := status(c.query(`SELECT 1`)); got != statusIdle {
		t.Errorf("outside a transaction: %q, want I", string(got))
	}
	if got := status(c.query(`BEGIN`)); got != statusInTxn {
		t.Errorf("after BEGIN: %q, want T", string(got))
	}
	if got := status(c.query(`INSERT INTO t VALUES (1)`)); got != statusInTxn {
		t.Errorf("inside a transaction: %q, want T", string(got))
	}
	// A failed statement inside a transaction puts it in the failed state,
	// where only ROLLBACK is accepted.
	if got := status(c.query(`SELECT * FROM nosuchtable`)); got != statusFailed {
		t.Errorf("after an error inside a transaction: %q, want E", string(got))
	}
	msgs := c.query(`SELECT 1`)
	if _, ok := find(msgs, msgErrorResponse); !ok {
		t.Error("a statement in a failed transaction should be refused")
	}
	if got := status(c.query(`ROLLBACK`)); got != statusIdle {
		t.Errorf("after ROLLBACK: %q, want I", string(got))
	}

	// The insert was rolled back with the transaction.
	msgs = c.query(`SELECT COUNT(*) FROM t`)
	var rows []message
	for _, m := range msgs {
		if m.typ == msgDataRow {
			rows = append(rows, m)
		}
	}
	if len(rows) != 1 || string(dataRowValues(rows[0])[0]) != "0" {
		t.Error("the failed transaction was not rolled back")
	}
}

// Several statements in one query string run in sequence.
func TestMultiStatementQuery(t *testing.T) {
	_, addr := startTestServer(t)
	c := dial(t, addr)
	c.startup()

	msgs := c.query(`CREATE TABLE t (a INTEGER); INSERT INTO t VALUES (1); SELECT a FROM t;`)
	if n := countOf(msgs, msgCommandComplete); n != 3 {
		t.Errorf("got %d CommandComplete messages, want 3", n)
	}
	if n := countOf(msgs, msgDataRow); n != 1 {
		t.Errorf("got %d rows, want 1", n)
	}
	// Only one ReadyForQuery ends the whole batch.
	if n := countOf(msgs, msgReadyForQuery); n != 1 {
		t.Errorf("got %d ReadyForQuery messages, want 1", n)
	}
}

func TestEmptyQuery(t *testing.T) {
	_, addr := startTestServer(t)
	c := dial(t, addr)
	c.startup()

	msgs := c.query("")
	if _, ok := find(msgs, msgEmptyQuery); !ok {
		t.Error("an empty query should get EmptyQueryResponse")
	}
	if _, ok := find(msgs, msgReadyForQuery); !ok {
		t.Error("no ReadyForQuery after an empty query")
	}
}

// The extended query protocol is what drivers use, as opposed to psql's
// simple queries.
func TestExtendedQueryProtocol(t *testing.T) {
	_, addr := startTestServer(t)
	c := dial(t, addr)
	c.startup()
	c.query(`CREATE TABLE t (a INTEGER)`)
	c.query(`INSERT INTO t VALUES (1),(2)`)

	send := func(typ byte, payload []byte) {
		packet := []byte{typ}
		packet = binary.BigEndian.AppendUint32(packet, uint32(len(payload)+4))
		packet = append(packet, payload...)
		if _, err := c.nc.Write(packet); err != nil {
			t.Fatalf("write %q: %v", string(typ), err)
		}
	}

	// Parse an unnamed statement, bind it to an unnamed portal, execute, sync.
	var parse []byte
	parse = append(parse, 0)                        // statement name: unnamed
	parse = append(parse, "SELECT a FROM t"...)     // query
	parse = append(parse, 0)                        //
	parse = binary.BigEndian.AppendUint16(parse, 0) // no parameter types
	send(msgParse, parse)

	var bind []byte
	bind = append(bind, 0)                        // portal name
	bind = append(bind, 0)                        // statement name
	bind = binary.BigEndian.AppendUint16(bind, 0) // format codes
	bind = binary.BigEndian.AppendUint16(bind, 0) // parameter values
	bind = binary.BigEndian.AppendUint16(bind, 0) // result formats
	send(msgBind, bind)

	var execute []byte
	execute = append(execute, 0)                        // portal name
	execute = binary.BigEndian.AppendUint32(execute, 0) // no row limit
	send(msgExecute, execute)
	send(msgSync, nil)

	var msgs []message
	for {
		msg := c.read()
		msgs = append(msgs, msg)
		if msg.typ == msgReadyForQuery {
			break
		}
	}

	if _, ok := find(msgs, msgParseComplete); !ok {
		t.Error("no ParseComplete")
	}
	if _, ok := find(msgs, msgBindComplete); !ok {
		t.Error("no BindComplete")
	}
	if n := countOf(msgs, msgDataRow); n != 2 {
		t.Errorf("got %d rows, want 2", n)
	}
}

// Every connection gets its own session, so a transaction on one must not be
// visible to another.
func TestConnectionsAreIndependent(t *testing.T) {
	_, addr := startTestServer(t)

	a := dial(t, addr)
	a.startup()
	b := dial(t, addr)
	b.startup()

	a.query(`CREATE TABLE t (x INTEGER)`)

	// b starts a transaction; a is unaffected by it.
	b.query(`BEGIN`)
	msgs := a.query(`SELECT 1`)
	ready, _ := find(msgs, msgReadyForQuery)
	if ready.payload[0] != statusIdle {
		t.Errorf("connection a reports %q while b is in a transaction",
			string(ready.payload[0]))
	}
	b.query(`ROLLBACK`)
}

func TestTerminate(t *testing.T) {
	_, addr := startTestServer(t)
	c := dial(t, addr)
	c.startup()

	packet := []byte{msgTerminate}
	packet = binary.BigEndian.AppendUint32(packet, 4)
	if _, err := c.nc.Write(packet); err != nil {
		t.Fatal(err)
	}

	// The server should close the connection.
	_ = c.nc.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 1)
	if _, err := c.nc.Read(buf); err == nil {
		t.Error("the connection stayed open after Terminate")
	}
}

// A message whose declared length is absurd must be rejected rather than
// making the server allocate.
func TestRejectsAbsurdMessageLength(t *testing.T) {
	_, addr := startTestServer(t)
	c := dial(t, addr)
	c.startup()

	packet := []byte{msgQuery}
	packet = binary.BigEndian.AppendUint32(packet, 0xFFFFFFF)
	packet = append(packet, bytes.Repeat([]byte{'x'}, 16)...)
	_, _ = c.nc.Write(packet)

	_ = c.nc.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 1)
	if _, err := c.nc.Read(buf); err == nil {
		t.Error("an oversized message was accepted")
	}
}
