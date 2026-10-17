package db

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Nafiz001/quill/internal/exec"
)

// These are the tests that matter most: they go in through SQL and come out as
// rows, exercising the parser, planner, executor, catalog, B+tree, buffer pool
// and log together. A bug anywhere below shows up here.

type harness struct {
	t   *testing.T
	dir string
	db  *DB
	s   *Session
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, dir: t.TempDir()}
	h.open()
	return h
}

func (h *harness) open() {
	h.t.Helper()
	db, err := Open(filepath.Join(h.dir, "test.db"), DefaultOptions())
	if err != nil {
		h.t.Fatalf("open: %v", err)
	}
	h.db = db
	h.s = db.NewSession()
	h.t.Cleanup(func() {
		if h.db != nil {
			h.db.Close()
		}
	})
}

// reopen closes and reopens, which is how persistence is actually proven.
func (h *harness) reopen() {
	h.t.Helper()
	if err := h.db.Close(); err != nil {
		h.t.Fatalf("close: %v", err)
	}
	h.db = nil
	h.open()
}

func (h *harness) exec(query string) *exec.Result {
	h.t.Helper()
	res, err := h.s.Execute(query)
	if err != nil {
		h.t.Fatalf("%s: %v", query, err)
	}
	return res
}

func (h *harness) execErr(query string) error {
	h.t.Helper()
	_, err := h.s.Execute(query)
	if err == nil {
		h.t.Fatalf("%s: expected an error, got none", query)
	}
	return err
}

// rows renders a result as "a|b, c|d" for compact comparison.
func rows(res *exec.Result) string {
	lines := make([]string, len(res.Rows))
	for i, row := range res.Rows {
		cells := make([]string, len(row))
		for j, v := range row {
			cells[j] = v.String()
		}
		lines[i] = strings.Join(cells, "|")
	}
	return strings.Join(lines, ", ")
}

func (h *harness) query(q string) string {
	h.t.Helper()
	return rows(h.exec(q))
}

func TestCreateAndInsert(t *testing.T) {
	h := newHarness(t)

	h.exec(`CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT, age INTEGER)`)
	h.exec(`INSERT INTO users (id, name, age) VALUES (1, 'Nafiz', 25)`)
	h.exec(`INSERT INTO users (id, name, age) VALUES (2, 'Rahim', 30), (3, 'Karim', 22)`)

	if got := h.query(`SELECT id, name, age FROM users`); got != "1|Nafiz|25, 2|Rahim|30, 3|Karim|22" {
		t.Errorf("got %s", got)
	}

	// Rows come back in primary key order, because the key is where the row
	// lives in the tree.
	h.exec(`INSERT INTO users (id, name, age) VALUES (0, 'First', 1)`)
	if got := h.query(`SELECT id FROM users`); got != "0, 1, 2, 3" {
		t.Errorf("insertion did not preserve key order: %s", got)
	}
}

func TestSelectStar(t *testing.T) {
	h := newHarness(t)
	h.exec(`CREATE TABLE t (a INTEGER, b TEXT)`)
	h.exec(`INSERT INTO t VALUES (1, 'one'), (2, 'two')`)

	res := h.exec(`SELECT * FROM t`)
	if got := strings.Join(res.Columns, ","); got != "a,b" {
		t.Errorf("columns: got %s, want a,b", got)
	}
	if got := rows(res); got != "1|one, 2|two" {
		t.Errorf("rows: got %s", got)
	}
}

