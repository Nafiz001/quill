// Package exec evaluates expressions and runs query plans.
//
// The execution model is the volcano, or iterator, model: every operator is a
// thing you call Next on, and it pulls from the operator below it. A query is
// a tree of them, and rows flow up one at a time.
//
// The reason it is worth using is memory. A filter over a scan never
// materialises the scan -- it asks for one row, tests it, and either passes it
// on or asks for another -- so a query over a table larger than RAM costs a
// constant amount of memory. The operators that genuinely cannot stream, sort
// and hash-aggregate, are exactly the ones that have to buffer, and they are
// the ones that will need spilling to disk if the input outgrows memory.
package exec

import (
	"fmt"
	"strings"

	"github.com/Nafiz001/quill/internal/sql"
	"github.com/Nafiz001/quill/internal/types"
)

// Row is one tuple flowing through a plan.
type Row []types.Value

// Schema names the columns a row carries, so expressions can be resolved to
// positions once rather than by name on every row.
type Schema struct {
	Columns []ColumnInfo
}

// ColumnInfo describes one output column.
type ColumnInfo struct {
	// Table is the table or alias the column came from, empty for computed
	// expressions.
	Table string
	Name  string
	Type  types.Type
}

// Index finds a column by optionally-qualified name.
//
// An unqualified name that matches columns in two joined tables is ambiguous
// and is reported as such, rather than silently picking one -- that is a class
// of bug that produces plausible wrong answers.
func (s *Schema) Index(table, name string) (int, error) {
	found := -1
	for i, col := range s.Columns {
		if !strings.EqualFold(col.Name, name) {
			continue
		}
		if table != "" && !strings.EqualFold(col.Table, table) {
			continue
		}
		if found >= 0 {
			if table == "" {
				return -1, fmt.Errorf("column reference %q is ambiguous", name)
			}
			continue
		}
		found = i
	}
	if found < 0 {
		if table != "" {
			return -1, fmt.Errorf("column %s.%s does not exist", table, name)
		}
		return -1, fmt.Errorf("column %q does not exist", name)
	}
	return found, nil
}

// Names returns the output column names, for result headers.
func (s *Schema) Names() []string {
	out := make([]string, len(s.Columns))
	for i, c := range s.Columns {
		out[i] = c.Name
	}
	return out
}

// Evaluator turns an expression into a value for a given row.
//
// Column references are resolved against the schema once, when the evaluator
// is built, rather than looked up by name per row. On a million-row scan that
// is the difference between a million map lookups and none.
type Evaluator struct {
	expr   sql.Expression
	schema *Schema
	// resolved caches column positions, keyed by the ColumnRef node.
	resolved map[*sql.ColumnRef]int
}

// NewEvaluator builds an evaluator, resolving every column reference now so a
// bad name is an error before any rows are read.
func NewEvaluator(expr sql.Expression, schema *Schema) (*Evaluator, error) {
	e := &Evaluator{expr: expr, schema: schema, resolved: make(map[*sql.ColumnRef]int)}
	if err := e.resolve(expr); err != nil {
		return nil, err
	}
	return e, nil
}

func (e *Evaluator) resolve(expr sql.Expression) error {
	switch node := expr.(type) {
	case *sql.ColumnRef:
		idx, err := e.schema.Index(node.Table, node.Column)
		if err != nil {
			return err
		}
		e.resolved[node] = idx
	case *sql.BinaryOp:
		if err := e.resolve(node.Left); err != nil {
			return err
		}
		return e.resolve(node.Right)
	case *sql.UnaryOp:
		return e.resolve(node.Expr)
	case *sql.IsNull:
		return e.resolve(node.Expr)
	case *sql.InList:
		if err := e.resolve(node.Expr); err != nil {
			return err
		}
		for _, v := range node.Values {
			if err := e.resolve(v); err != nil {
				return err
			}
		}
	case *sql.Between:
		for _, sub := range []sql.Expression{node.Expr, node.Low, node.High} {
			if err := e.resolve(sub); err != nil {
				return err
			}
		}
	case *sql.FunctionCall:
		for _, a := range node.Args {
			if err := e.resolve(a); err != nil {
				return err
			}
		}
	}
	return nil
}

// Eval computes the expression for one row.
func (e *Evaluator) Eval(row Row) (types.Value, error) {
	return e.eval(e.expr, row)
}

