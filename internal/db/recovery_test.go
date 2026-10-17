package db

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Nafiz001/quill/internal/exec"
	"github.com/Nafiz001/quill/internal/storage"
)

// Durability is only proven by killing the database, so these tests simulate a
// crash: the process never gets to close cleanly, and the next open has to
// reconstruct what was committed from the log alone.
//
// A crash is simulated by abandoning the DB without calling Close. That leaves
// the data file exactly as the buffer pool last wrote it -- which for anything
// still cached is nothing at all -- and the log holding the truth.

// crashHarness opens a database, does something, then abandons it.
type crashHarness struct {
	t    *testing.T
	dir  string
	path string
}

func newCrashHarness(t *testing.T) *crashHarness {
	dir := t.TempDir()
	return &crashHarness{t: t, dir: dir, path: filepath.Join(dir, "crash.db")}
}

// open returns a fresh database handle over the same file.
func (h *crashHarness) open() (*DB, *Session) {
	h.t.Helper()
	database, err := Open(h.path, DefaultOptions())
	if err != nil {
		h.t.Fatalf("open: %v", err)
	}
	return database, database.NewSession()
}

// openClean opens, and registers a proper Close.
func (h *crashHarness) openClean() (*DB, *Session) {
	database, session := h.open()
	h.t.Cleanup(func() { database.Close() })
	return database, session
}

func TestRecoveryReplaysCommittedWork(t *testing.T) {
	h := newCrashHarness(t)

	// Write some data and commit it, then vanish without closing.
	func() {
		database, s := h.open()
		mustExec(t, s, `CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)`)
		for i := 1; i <= 50; i++ {
			mustExec(t, s, fmt.Sprintf(`INSERT INTO t VALUES (%d, 'value%d')`, i, i))
		}
		database.simulateCrash()
	}()

	// The log should be non-empty, since nothing checkpointed it away.
	info, err := os.Stat(h.path + ".wal")
	if err != nil {
		t.Fatalf("the log is missing after a crash: %v", err)
	}
	if info.Size() == 0 {
		t.Fatal("the log is empty after a crash, so nothing could be recovered")
	}

	database, s := h.openClean()

	stats := database.RecoveryStats()
	if stats == nil || stats.PagesRestored == 0 {
		t.Errorf("recovery restored nothing: %+v", stats)
	}
	t.Logf("recovery: %d records, %d pages restored, %d committed transactions",
		stats.RecordsScanned, stats.PagesRestored, stats.Committed)

	res := mustExec(t, s, `SELECT COUNT(*) FROM t`)
	if got := rows(res); got != "50" {
		t.Errorf("row count after recovery: got %s, want 50", got)
	}
	res = mustExec(t, s, `SELECT v FROM t WHERE id = 25`)
	if got := rows(res); got != "value25" {
		t.Errorf("row lookup after recovery: got %s", got)
	}
}

// An explicit transaction that never committed must leave nothing behind.
func TestRecoveryDiscardsUncommittedWork(t *testing.T) {
	h := newCrashHarness(t)

	func() {
		database, s := h.open()
		mustExec(t, s, `CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)`)
		mustExec(t, s, `INSERT INTO t VALUES (1, 'committed')`)

		// Open a transaction, write, and crash before committing.
		mustExec(t, s, `BEGIN`)
		mustExec(t, s, `INSERT INTO t VALUES (2, 'never committed')`)
		mustExec(t, s, `INSERT INTO t VALUES (3, 'also never committed')`)
		database.simulateCrash()
	}()

	_, s := h.openClean()

	res := mustExec(t, s, `SELECT id, v FROM t`)
	if got := rows(res); got != "1|committed" {
		t.Errorf("uncommitted work survived the crash: %s", got)
	}
}

// A committed transaction survives; an uncommitted one that followed it does
// not. Both halves have to be right at once.
func TestRecoveryDrawsTheLineAtCommit(t *testing.T) {
	h := newCrashHarness(t)

	func() {
		database, s := h.open()
		mustExec(t, s, `CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)`)

		mustExec(t, s, `BEGIN`)
		mustExec(t, s, `INSERT INTO t VALUES (1, 'first')`)
		mustExec(t, s, `INSERT INTO t VALUES (2, 'second')`)
		mustExec(t, s, `COMMIT`)

		mustExec(t, s, `BEGIN`)
		mustExec(t, s, `INSERT INTO t VALUES (3, 'third')`)
		mustExec(t, s, `DELETE FROM t WHERE id = 1`)
		// Crash mid-transaction.
		database.simulateCrash()
	}()

	_, s := h.openClean()

	res := mustExec(t, s, `SELECT id, v FROM t`)
	if got := rows(res); got != "1|first, 2|second" {
		t.Errorf("recovery drew the line in the wrong place: %s", got)
	}
}

