package sql

import (
	"strings"
	"testing"
)

func TestTokenize(t *testing.T) {
	tests := []struct {
		input string
		want  string // "type:literal" joined by spaces
	}{
		{"SELECT", "keyword:SELECT"},
		{"select", "keyword:SELECT"}, // keywords normalise to upper case
		{"users", "identifier:users"},
		{"Users", "identifier:Users"}, // identifiers keep their case
		{"42", "number:42"},
		{"1.5", "number:1.5"},
		{"1e10", "number:1e10"},
		{"'hello'", "string:hello"},
		{"'it''s'", "string:it's"}, // a doubled quote is an escaped one
		{`"quoted ident"`, "identifier:quoted ident"},
		{"<=", "operator:<="},
		{">=", "operator:>="},
		{"<>", "operator:<>"},
		{"!=", "operator:!="},
		{"||", "operator:||"},
		{"<", "operator:<"},
		{"(", "punctuation:("},
		{"a.b", "identifier:a punctuation:. identifier:b"},
		{"-- a comment\nSELECT", "keyword:SELECT"},
		{"/* block */ 1", "number:1"},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			tokens, err := Tokenize(tc.input)
			if err != nil {
				t.Fatalf("tokenize: %v", err)
			}
			var parts []string
			for _, tok := range tokens {
				if tok.Type == TokenEOF {
					break
				}
				parts = append(parts, tok.Type.String()+":"+tok.Literal)
			}
			if got := strings.Join(parts, " "); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTokenizeErrors(t *testing.T) {
	for _, input := range []string{
		"'unterminated",
		`"unterminated`,
		"@",
	} {
		if _, err := Tokenize(input); err == nil {
			t.Errorf("%q was accepted", input)
		}
	}
}

// The parser's output is compared through String(), which renders the tree
// fully parenthesised -- so the test is really checking how it grouped things.
func TestParseExpressionPrecedence(t *testing.T) {
	tests := []struct{ input, want string }{
		{"SELECT 1 + 2 * 3", "((1 + (2 * 3)))"},
		{"SELECT (1 + 2) * 3", "(((1 + 2) * 3))"},
		{"SELECT 1 - 2 - 3", "(((1 - 2) - 3))"}, // left associative
		{"SELECT a = 1 AND b = 2", "(((a = 1) AND (b = 2)))"},
		{"SELECT a OR b AND c", "((a OR (b AND c)))"}, // AND binds tighter
		{"SELECT NOT a AND b", "(((NOT a) AND b))"},
		{"SELECT a < 1 OR a > 5", "(((a < 1) OR (a > 5)))"},
		{"SELECT -1 + 2", "((-1 + 2))"}, // a negated literal folds
	}
	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			stmt, err := Parse(tc.input)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			sel := stmt.(*SelectStmt)
			if got := "(" + sel.Items[0].Expr.String() + ")"; got != tc.want {
				t.Errorf("got %s, want %s", got, tc.want)
			}
		})
	}
}