func TestWhereClause(t *testing.T) {
	h := newHarness(t)
	h.exec(`CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT, score INTEGER)`)
	h.exec(`INSERT INTO t VALUES (1,'a',10),(2,'b',20),(3,'c',30),(4,'d',40),(5,'e',NULL)`)

	tests := []struct{ query, want string }{
		{`SELECT id FROM t WHERE score > 20`, "3, 4"},
		{`SELECT id FROM t WHERE score >= 20`, "2, 3, 4"},
		{`SELECT id FROM t WHERE score = 20`, "2"},
		{`SELECT id FROM t WHERE score <> 20`, "1, 3, 4"},
		{`SELECT id FROM t WHERE score < 25 AND score > 5`, "1, 2"},
		{`SELECT id FROM t WHERE score = 10 OR score = 40`, "1, 4"},
		{`SELECT id FROM t WHERE name = 'c'`, "3"},
		{`SELECT id FROM t WHERE score BETWEEN 20 AND 30`, "2, 3"},
		{`SELECT id FROM t WHERE score NOT BETWEEN 20 AND 30`, "1, 4"},
		{`SELECT id FROM t WHERE id IN (1, 3, 99)`, "1, 3"},
		{`SELECT id FROM t WHERE id NOT IN (1, 3)`, "2, 4, 5"},
		{`SELECT id FROM t WHERE name LIKE 'a%'`, "1"},
		{`SELECT id FROM t WHERE name LIKE '_'`, "1, 2, 3, 4, 5"},
		{`SELECT id FROM t WHERE NOT score > 20`, "1, 2"},

		// The NULL rules. A comparison against NULL is unknown, never true,
		// which is why row 5 appears in none of these.
		{`SELECT id FROM t WHERE score IS NULL`, "5"},
		{`SELECT id FROM t WHERE score IS NOT NULL`, "1, 2, 3, 4"},
		{`SELECT id FROM t WHERE score = NULL`, ""},
		{`SELECT id FROM t WHERE score <> NULL`, ""},
	}
	for _, tc := range tests {
		t.Run(tc.query, func(t *testing.T) {
			if got := h.query(tc.query); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestOrderByLimitOffset(t *testing.T) {
	h := newHarness(t)
	h.exec(`CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT, score INTEGER)`)
	h.exec(`INSERT INTO t VALUES (1,'charlie',30),(2,'alpha',10),(3,'bravo',20),(4,'delta',20)`)

	tests := []struct{ query, want string }{
		{`SELECT name FROM t ORDER BY name`, "alpha, bravo, charlie, delta"},
		{`SELECT name FROM t ORDER BY name DESC`, "delta, charlie, bravo, alpha"},
		{`SELECT id FROM t ORDER BY score, id`, "2, 3, 4, 1"},
		{`SELECT id FROM t ORDER BY score DESC, id DESC`, "1, 4, 3, 2"},
		{`SELECT id FROM t ORDER BY id LIMIT 2`, "1, 2"},
		{`SELECT id FROM t ORDER BY id LIMIT 2 OFFSET 1`, "2, 3"},
		{`SELECT id FROM t ORDER BY id OFFSET 3`, "4"},
		{`SELECT id FROM t ORDER BY id LIMIT 0`, ""},
	}
	for _, tc := range tests {
		t.Run(tc.query, func(t *testing.T) {
			if got := h.query(tc.query); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestExpressionsAndAliases(t *testing.T) {
	h := newHarness(t)
	h.exec(`CREATE TABLE t (a INTEGER, b INTEGER, s TEXT)`)
	h.exec(`INSERT INTO t VALUES (10, 3, 'hi')`)

	tests := []struct{ query, want string }{
		{`SELECT a + b FROM t`, "13"},
		{`SELECT a - b FROM t`, "7"},
		{`SELECT a * b FROM t`, "30"},
		{`SELECT a / b FROM t`, "3"},
		{`SELECT a % b FROM t`, "1"},
		{`SELECT a * 1.5 FROM t`, "15"},
		{`SELECT -a FROM t`, "-10"},
		{`SELECT (a + b) * 2 FROM t`, "26"},
		{`SELECT a > b FROM t`, "true"},
		{`SELECT s || '!' FROM t`, "hi!"},
		{`SELECT 1 + 1`, "2"},
		{`SELECT 'literal'`, "literal"},
	}
	for _, tc := range tests {
		t.Run(tc.query, func(t *testing.T) {
			if got := h.query(tc.query); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}

	res := h.exec(`SELECT a + b AS total, s AS greeting FROM t`)
	if got := strings.Join(res.Columns, ","); got != "total,greeting" {
		t.Errorf("aliases: got %s", got)
	}

	// Precedence: multiplication binds tighter than addition, AND tighter
	// than OR.
	if got := h.query(`SELECT 2 + 3 * 4`); got != "14" {
		t.Errorf("precedence: got %s, want 14", got)
	}
	if got := h.query(`SELECT true OR false AND false`); got != "true" {
		t.Errorf("boolean precedence: got %s, want true", got)
	}
}

func TestAggregates(t *testing.T) {
	h := newHarness(t)
	h.exec(`CREATE TABLE sales (id INTEGER PRIMARY KEY, region TEXT, amount INTEGER)`)
	h.exec(`INSERT INTO sales VALUES
		(1,'north',100),(2,'north',200),(3,'south',50),
		(4,'south',150),(5,'east',NULL)`)

	tests := []struct{ query, want string }{
		{`SELECT COUNT(*) FROM sales`, "5"},
		{`SELECT COUNT(amount) FROM sales`, "4"}, // NULL is not counted
		{`SELECT SUM(amount) FROM sales`, "500"},
		{`SELECT AVG(amount) FROM sales`, "125"},
		{`SELECT MIN(amount) FROM sales`, "50"},
		{`SELECT MAX(amount) FROM sales`, "200"},
		{`SELECT COUNT(DISTINCT region) FROM sales`, "3"},
	}
	for _, tc := range tests {
		t.Run(tc.query, func(t *testing.T) {
			if got := h.query(tc.query); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}

	// An aggregate over no rows: COUNT is 0, but SUM is NULL.
	h.exec(`CREATE TABLE empty (x INTEGER)`)
	if got := h.query(`SELECT COUNT(*) FROM empty`); got != "0" {
		t.Errorf("COUNT over empty: got %s, want 0", got)
	}
	if got := h.query(`SELECT SUM(x) FROM empty`); got != "NULL" {
		t.Errorf("SUM over empty: got %s, want NULL", got)
	}
}

func TestGroupByAndHaving(t *testing.T) {
	h := newHarness(t)
	h.exec(`CREATE TABLE sales (id INTEGER PRIMARY KEY, region TEXT, amount INTEGER)`)
	h.exec(`INSERT INTO sales VALUES
		(1,'north',100),(2,'north',200),(3,'south',50),
		(4,'south',150),(5,'east',75)`)

	if got := h.query(`SELECT region, SUM(amount) FROM sales GROUP BY region ORDER BY region`); got !=
		"east|75, north|300, south|200" {
		t.Errorf("GROUP BY: got %s", got)
	}
	if got := h.query(`SELECT region, COUNT(*) FROM sales GROUP BY region ORDER BY region`); got !=
		"east|1, north|2, south|2" {
		t.Errorf("GROUP BY COUNT: got %s", got)
	}
	if got := h.query(
		`SELECT region, SUM(amount) FROM sales GROUP BY region HAVING SUM(amount) > 100 ORDER BY region`); got !=
		"north|300, south|200" {
		t.Errorf("HAVING: got %s", got)
	}
}

func TestDistinct(t *testing.T) {
	h := newHarness(t)
	h.exec(`CREATE TABLE t (a INTEGER, b TEXT)`)
	h.exec(`INSERT INTO t VALUES (1,'x'),(1,'x'),(2,'y'),(1,'z')`)

	if got := h.query(`SELECT DISTINCT a FROM t ORDER BY a`); got != "1, 2" {
		t.Errorf("DISTINCT: got %s", got)
	}
	if got := h.query(`SELECT DISTINCT a, b FROM t ORDER BY a, b`); got != "1|x, 1|z, 2|y" {
		t.Errorf("DISTINCT on two columns: got %s", got)
	}
}

func TestUpdateAndDelete(t *testing.T) {
	h := newHarness(t)
	h.exec(`CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT, score INTEGER)`)
	h.exec(`INSERT INTO t VALUES (1,'a',10),(2,'b',20),(3,'c',30)`)

	res := h.exec(`UPDATE t SET score = 99 WHERE id = 2`)
	if res.RowsAffected != 1 {
		t.Errorf("UPDATE affected %d rows, want 1", res.RowsAffected)
	}
	if got := h.query(`SELECT id, score FROM t`); got != "1|10, 2|99, 3|30" {
		t.Errorf("after UPDATE: %s", got)
	}

	// An UPDATE with an expression referencing the row's own value.
	h.exec(`UPDATE t SET score = score * 2 WHERE score < 50`)
	if got := h.query(`SELECT id, score FROM t`); got != "1|20, 2|99, 3|60" {
		t.Errorf("after computed UPDATE: %s", got)
	}

	// No WHERE means every row.
	h.exec(`UPDATE t SET name = 'x'`)
	if got := h.query(`SELECT DISTINCT name FROM t`); got != "x" {
		t.Errorf("unqualified UPDATE: %s", got)
	}

	res = h.exec(`DELETE FROM t WHERE id = 1`)
	if res.RowsAffected != 1 {
		t.Errorf("DELETE affected %d rows, want 1", res.RowsAffected)
	}
	if got := h.query(`SELECT id FROM t`); got != "2, 3" {
		t.Errorf("after DELETE: %s", got)
	}

	h.exec(`DELETE FROM t`)
	if got := h.query(`SELECT COUNT(*) FROM t`); got != "0" {
		t.Errorf("after unqualified DELETE: %s", got)
	}
}

// Changing a primary key moves the row, because the key is where it lives.
func TestUpdatePrimaryKey(t *testing.T) {
	h := newHarness(t)
	h.exec(`CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT)`)
	h.exec(`INSERT INTO t VALUES (1,'a'),(2,'b')`)

	h.exec(`UPDATE t SET id = 10 WHERE id = 1`)
	if got := h.query(`SELECT id, name FROM t`); got != "2|b, 10|a" {
		t.Errorf("after moving a primary key: %s", got)
	}
	if err := h.execErr(`UPDATE t SET id = 2 WHERE id = 10`); !strings.Contains(err.Error(), "duplicate key") {
		t.Errorf("colliding key update: %v", err)
	}
}

func TestJoins(t *testing.T) {
	h := newHarness(t)
	h.exec(`CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT)`)
	h.exec(`CREATE TABLE orders (id INTEGER PRIMARY KEY, user_id INTEGER, total INTEGER)`)
	h.exec(`INSERT INTO users VALUES (1,'Nafiz'),(2,'Rahim'),(3,'Karim')`)
	h.exec(`INSERT INTO orders VALUES (10,1,100),(11,1,200),(12,2,50)`)

	if got := h.query(
		`SELECT users.name, orders.total FROM users JOIN orders ON users.id = orders.user_id ORDER BY orders.id`); got !=
		"Nafiz|100, Nafiz|200, Rahim|50" {
		t.Errorf("INNER JOIN: got %s", got)
	}

	// A LEFT JOIN keeps the unmatched left row, padded with nulls -- that is
	// the entire difference from an inner join.
	if got := h.query(
		`SELECT users.name, orders.total FROM users LEFT JOIN orders ON users.id = orders.user_id ORDER BY users.id, orders.id`); got !=
		"Nafiz|100, Nafiz|200, Rahim|50, Karim|NULL" {
		t.Errorf("LEFT JOIN: got %s", got)
	}

	// Aliases.
	if got := h.query(
		`SELECT u.name, o.total FROM users u JOIN orders o ON u.id = o.user_id ORDER BY o.id`); got !=
		"Nafiz|100, Nafiz|200, Rahim|50" {
		t.Errorf("aliased join: got %s", got)
	}

	// Aggregating across a join.
	if got := h.query(
		`SELECT u.name, SUM(o.total) FROM users u JOIN orders o ON u.id = o.user_id GROUP BY u.name ORDER BY u.name`); got !=
		"Nafiz|300, Rahim|50" {
		t.Errorf("grouped join: got %s", got)
	}
}

func TestConstraints(t *testing.T) {
	h := newHarness(t)
	h.exec(`CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT NOT NULL, note TEXT)`)
	h.exec(`INSERT INTO t VALUES (1,'a','x')`)

	if err := h.execErr(`INSERT INTO t VALUES (1,'b','y')`); !strings.Contains(err.Error(), "duplicate key") {
		t.Errorf("duplicate primary key: %v", err)
	}
	if err := h.execErr(`INSERT INTO t VALUES (2,NULL,'y')`); !strings.Contains(err.Error(), "cannot be NULL") {
		t.Errorf("NOT NULL violation: %v", err)
	}
	if err := h.execErr(`INSERT INTO t (id, note) VALUES (3,'y')`); !strings.Contains(err.Error(), "cannot be NULL") {
		t.Errorf("omitted NOT NULL column: %v", err)
	}
	// A nullable column may be omitted.
	h.exec(`INSERT INTO t (id, name) VALUES (4,'d')`)
	if got := h.query(`SELECT id, note FROM t WHERE id = 4`); got != "4|NULL" {
		t.Errorf("omitted nullable column: %s", got)
	}

	// The failed inserts must have left nothing behind.
	if got := h.query(`SELECT COUNT(*) FROM t`); got != "2" {
		t.Errorf("failed inserts were not rolled back: %s rows", got)
	}
}

func TestTypeCoercionAndErrors(t *testing.T) {
	h := newHarness(t)
	h.exec(`CREATE TABLE t (i INTEGER, f FLOAT, s TEXT, b BOOLEAN)`)

	h.exec(`INSERT INTO t VALUES (1, 1.5, 'hello', true)`)
	if got := h.query(`SELECT i, f, s, b FROM t`); got != "1|1.5|hello|true" {
		t.Errorf("round trip: %s", got)
	}

	// An integer literal into a FLOAT column widens silently.
	h.exec(`INSERT INTO t (f) VALUES (3)`)
	if got := h.query(`SELECT f FROM t WHERE f = 3`); got != "3" {
		t.Errorf("int into float: %s", got)
	}
	// A fractional literal into an INTEGER column does not, because that would
	// be silent data loss.
	if err := h.execErr(`INSERT INTO t (i) VALUES (1.7)`); !strings.Contains(err.Error(), "precision") {
		t.Errorf("lossy cast: %v", err)
	}

	if err := h.execErr(`SELECT nosuchcolumn FROM t`); !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("unknown column: %v", err)
	}
	if err := h.execErr(`SELECT * FROM nosuchtable`); !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("unknown table: %v", err)
	}
	if err := h.execErr(`SELECT 1 / 0`); !strings.Contains(err.Error(), "division by zero") {
		t.Errorf("division by zero: %v", err)
	}
	if err := h.execErr(`SELCT 1`); err == nil {
		t.Error("a syntax error was accepted")
	}
}

func TestTransactions(t *testing.T) {
	h := newHarness(t)
	h.exec(`CREATE TABLE t (id INTEGER PRIMARY KEY, v INTEGER)`)
	h.exec(`INSERT INTO t VALUES (1, 100)`)

	// Committed work sticks.
	h.exec(`BEGIN`)
	h.exec(`INSERT INTO t VALUES (2, 200)`)
	h.exec(`UPDATE t SET v = 111 WHERE id = 1`)
	h.exec(`COMMIT`)
	if got := h.query(`SELECT id, v FROM t`); got != "1|111, 2|200" {
		t.Errorf("after COMMIT: %s", got)
	}

	// Rolled-back work vanishes.
	h.exec(`BEGIN`)
	h.exec(`INSERT INTO t VALUES (3, 300)`)
	h.exec(`DELETE FROM t WHERE id = 1`)
	h.exec(`ROLLBACK`)
	if got := h.query(`SELECT id, v FROM t`); got != "1|111, 2|200" {
		t.Errorf("ROLLBACK did not undo the transaction: %s", got)
	}

	if err := h.execErr(`COMMIT`); !strings.Contains(err.Error(), "without an open transaction") {
		t.Errorf("COMMIT outside a transaction: %v", err)
	}
	if err := h.execErr(`ROLLBACK`); !strings.Contains(err.Error(), "without an open transaction") {
		t.Errorf("ROLLBACK outside a transaction: %v", err)
	}
	h.exec(`BEGIN`)
	if err := h.execErr(`BEGIN`); !strings.Contains(err.Error(), "already open") {
		t.Errorf("nested BEGIN: %v", err)
	}
	h.exec(`ROLLBACK`)
}

// The point of putting it on disk.
func TestPersistenceAcrossReopen(t *testing.T) {
	h := newHarness(t)

	h.exec(`CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT, active BOOLEAN)`)
	h.exec(`CREATE TABLE orders (id INTEGER PRIMARY KEY, total FLOAT)`)
	for i := 1; i <= 500; i++ {
		h.exec(fmt.Sprintf(`INSERT INTO users VALUES (%d, 'user%d', true)`, i, i))
	}
	h.exec(`INSERT INTO orders VALUES (1, 99.5)`)

	h.reopen()

	if got := h.query(`SELECT COUNT(*) FROM users`); got != "500" {
		t.Errorf("row count after reopening: %s", got)
	}
	if got := h.query(`SELECT name FROM users WHERE id = 250`); got != "user250" {
		t.Errorf("row lookup after reopening: %s", got)
	}
	if got := h.query(`SELECT total FROM orders`); got != "99.5" {
		t.Errorf("float after reopening: %s", got)
	}
	if got := h.query(`SELECT active FROM users WHERE id = 1`); got != "true" {
		t.Errorf("boolean after reopening: %s", got)
	}

	// The schema survives too, including which column is the primary key.
	tables := h.db.Catalog().Tables()
	if len(tables) != 2 {
		t.Fatalf("catalog holds %d tables after reopening, want 2", len(tables))
	}
	users, _ := h.db.Catalog().Table("users")
	if users.PrimaryKeyIndex() != 0 {
		t.Error("the primary key was lost")
	}
}

func TestIndexes(t *testing.T) {
	h := newHarness(t)
	h.exec(`CREATE TABLE t (id INTEGER PRIMARY KEY, email TEXT, city TEXT)`)
	h.exec(`INSERT INTO t VALUES (1,'a@x.com','dhaka'),(2,'b@x.com','dhaka'),(3,'c@x.com','ctg')`)

	h.exec(`CREATE INDEX idx_city ON t (city)`)
	h.exec(`CREATE UNIQUE INDEX idx_email ON t (email)`)

	// A unique index rejects a duplicate.
	if err := h.execErr(`INSERT INTO t VALUES (4,'a@x.com','dhaka')`); !strings.Contains(err.Error(), "unique") {
		t.Errorf("unique index violation: %v", err)
	}
	// A non-unique one does not.
	h.exec(`INSERT INTO t VALUES (5,'e@x.com','dhaka')`)

	// The index has to be backfilled, or rows written before it would be
	// invisible to it.
	table, _ := h.db.Catalog().Table("t")
	if len(table.Indexes) != 2 {
		t.Fatalf("catalog holds %d indexes, want 2", len(table.Indexes))
	}

	h.reopen()
	table, _ = h.db.Catalog().Table("t")
	if len(table.Indexes) != 2 {
		t.Errorf("indexes did not survive a reopen: %d", len(table.Indexes))
	}
	// And they still enforce uniqueness afterwards.
	if err := h.execErr(`INSERT INTO t VALUES (6,'b@x.com','x')`); !strings.Contains(err.Error(), "unique") {
		t.Errorf("unique index after reopening: %v", err)
	}
}

// The planner's one real decision: an equality on the primary key should
// descend the tree rather than scan the table.
func TestPrimaryKeyLookupUsesIndex(t *testing.T) {
	h := newHarness(t)
	h.exec(`CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)`)
	for i := 1; i <= 200; i++ {
		h.exec(fmt.Sprintf(`INSERT INTO t VALUES (%d, 'v%d')`, i, i))
	}

	plan := h.query(`EXPLAIN SELECT * FROM t WHERE id = 150`)
	if !strings.Contains(plan, "IndexScan") {
		t.Errorf("a primary key lookup did not use the index:\n%s", plan)
	}
	if got := h.query(`SELECT v FROM t WHERE id = 150`); got != "v150" {
		t.Errorf("index lookup returned %s", got)
	}

	// A predicate on a non-key column has to scan.
	plan = h.query(`EXPLAIN SELECT * FROM t WHERE v = 'v150'`)
	if !strings.Contains(plan, "SeqScan") {
		t.Errorf("a non-key predicate should have scanned:\n%s", plan)
	}
}

func TestExplain(t *testing.T) {
	h := newHarness(t)
	h.exec(`CREATE TABLE t (id INTEGER PRIMARY KEY, a INTEGER, b TEXT)`)
	h.exec(`INSERT INTO t VALUES (1, 1, 'x')`)

	plan := h.query(`EXPLAIN SELECT b FROM t WHERE a > 1 ORDER BY b LIMIT 5`)
	for _, want := range []string{"Limit", "Sort", "Project", "Filter", "SeqScan"} {
		if !strings.Contains(plan, want) {
			t.Errorf("plan is missing %s:\n%s", want, plan)
		}
	}
}

func TestDropTable(t *testing.T) {
	h := newHarness(t)
	h.exec(`CREATE TABLE t (a INTEGER)`)
	h.exec(`INSERT INTO t VALUES (1)`)
	h.exec(`DROP TABLE t`)

	if err := h.execErr(`SELECT * FROM t`); !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("querying a dropped table: %v", err)
	}
	if err := h.execErr(`DROP TABLE t`); !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("dropping twice: %v", err)
	}
	h.exec(`DROP TABLE IF EXISTS t`)
	h.exec(`CREATE TABLE IF NOT EXISTS t (a INTEGER)`)
	h.exec(`CREATE TABLE IF NOT EXISTS t (a INTEGER)`)
}

// A table larger than the buffer pool proves the pool actually works.
func TestLargeTableExceedsBufferPool(t *testing.T) {
	dir := t.TempDir()
	// A pool of 16 frames is 64KB, far smaller than the data below.
	opts := DefaultOptions()
	opts.PoolSize = 16
	db, err := Open(filepath.Join(dir, "big.db"), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := db.NewSession()

	if _, err := s.Execute(`CREATE TABLE t (id INTEGER PRIMARY KEY, payload TEXT)`); err != nil {
		t.Fatal(err)
	}
	payload := strings.Repeat("x", 200)
	const n = 2000
	for i := 1; i <= n; i++ {
		if _, err := s.Execute(fmt.Sprintf(`INSERT INTO t VALUES (%d, '%s')`, i, payload)); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}

	res, err := s.Execute(`SELECT COUNT(*) FROM t`)
	if err != nil {
		t.Fatal(err)
	}
	if got := rows(res); got != fmt.Sprint(n) {
		t.Errorf("count: got %s, want %d", got, n)
	}

	// Spot-check rows spread through the table.
	for _, id := range []int{1, 500, 1000, 1999, 2000} {
		res, err := s.Execute(fmt.Sprintf(`SELECT id FROM t WHERE id = %d`, id))
		if err != nil {
			t.Fatal(err)
		}
		if got := rows(res); got != fmt.Sprint(id) {
			t.Errorf("row %d: got %q", id, got)
		}
	}

	stats := db.Stats()
	if stats.Pager.Evictions == 0 {
		t.Error("a table far larger than the pool evicted nothing")
	}
}
