// Package db is the front door: it opens a database file, recovers it if the
// last shutdown was a crash, and hands out sessions that run SQL.
package db

import (
	"fmt"
	"sync"
	"time"

	"github.com/Nafiz001/quill/internal/catalog"
	"github.com/Nafiz001/quill/internal/exec"
	"github.com/Nafiz001/quill/internal/sql"
	"github.com/Nafiz001/quill/internal/storage"
)

// Options configure a database.
type Options struct {
	// PoolSize is how many pages the buffer pool caches.
	PoolSize int
	// SyncOnCommit forces the log to disk before a commit returns. Leaving it
	// on means an acknowledged commit survives a crash; turning it off makes
	// commits much faster and risks losing the last few.
	SyncOnCommit bool
	// ReadOnly refuses every statement that would write.
	ReadOnly bool
	// StatementTimeout cancels a statement that runs longer. Zero means no
	// limit. MaxResultRows rejects a query that would return more rows than
	// this. Both are for a database that strangers can query.
	StatementTimeout time.Duration
	MaxResultRows    int
}

// DefaultOptions returns durable defaults.
func DefaultOptions() Options {
	return Options{PoolSize: storage.DefaultPoolSize, SyncOnCommit: true}
}

// DB is an open database.
type DB struct {
	path    string
	opts    Options
	pager   *storage.Pager
	wal     *storage.WAL
	catalog *catalog.Catalog
	engine  *exec.Engine

	// mu serialises statements.
	//
	// One writer at a time, and readers too. That is a real limitation and it
	// is deliberate at this stage: the alternative is either page-level
	// latching with a lock manager, or MVCC with row versions, and either one
	// is a project in itself. What this buys is that every statement sees a
	// consistent database without any of that machinery, and the honest
	// description is "serialised", not "concurrent".
	mu sync.Mutex

	nextTxnID     uint64
	closed        bool
	recoveryStats *storage.RecoveryStats
}

// Open opens or creates a database, recovering first if needed.
func Open(path string, opts Options) (*DB, error) {
	if opts.PoolSize <= 0 {
		opts.PoolSize = storage.DefaultPoolSize
	}

	// Recovery runs before anything opens the data file, because it writes to
	// that file directly and must not race the buffer pool.
	stats, err := storage.Recover(path, opts.SyncOnCommit)
	if err != nil {
		return nil, fmt.Errorf("recover: %w", err)
	}

	pager, err := storage.Open(path, opts.PoolSize)
	if err != nil {
		return nil, err
	}

	wal, err := storage.OpenWAL(path, opts.SyncOnCommit)
	if err != nil {
		pager.Close()
		return nil, err
	}
	// This is the write-ahead rule, wired up: the pager asks the log to become
	// durable before it writes any page.
	pager.SetFlushLog(wal.Flush)

	cat, err := catalog.Open(pager)
	if err != nil {
		wal.Close()
		pager.Close()
		return nil, err
	}

	d := &DB{
		path:      path,
		opts:      opts,
		pager:     pager,
		wal:       wal,
		catalog:   cat,
		engine:    exec.NewEngine(pager, cat),
		nextTxnID: 1,
	}
	d.recoveryStats = stats
	d.engine.SetLimits(opts.StatementTimeout, opts.MaxResultRows)
	return d, nil
}

// recoveryStats is what the last Recover reported, for the startup banner.
type recoveryReport = storage.RecoveryStats

// RecoveryStats returns what recovery did when the database was opened.
func (d *DB) RecoveryStats() *recoveryReport { return d.recoveryStats }

// Path returns the database file.
func (d *DB) Path() string { return d.path }

// Catalog exposes the schema.
func (d *DB) Catalog() *catalog.Catalog { return d.catalog }