// BETWEEN's AND is a separator, not the boolean operator. Parsing the bounds
// at full precedence would swallow it and leave BETWEEN incomplete.
func TestParseBetweenDoesNotSwallowAnd(t *testing.T) {
	stmt, err := Parse("SELECT * FROM t WHERE x BETWEEN 1 AND 5 AND y = 2")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	sel := stmt.(*SelectStmt)
	want := "((x BETWEEN 1 AND 5) AND (y = 2))"
	if got := sel.Where.String(); got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestParseSelect(t *testing.T) {
	stmt, err := Parse(`
		SELECT DISTINCT u.name AS who, COUNT(*)
		  FROM users u JOIN orders o ON u.id = o.user_id
		 WHERE u.age > 18
		 GROUP BY u.name
		HAVING COUNT(*) > 1
		 ORDER BY u.name DESC
		 LIMIT 10 OFFSET 5`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	sel := stmt.(*SelectStmt)

	if !sel.Distinct {
		t.Error("DISTINCT was not recorded")
	}
	if len(sel.Items) != 2 {
		t.Fatalf("got %d select items, want 2", len(sel.Items))
	}
	if sel.Items[0].Alias != "who" {
		t.Errorf("alias: got %q", sel.Items[0].Alias)
	}
	call, ok := sel.Items[1].Expr.(*FunctionCall)
	if !ok || !call.Star || call.Name != "COUNT" {
		t.Errorf("second item: got %v", sel.Items[1].Expr)
	}
	if sel.From.Name != "users" || sel.From.Alias != "u" {
		t.Errorf("FROM: %q %q", sel.From.Name, sel.From.Alias)
	}
	if sel.From.Join == nil || sel.From.Join.Right.Name != "orders" {
		t.Error("the join was not parsed")
	}
	if sel.From.Join.On == nil {
		t.Error("the join condition was not parsed")
	}
	if sel.Where == nil || sel.Having == nil {
		t.Error("WHERE or HAVING was dropped")
	}
	if len(sel.GroupBy) != 1 {
		t.Errorf("GROUP BY: got %d expressions", len(sel.GroupBy))
	}
	if len(sel.OrderBy) != 1 || !sel.OrderBy[0].Desc {
		t.Error("ORDER BY DESC was not recorded")
	}
	if sel.Limit == nil || sel.Offset == nil {
		t.Error("LIMIT or OFFSET was dropped")
	}
}

func TestParseJoinTypes(t *testing.T) {
	tests := []struct {
		input string
		want  JoinType
	}{
		{"SELECT * FROM a JOIN b ON a.x = b.x", JoinInner},
		{"SELECT * FROM a INNER JOIN b ON a.x = b.x", JoinInner},
		{"SELECT * FROM a LEFT JOIN b ON a.x = b.x", JoinLeft},
		{"SELECT * FROM a LEFT OUTER JOIN b ON a.x = b.x", JoinLeft},
		{"SELECT * FROM a CROSS JOIN b", JoinCross},
	}
	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			stmt, err := Parse(tc.input)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			sel := stmt.(*SelectStmt)
			if sel.From.Join == nil {
				t.Fatal("no join was parsed")
			}
			if sel.From.Join.Type != tc.want {
				t.Errorf("got %v, want %v", sel.From.Join.Type, tc.want)
			}
		})
	}

	// An inner join without ON is an error, because it would silently become a
	// cross join and return every pairing.
	if _, err := Parse("SELECT * FROM a JOIN b"); err == nil {
		t.Error("a join without ON was accepted")
	}
}