// A crash halfway through writing a log record must not stop the database
// starting: everything before the tear is intact.
func TestRecoveryToleratesTornTail(t *testing.T) {
	h := newCrashHarness(t)

	func() {
		database, s := h.open()
		mustExec(t, s, `CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)`)
		for i := 1; i <= 20; i++ {
			mustExec(t, s, fmt.Sprintf(`INSERT INTO t VALUES (%d, 'v%d')`, i, i))
		}
		database.simulateCrash()
	}()

	// Chop the last few bytes, as a power cut mid-write would.
	walPath := h.path + ".wal"
	data, err := os.ReadFile(walPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) < 100 {
		t.Fatalf("the log is only %d bytes; nothing to tear", len(data))
	}
	if err := os.WriteFile(walPath, data[:len(data)-40], 0o644); err != nil {
		t.Fatal(err)
	}

	database, s := h.openClean()
	if stats := database.RecoveryStats(); stats != nil && !stats.TornTail {
		t.Error("recovery did not notice the torn tail")
	}

	// The database has to be usable, and the rows before the tear present.
	res := mustExec(t, s, `SELECT COUNT(*) FROM t`)
	count := rows(res)
	if count == "0" {
		t.Error("a torn tail lost everything, not just the tail")
	}
	t.Logf("%s of 20 rows survived a torn log tail", count)

	// And it must accept new work afterwards.
	mustExec(t, s, `INSERT INTO t VALUES (999, 'after recovery')`)
	res = mustExec(t, s, `SELECT v FROM t WHERE id = 999`)
	if got := rows(res); got != "after recovery" {
		t.Errorf("the database is unusable after recovering a torn log: %s", got)
	}
}

// A corrupted record must be treated as the tear, not applied.
func TestRecoveryRejectsCorruptRecord(t *testing.T) {
	h := newCrashHarness(t)

	func() {
		database, s := h.open()
		mustExec(t, s, `CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)`)
		for i := 1; i <= 10; i++ {
			mustExec(t, s, fmt.Sprintf(`INSERT INTO t VALUES (%d, 'v%d')`, i, i))
		}
		database.simulateCrash()
	}()

	// Flip a byte in the middle. The CRC will not match, so replay must stop
	// there rather than write a corrupted page into the data file.
	walPath := h.path + ".wal"
	data, err := os.ReadFile(walPath)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)/2] ^= 0xFF
	if err := os.WriteFile(walPath, data, 0o644); err != nil {
		t.Fatal(err)
	}

	database, s := h.openClean()
	if stats := database.RecoveryStats(); stats != nil && !stats.TornTail {
		t.Error("a corrupted record was not detected")
	}
	// Whatever survived, the database must open and work.
	if _, err := s.Execute(`SELECT COUNT(*) FROM t`); err != nil {
		t.Errorf("the database is unusable after a corrupted log: %v", err)
	}
}

// A clean shutdown checkpoints, so the next open has nothing to replay. That
// is what keeps startup fast on a healthy database.
func TestCleanShutdownLeavesNothingToRecover(t *testing.T) {
	h := newCrashHarness(t)

	database, s := h.open()
	mustExec(t, s, `CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)`)
	for i := 1; i <= 30; i++ {
		mustExec(t, s, fmt.Sprintf(`INSERT INTO t VALUES (%d, 'v%d')`, i, i))
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(h.path + ".wal")
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 0 {
		t.Errorf("a clean shutdown left %d bytes in the log", info.Size())
	}

	database2, s2 := h.openClean()
	if stats := database2.RecoveryStats(); stats.PagesRestored != 0 {
		t.Errorf("a clean shutdown still needed recovery: %+v", stats)
	}
	res := mustExec(t, s2, `SELECT COUNT(*) FROM t`)
	if got := rows(res); got != "30" {
		t.Errorf("after a clean restart: %s", got)
	}
}

// Recovery has to be idempotent: replaying the same log twice must not corrupt
// anything, which is why page images are logged rather than deltas.
func TestRecoveryIsIdempotent(t *testing.T) {
	h := newCrashHarness(t)

	func() {
		database, s := h.open()
		mustExec(t, s, `CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)`)
		for i := 1; i <= 20; i++ {
			mustExec(t, s, fmt.Sprintf(`INSERT INTO t VALUES (%d, 'v%d')`, i, i))
		}
		database.simulateCrash()
	}()

	// Keep a copy of the log, recover, then put the log back and recover again.
	walPath := h.path + ".wal"
	saved, err := os.ReadFile(walPath)
	if err != nil {
		t.Fatal(err)
	}

	database, _ := h.open()
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(walPath, saved, 0o644); err != nil {
		t.Fatal(err)
	}
	stats, err := storage.Recover(h.path, true)
	if err != nil {
		t.Fatalf("second recovery: %v", err)
	}
	t.Logf("second replay restored %d pages", stats.PagesRestored)

	_, s := h.openClean()
	res := mustExec(t, s, `SELECT COUNT(*) FROM t`)
	if got := rows(res); got != "20" {
		t.Errorf("replaying the log twice changed the data: %s rows, want 20", got)
	}
}

// A statement that fails partway must leave nothing behind, which is what the
// implicit transaction around every statement is for.
func TestFailedStatementRollsBack(t *testing.T) {
	h := newCrashHarness(t)
	_, s := h.openClean()

	mustExec(t, s, `CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT NOT NULL)`)
	mustExec(t, s, `INSERT INTO t VALUES (1, 'one')`)

	// A multi-row insert where a later row violates a constraint.
	_, err := s.Execute(`INSERT INTO t VALUES (2, 'two'), (3, NULL)`)
	if err == nil {
		t.Fatal("the constraint violation was not reported")
	}
	if !strings.Contains(err.Error(), "cannot be NULL") {
		t.Errorf("unexpected error: %v", err)
	}

	// Row 2 must not have survived: the statement was atomic.
	res := mustExec(t, s, `SELECT id FROM t`)
	if got := rows(res); got != "1" {
		t.Errorf("a partially applied INSERT left rows behind: %s", got)
	}
}

func mustExec(t *testing.T, s *Session, query string) *exec.Result {
	t.Helper()
	res, err := s.Execute(query)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return res
}