func (e *Evaluator) eval(expr sql.Expression, row Row) (types.Value, error) {
	switch node := expr.(type) {
	case *sql.Literal:
		return node.Value, nil

	case *sql.ColumnRef:
		idx, ok := e.resolved[node]
		if !ok {
			return types.Null(), fmt.Errorf("unresolved column %s", node)
		}
		if idx >= len(row) {
			return types.Null(), fmt.Errorf("row has %d columns, wanted position %d", len(row), idx)
		}
		return row[idx], nil

	case *sql.BinaryOp:
		return e.evalBinary(node, row)

	case *sql.UnaryOp:
		return e.evalUnary(node, row)

	case *sql.IsNull:
		v, err := e.eval(node.Expr, row)
		if err != nil {
			return types.Null(), err
		}
		return types.NewBool(v.IsNull() != node.Negated), nil

	case *sql.InList:
		return e.evalInList(node, row)

	case *sql.Between:
		return e.evalBetween(node, row)

	case *sql.FunctionCall:
		// Aggregates are handled by the aggregate operator, which replaces
		// them before the evaluator ever sees one.
		return types.Null(), fmt.Errorf("%s cannot be used here", node.Name)

	case *sql.Star:
		return types.Null(), fmt.Errorf("* cannot be used in an expression")

	default:
		return types.Null(), fmt.Errorf("unsupported expression %T", expr)
	}
}

func (e *Evaluator) evalBinary(node *sql.BinaryOp, row Row) (types.Value, error) {
	// AND and OR short-circuit, which is not just an optimisation: it is what
	// lets `WHERE x IS NOT NULL AND x > 5` avoid evaluating the comparison on
	// a NULL.
	switch node.Op {
	case "AND":
		left, err := e.eval(node.Left, row)
		if err != nil {
			return types.Null(), err
		}
		if !left.IsNull() && !left.Truthy() {
			return types.NewBool(false), nil
		}
		right, err := e.eval(node.Right, row)
		if err != nil {
			return types.Null(), err
		}
		if left.IsNull() || right.IsNull() {
			// Three-valued logic: unknown AND true is unknown, but
			// unknown AND false is false, which the check above handled.
			if !right.IsNull() && !right.Truthy() {
				return types.NewBool(false), nil
			}
			return types.Null(), nil
		}
		return types.NewBool(right.Truthy()), nil

	case "OR":
		left, err := e.eval(node.Left, row)
		if err != nil {
			return types.Null(), err
		}
		if !left.IsNull() && left.Truthy() {
			return types.NewBool(true), nil
		}
		right, err := e.eval(node.Right, row)
		if err != nil {
			return types.Null(), err
		}
		if !right.IsNull() && right.Truthy() {
			return types.NewBool(true), nil
		}
		if left.IsNull() || right.IsNull() {
			return types.Null(), nil
		}
		return types.NewBool(false), nil
	}

	left, err := e.eval(node.Left, row)
	if err != nil {
		return types.Null(), err
	}
	right, err := e.eval(node.Right, row)
	if err != nil {
		return types.Null(), err
	}

	switch node.Op {
	case "=", "<>", "<", "<=", ">", ">=":
		// Any comparison involving NULL is unknown, never true -- including
		// NULL = NULL. This is the rule IS NULL exists to work around.
		if left.IsNull() || right.IsNull() {
			return types.Null(), nil
		}
		cmp, ok := types.Compare(left, right)
		if !ok {
			return types.Null(), fmt.Errorf("cannot compare %s with %s", left.Type, right.Type)
		}
		switch node.Op {
		case "=":
			return types.NewBool(cmp == 0), nil
		case "<>":
			return types.NewBool(cmp != 0), nil
		case "<":
			return types.NewBool(cmp < 0), nil
		case "<=":
			return types.NewBool(cmp <= 0), nil
		case ">":
			return types.NewBool(cmp > 0), nil
		default:
			return types.NewBool(cmp >= 0), nil
		}

	case "+", "-", "*", "/", "%":
		return arithmetic(node.Op, left, right)

	case "||":
		if left.IsNull() || right.IsNull() {
			return types.Null(), nil
		}
		return types.NewText(left.String() + right.String()), nil

	case "LIKE":
		if left.IsNull() || right.IsNull() {
			return types.Null(), nil
		}
		return types.NewBool(likeMatch(right.String(), left.String())), nil

	default:
		return types.Null(), fmt.Errorf("unsupported operator %q", node.Op)
	}
}