func TestParseInsert(t *testing.T) {
	stmt, err := Parse(`INSERT INTO t (a, b) VALUES (1, 'x'), (2, 'y')`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ins := stmt.(*InsertStmt)
	if ins.Table != "t" {
		t.Errorf("table: %q", ins.Table)
	}
	if len(ins.Columns) != 2 || ins.Columns[0] != "a" {
		t.Errorf("columns: %v", ins.Columns)
	}
	if len(ins.Rows) != 2 || len(ins.Rows[0]) != 2 {
		t.Errorf("rows: %v", ins.Rows)
	}

	// Without a column list, the values are positional.
	stmt, err = Parse(`INSERT INTO t VALUES (1)`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(stmt.(*InsertStmt).Columns) != 0 {
		t.Error("a column list was invented")
	}
}

func TestParseCreateTable(t *testing.T) {
	stmt, err := Parse(`CREATE TABLE t (
		id INTEGER PRIMARY KEY,
		name VARCHAR(255) NOT NULL,
		email TEXT UNIQUE,
		score FLOAT,
		active BOOLEAN
	)`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ct := stmt.(*CreateTableStmt)
	if len(ct.Columns) != 5 {
		t.Fatalf("got %d columns, want 5", len(ct.Columns))
	}

	if !ct.Columns[0].PrimaryKey {
		t.Error("PRIMARY KEY was not recorded")
	}
	// A primary key is implicitly NOT NULL, which is part of what it means.
	if !ct.Columns[0].NotNull {
		t.Error("a primary key should imply NOT NULL")
	}
	if !ct.Columns[1].NotNull {
		t.Error("NOT NULL was not recorded")
	}
	// VARCHAR(255) parses; the length is accepted and ignored.
	if ct.Columns[1].Type.String() != "TEXT" {
		t.Errorf("VARCHAR became %v", ct.Columns[1].Type)
	}
	if !ct.Columns[2].Unique {
		t.Error("UNIQUE was not recorded")
	}
}

// Most SQL keywords are also plausible column names, so a keyword has to be
// accepted wherever an identifier is expected.
func TestKeywordsUsableAsIdentifiers(t *testing.T) {
	for _, query := range []string{
		"SELECT count FROM t",
		"SELECT key FROM t",
		"SELECT name FROM index",
	} {
		if _, err := Parse(query); err != nil {
			t.Errorf("%s: %v", query, err)
		}
	}
}

func TestParseErrors(t *testing.T) {
	for _, query := range []string{
		"SELCT 1",
		"SELECT FROM",
		"SELECT * FROM",
		"INSERT INTO t",
		"INSERT INTO t VALUES",
		"CREATE TABLE t",
		"CREATE TABLE t (a NOSUCHTYPE)",
		"UPDATE t SET",
		"UPDATE t SET a",
		"DELETE",
		"SELECT * FROM t WHERE",
		"SELECT 1 +",
		"SELECT (1",
	} {
		t.Run(query, func(t *testing.T) {
			if _, err := Parse(query); err == nil {
				t.Error("this was accepted")
			}
		})
	}
}

// Errors should say where they happened, so a user can act on them.
func TestParseErrorsCarryPosition(t *testing.T) {
	_, err := Parse("SELECT * FROM t WHERE")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "position") {
		t.Errorf("the error gives no position: %v", err)
	}
}

func TestParseMany(t *testing.T) {
	stmts, err := ParseMany(`
		CREATE TABLE t (a INTEGER);
		INSERT INTO t VALUES (1);
		SELECT a FROM t;
	`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(stmts) != 3 {
		t.Fatalf("got %d statements, want 3", len(stmts))
	}
	if _, ok := stmts[0].(*CreateTableStmt); !ok {
		t.Errorf("first statement is %T", stmts[0])
	}
	if _, ok := stmts[2].(*SelectStmt); !ok {
		t.Errorf("third statement is %T", stmts[2])
	}

	// Trailing semicolons and whitespace should not produce empty statements.
	stmts, err = ParseMany("SELECT 1;;;  ")
	if err != nil {
		t.Fatal(err)
	}
	if len(stmts) != 1 {
		t.Errorf("got %d statements from a single query, want 1", len(stmts))
	}
}

func TestParseTransactionAndExplain(t *testing.T) {
	for _, tc := range []struct{ input, action string }{
		{"BEGIN", "BEGIN"},
		{"BEGIN TRANSACTION", "BEGIN"},
		{"COMMIT", "COMMIT"},
		{"ROLLBACK", "ROLLBACK"},
	} {
		stmt, err := Parse(tc.input)
		if err != nil {
			t.Fatalf("%s: %v", tc.input, err)
		}
		if got := stmt.(*TransactionStmt).Action; got != tc.action {
			t.Errorf("%s: got %q", tc.input, got)
		}
	}

	stmt, err := Parse("EXPLAIN SELECT 1")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := stmt.(*ExplainStmt).Statement.(*SelectStmt); !ok {
		t.Error("EXPLAIN did not wrap a SELECT")
	}
}

func BenchmarkParse(b *testing.B) {
	query := `SELECT u.name, COUNT(*) FROM users u JOIN orders o ON u.id = o.user_id
	          WHERE u.age > 18 AND u.city = 'Dhaka' GROUP BY u.name ORDER BY u.name LIMIT 10`
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Parse(query); err != nil {
			b.Fatal(err)
		}
	}
}