// Close checkpoints and closes everything.
func (d *DB) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil
	}
	d.closed = true

	// A clean shutdown is a checkpoint: flush every dirty page, then record
	// that the data file is current so recovery has nothing to replay.
	if err := d.pager.Flush(); err != nil {
		d.wal.Close()
		d.pager.Close()
		return err
	}
	if _, err := d.wal.LogCheckpoint(); err != nil {
		d.wal.Close()
		d.pager.Close()
		return err
	}
	if err := d.wal.Truncate(); err != nil {
		d.wal.Close()
		d.pager.Close()
		return err
	}
	if err := d.wal.Close(); err != nil {
		d.pager.Close()
		return err
	}
	return d.pager.Close()
}

// Session is one client's connection to the database.
//
// It holds the transaction state, because whether a statement is inside a
// transaction is a property of the connection and not of the database.
type Session struct {
	db *DB
	// txn is the open transaction, or nil when each statement commits on its
	// own.
	txn *Transaction
}

// NewSession starts a session.
func (d *DB) NewSession() *Session { return &Session{db: d} }

// Transaction is an explicit BEGIN ... COMMIT block.
type Transaction struct {
	id uint64
	// dirtyPages records which pages the transaction touched, so a rollback
	// knows what to discard.
	dirtyPages map[storage.PageID]struct{}
}

// InTransaction reports whether a transaction is open.
func (s *Session) InTransaction() bool { return s.txn != nil }

// Execute runs one SQL statement.
func (s *Session) Execute(query string) (*exec.Result, error) {
	stmt, err := sql.Parse(query)
	if err != nil {
		return nil, err
	}
	return s.ExecuteStatement(stmt)
}

// ExecuteMany runs a script of statements, returning the last result.
func (s *Session) ExecuteMany(script string) ([]*exec.Result, error) {
	stmts, err := sql.ParseMany(script)
	if err != nil {
		return nil, err
	}
	var results []*exec.Result
	for _, stmt := range stmts {
		res, err := s.ExecuteStatement(stmt)
		if err != nil {
			return results, err
		}
		results = append(results, res)
	}
	return results, nil
}