func arithmetic(op string, left, right types.Value) (types.Value, error) {
	if left.IsNull() || right.IsNull() {
		return types.Null(), nil
	}

	// Integer arithmetic stays integer; anything touching a float promotes.
	bothInt := left.Type == types.TypeInt && right.Type == types.TypeInt
	if bothInt {
		a, b := left.Int, right.Int
		switch op {
		case "+":
			return types.NewInt(a + b), nil
		case "-":
			return types.NewInt(a - b), nil
		case "*":
			return types.NewInt(a * b), nil
		case "/":
			if b == 0 {
				return types.Null(), fmt.Errorf("division by zero")
			}
			return types.NewInt(a / b), nil
		case "%":
			if b == 0 {
				return types.Null(), fmt.Errorf("division by zero")
			}
			return types.NewInt(a % b), nil
		}
	}

	lf, err := numericValue(left)
	if err != nil {
		return types.Null(), err
	}
	rf, err := numericValue(right)
	if err != nil {
		return types.Null(), err
	}
	switch op {
	case "+":
		return types.NewFloat(lf + rf), nil
	case "-":
		return types.NewFloat(lf - rf), nil
	case "*":
		return types.NewFloat(lf * rf), nil
	case "/":
		if rf == 0 {
			return types.Null(), fmt.Errorf("division by zero")
		}
		return types.NewFloat(lf / rf), nil
	case "%":
		if rf == 0 {
			return types.Null(), fmt.Errorf("division by zero")
		}
		return types.NewFloat(float64(int64(lf) % int64(rf))), nil
	}
	return types.Null(), fmt.Errorf("unsupported operator %q", op)
}

func numericValue(v types.Value) (float64, error) {
	switch v.Type {
	case types.TypeInt:
		return float64(v.Int), nil
	case types.TypeFloat:
		return v.Float, nil
	default:
		return 0, fmt.Errorf("%s is not a number", v.Type)
	}
}

func (e *Evaluator) evalUnary(node *sql.UnaryOp, row Row) (types.Value, error) {
	v, err := e.eval(node.Expr, row)
	if err != nil {
		return types.Null(), err
	}
	switch node.Op {
	case "NOT":
		if v.IsNull() {
			return types.Null(), nil // NOT unknown is still unknown
		}
		return types.NewBool(!v.Truthy()), nil
	case "-":
		switch v.Type {
		case types.TypeInt:
			return types.NewInt(-v.Int), nil
		case types.TypeFloat:
			return types.NewFloat(-v.Float), nil
		case types.TypeNull:
			return types.Null(), nil
		}
		return types.Null(), fmt.Errorf("cannot negate %s", v.Type)
	default:
		return types.Null(), fmt.Errorf("unsupported unary operator %q", node.Op)
	}
}

func (e *Evaluator) evalInList(node *sql.InList, row Row) (types.Value, error) {
	target, err := e.eval(node.Expr, row)
	if err != nil {
		return types.Null(), err
	}
	if target.IsNull() {
		return types.Null(), nil
	}

	sawNull := false
	for _, candidate := range node.Values {
		v, err := e.eval(candidate, row)
		if err != nil {
			return types.Null(), err
		}
		if v.IsNull() {
			sawNull = true
			continue
		}
		cmp, ok := types.Compare(target, v)
		if ok && cmp == 0 {
			return types.NewBool(!node.Negated), nil
		}
	}
	// `x IN (1, NULL)` where x is 2 is unknown, not false: the NULL might have
	// been a 2. This is the same three-valued logic as everywhere else, and it
	// is the reason NOT IN with a NULL in the list never returns any rows.
	if sawNull {
		return types.Null(), nil
	}
	return types.NewBool(node.Negated), nil
}

func (e *Evaluator) evalBetween(node *sql.Between, row Row) (types.Value, error) {
	target, err := e.eval(node.Expr, row)
	if err != nil {
		return types.Null(), err
	}
	low, err := e.eval(node.Low, row)
	if err != nil {
		return types.Null(), err
	}
	high, err := e.eval(node.High, row)
	if err != nil {
		return types.Null(), err
	}
	if target.IsNull() || low.IsNull() || high.IsNull() {
		return types.Null(), nil
	}

	cmpLow, ok1 := types.Compare(target, low)
	cmpHigh, ok2 := types.Compare(target, high)
	if !ok1 || !ok2 {
		return types.Null(), fmt.Errorf("cannot compare %s in a BETWEEN range", target.Type)
	}
	within := cmpLow >= 0 && cmpHigh <= 0
	return types.NewBool(within != node.Negated), nil
}

// likeMatch implements SQL LIKE: % matches any run of characters, _ matches
// exactly one.
//
// The iterative backtracking matcher is the same shape as a glob matcher, and
// for the same reason: a recursive one is exponential on patterns like
// "%a%a%a%b".
func likeMatch(pattern, s string) bool {
	var (
		si, pi         int
		starPi, starSi int
		haveStar       bool
	)
	starPi = -1

	for si < len(s) {
		if pi < len(pattern) && pattern[pi] == '%' {
			starPi, starSi, haveStar = pi, si, true
			pi++
			continue
		}
		if pi < len(pattern) && (pattern[pi] == '_' || pattern[pi] == s[si]) {
			pi++
			si++
			continue
		}
		if !haveStar {
			return false
		}
		starSi++
		pi = starPi + 1
		si = starSi
	}
	for pi < len(pattern) && pattern[pi] == '%' {
		pi++
	}
	return pi == len(pattern)
}
