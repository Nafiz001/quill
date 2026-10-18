package db

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Nafiz001/quill/internal/exec"
)

// A database that strangers can query must not let one query take it down:
// results are built in memory and a join is a nested loop, and statements
// run one at a time.

func openLimited(t *testing.T, timeout time.Duration, maxRows int) *Session {
	t.Helper()
	opts := DefaultOptions()
	opts.StatementTimeout = timeout
	opts.MaxResultRows = maxRows
	db, err := Open(filepath.Join(t.TempDir(), "limits.db"), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s := db.NewSession()
	if _, err := s.Execute(`CREATE TABLE n (id INTEGER PRIMARY KEY, v INTEGER)`); err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	sb.WriteString("INSERT INTO n VALUES ")
	for i := 1; i <= 4000; i++ {
		if i > 1 {
			sb.WriteString(", ")
		}
		fmt.Fprintf(&sb, "(%d, %d)", i, i%7)
	}
	if _, err := s.Execute(sb.String()); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestStatementTimeoutStopsARunawayJoin(t *testing.T) {
	s := openLimited(t, 30*time.Millisecond, 0)

	// 4,000 x 4,000 comparisons, run to completion when no limit is set.
	start := time.Now()
	_, err := s.Execute(`SELECT COUNT(*) FROM n a JOIN n b ON a.v = b.v`)
	if !errors.Is(err, exec.ErrStatementTimeout) {
		t.Fatalf("want a statement timeout, got %v", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("the timeout took %v to fire", took)
	}

	// The session is still usable, and a cheap query still works.
	res, err := s.Execute(`SELECT COUNT(*) FROM n`)
	if err != nil {
		t.Fatalf("session unusable after a timeout: %v", err)
	}
	if got := res.Rows[0][0].String(); got != "4000" {
		t.Errorf("count after timeout = %s", got)
	}
}

func TestMaxResultRowsRefusesAHugeResult(t *testing.T) {
	s := openLimited(t, 0, 500)

	if _, err := s.Execute(`SELECT * FROM n`); !errors.Is(err, exec.ErrResultTooLarge) {
		t.Fatalf("want ErrResultTooLarge, got %v", err)
	}
	// A bounded query is fine, and so is a query under the cap.
	if res, err := s.Execute(`SELECT * FROM n LIMIT 100`); err != nil || len(res.Rows) != 100 {
		t.Fatalf("LIMIT 100: %v", err)
	}
}

func TestNoLimitsByDefault(t *testing.T) {
	s := openLimited(t, 0, 0)
	res, err := s.Execute(`SELECT * FROM n`)
	if err != nil || len(res.Rows) != 4000 {
		t.Fatalf("default options limited a query: %v", err)
	}
}