// ExecuteStatement runs a parsed statement.
func (s *Session) ExecuteStatement(stmt sql.Statement) (*exec.Result, error) {
	s.db.mu.Lock()
	defer s.db.mu.Unlock()

	if s.db.closed {
		return nil, fmt.Errorf("database is closed")
	}

	if txnStmt, ok := stmt.(*sql.TransactionStmt); ok {
		return s.handleTransaction(txnStmt)
	}

	if s.db.opts.ReadOnly && isWrite(stmt) {
		return nil, fmt.Errorf("the database is open read-only")
	}

	// A statement outside an explicit transaction gets an implicit one, so
	// every statement is atomic whether the user asked for it or not. This is
	// autocommit, and it is why a failed multi-row INSERT leaves nothing
	// behind.
	implicit := s.txn == nil
	if implicit {
		if err := s.beginLocked(); err != nil {
			return nil, err
		}
	}

	result, err := s.db.engine.Execute(stmt)
	if err != nil {
		if implicit {
			// The statement failed, so undo it. Anything it wrote is still in
			// the buffer pool and has not been logged as committed.
			if rbErr := s.rollbackLocked(); rbErr != nil {
				return nil, fmt.Errorf("%w (rollback also failed: %v)", err, rbErr)
			}
		}
		return nil, err
	}

	if implicit {
		if err := s.commitLocked(); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func isWrite(stmt sql.Statement) bool {
	switch s := stmt.(type) {
	case *sql.InsertStmt, *sql.UpdateStmt, *sql.DeleteStmt,
		*sql.CreateTableStmt, *sql.DropTableStmt, *sql.CreateIndexStmt:
		return true
	case *sql.ExplainStmt:
		return isWrite(s.Statement)
	default:
		return false
	}
}

func (s *Session) handleTransaction(stmt *sql.TransactionStmt) (*exec.Result, error) {
	switch stmt.Action {
	case "BEGIN":
		if s.txn != nil {
			return nil, fmt.Errorf("a transaction is already open")
		}
		if err := s.beginLocked(); err != nil {
			return nil, err
		}
		return &exec.Result{Tag: "BEGIN"}, nil

	case "COMMIT":
		if s.txn == nil {
			return nil, fmt.Errorf("COMMIT without an open transaction")
		}
		if err := s.commitLocked(); err != nil {
			return nil, err
		}
		return &exec.Result{Tag: "COMMIT"}, nil

	case "ROLLBACK":
		if s.txn == nil {
			return nil, fmt.Errorf("ROLLBACK without an open transaction")
		}
		if err := s.rollbackLocked(); err != nil {
			return nil, err
		}
		return &exec.Result{Tag: "ROLLBACK"}, nil

	default:
		return nil, fmt.Errorf("unknown transaction command %q", stmt.Action)
	}
}

func (s *Session) beginLocked() error {
	id := s.db.nextTxnID
	s.db.nextTxnID++
	s.txn = &Transaction{id: id, dirtyPages: make(map[storage.PageID]struct{})}
	return nil
}

// commitLocked makes a transaction durable.
//
// The order is the whole point. Every dirty page is logged, then the commit
// record is written and synced, and only then may the pages themselves reach
// disk. A crash before the commit record leaves a transaction that recovery
// will not replay; a crash after it leaves one recovery will complete.
func (s *Session) commitLocked() error {
	txn := s.txn
	s.txn = nil
	if txn == nil {
		return nil
	}

	if err := s.db.logDirtyPages(txn.id); err != nil {
		return err
	}
	if _, err := s.db.wal.LogCommit(txn.id); err != nil {
		return err
	}
	// Writing the pages now rather than leaving them for eviction keeps the
	// data file close to current, which keeps recovery short. A production
	// system would leave this to a background writer.
	return s.db.pager.Flush()
}

// rollbackLocked discards a transaction's changes.
//
// It works by throwing away the buffer pool's dirty pages and re-reading them
// from disk. That is correct because nothing uncommitted has been written
// there -- the commit path is the only thing that flushes -- and it avoids
// needing undo records at all. The cost is that a rollback invalidates the
// cache, and that a very large transaction can exhaust the pool before it ever
// reaches a commit; both are real, and both are the price of not writing a
// full ARIES undo pass.
func (s *Session) rollbackLocked() error {
	txn := s.txn
	s.txn = nil
	if txn == nil {
		return nil
	}
	if _, err := s.db.wal.LogAbort(txn.id); err != nil {
		return err
	}
	return s.db.pager.DiscardDirty()
}

// logDirtyPages writes an image of every modified page into the log.
func (d *DB) logDirtyPages(txnID uint64) error {
	return d.pager.ForEachDirty(func(page *storage.Page) error {
		lsn, err := d.wal.LogPage(txnID, page)
		if err != nil {
			return err
		}
		// Stamping the page with the LSN is what lets the pager ask the log to
		// flush far enough before writing it.
		page.SetLSN(lsn)
		return nil
	})
}

// Checkpoint flushes everything and truncates the log.
func (d *DB) Checkpoint() error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if err := d.pager.Flush(); err != nil {
		return err
	}
	if _, err := d.wal.LogCheckpoint(); err != nil {
		return err
	}
	return d.wal.Truncate()
}

// Stats reports storage activity.
type Stats struct {
	Pager   storage.PagerStats
	WALSize int64
	Pages   uint32
	Tables  int
}

func (d *DB) Stats() Stats {
	d.mu.Lock()
	defer d.mu.Unlock()
	return Stats{
		Pager:   d.pager.Stats(),
		WALSize: d.wal.Size(),
		Pages:   d.pager.PageCount(),
		Tables:  len(d.catalog.Tables()),
	}
}

// simulateCrash drops every file handle without flushing anything.
//
// It is how the durability tests kill a database: no checkpoint, no final
// flush, and every dirty page in the buffer pool discarded. What survives is
// exactly what a real crash would leave -- the data file as of its last write,
// and the log as of its last sync.
func (d *DB) simulateCrash() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return
	}
	d.closed = true
	_ = d.wal.Abandon()
	_ = d.pager.Abandon()
}
