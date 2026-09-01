package sql

import (
	"fmt"
	"strings"

	"github.com/nafizahmed/quill/internal/types"
)

// The AST is deliberately close to the surface syntax rather than to any
// execution strategy. Keeping "what the user wrote" separate from "how to run
// it" is what lets the planner rewrite freely -- choosing an index, reordering
// a join -- without the parser having pre-committed to anything.

// Statement is any top-level SQL statement.
type Statement interface {
	stmtNode()
	String() string
}

// Expression is any value-producing expression.
type Expression interface {
	exprNode()
	String() string
}

// --- expressions ---

// Literal is a constant.
type Literal struct{ Value types.Value }

func (*Literal) exprNode() {}
func (l *Literal) String() string {
	if l.Value.Type == types.TypeText {
		return "'" + strings.ReplaceAll(l.Value.Str, "'", "''") + "'"
	}
	return l.Value.String()
}

// ColumnRef names a column, optionally qualified by a table.
type ColumnRef struct {
	Table  string
	Column string
}

func (*ColumnRef) exprNode() {}
func (c *ColumnRef) String() string {
	if c.Table != "" {
		return c.Table + "." + c.Column
	}
	return c.Column
}

// Star is the `*` in SELECT *.
type Star struct{ Table string }

func (*Star) exprNode() {}
func (s *Star) String() string {
	if s.Table != "" {
		return s.Table + ".*"
	}
	return "*"
}

// BinaryOp is an infix operation.
type BinaryOp struct {
	Op          string
	Left, Right Expression
}

func (*BinaryOp) exprNode() {}
func (b *BinaryOp) String() string {
	return fmt.Sprintf("(%s %s %s)", b.Left, b.Op, b.Right)
}

// UnaryOp is a prefix operation: NOT, or negation.
type UnaryOp struct {
	Op   string
	Expr Expression
}

func (*UnaryOp) exprNode() {}
func (u *UnaryOp) String() string {
	return fmt.Sprintf("(%s %s)", u.Op, u.Expr)
}

// IsNull is `expr IS [NOT] NULL`.
//
// It is a distinct node rather than a comparison because NULL does not compare
// equal to anything, including itself -- `x = NULL` is never true, which is the
// single most common surprise in SQL and the reason IS NULL exists.
type IsNull struct {
	Expr    Expression
	Negated bool
}

func (*IsNull) exprNode() {}
func (i *IsNull) String() string {
	if i.Negated {
		return fmt.Sprintf("(%s IS NOT NULL)", i.Expr)
	}
	return fmt.Sprintf("(%s IS NULL)", i.Expr)
}

// InList is `expr IN (a, b, c)`.
type InList struct {
	Expr    Expression
	Values  []Expression
	Negated bool
}

func (*InList) exprNode() {}
func (i *InList) String() string {
	parts := make([]string, len(i.Values))
	for j, v := range i.Values {
		parts[j] = v.String()
	}
	op := "IN"
	if i.Negated {
		op = "NOT IN"
	}
	return fmt.Sprintf("(%s %s (%s))", i.Expr, op, strings.Join(parts, ", "))
}

// Between is `expr BETWEEN low AND high`.
type Between struct {
	Expr, Low, High Expression
	Negated         bool
}

func (*Between) exprNode() {}
func (b *Between) String() string {
	op := "BETWEEN"
	if b.Negated {
		op = "NOT BETWEEN"
	}
	return fmt.Sprintf("(%s %s %s AND %s)", b.Expr, op, b.Low, b.High)
}

// FunctionCall is an aggregate or scalar function.
type FunctionCall struct {
	Name     string
	Args     []Expression
	Distinct bool
	// Star marks COUNT(*), which counts rows rather than non-null values.
	Star bool
}

func (*FunctionCall) exprNode() {}
func (f *FunctionCall) String() string {
	if f.Star {
		return f.Name + "(*)"
	}
	parts := make([]string, len(f.Args))
	for i, a := range f.Args {
		parts[i] = a.String()
	}
	prefix := ""
	if f.Distinct {
		prefix = "DISTINCT "
	}
	return fmt.Sprintf("%s(%s%s)", f.Name, prefix, strings.Join(parts, ", "))
}

// IsAggregate reports whether a function name is one of the aggregates, which
// the planner has to route through a grouping operator rather than evaluate
// row by row.
func (f *FunctionCall) IsAggregate() bool {
	switch strings.ToUpper(f.Name) {
	case "COUNT", "SUM", "AVG", "MIN", "MAX":
		return true
	}
	return false
}

// --- statements ---

// SelectItem is one entry in a SELECT list.
type SelectItem struct {
	Expr  Expression
	Alias string
}

func (s SelectItem) String() string {
	if s.Alias != "" {
		return s.Expr.String() + " AS " + s.Alias
	}
	return s.Expr.String()
}

// JoinType distinguishes the join flavours.
type JoinType uint8

const (
	JoinInner JoinType = iota
	JoinLeft
	JoinCross
)

func (j JoinType) String() string {
	switch j {
	case JoinLeft:
		return "LEFT JOIN"
	case JoinCross:
		return "CROSS JOIN"
	default:
		return "INNER JOIN"
	}
}

// TableRef is a table in a FROM clause, possibly joined to another.
type TableRef struct {
	Name  string
	Alias string
	// Join, when set, makes this reference the left side of a join.
	Join *JoinClause
}

// JoinClause is the right side of a join and its condition.
type JoinClause struct {
	Type  JoinType
	Right *TableRef
	On    Expression
}

// Name returns how a table is referred to in expressions: its alias if it has
// one, otherwise its name.
func (t *TableRef) Ref() string {
	if t.Alias != "" {
		return t.Alias
	}
	return t.Name
}

// OrderByItem is one ORDER BY term.
type OrderByItem struct {
	Expr Expression
	Desc bool
}

// SelectStmt is a query.
type SelectStmt struct {
	Distinct bool
	Items    []SelectItem
	From     *TableRef
	Where    Expression
	GroupBy  []Expression
	Having   Expression
	OrderBy  []OrderByItem
	Limit    Expression
	Offset   Expression
}

func (*SelectStmt) stmtNode() {}
func (s *SelectStmt) String() string {
	var b strings.Builder
	b.WriteString("SELECT ")
	if s.Distinct {
		b.WriteString("DISTINCT ")
	}
	parts := make([]string, len(s.Items))
	for i, item := range s.Items {
		parts[i] = item.String()
	}
	b.WriteString(strings.Join(parts, ", "))
	if s.From != nil {
		b.WriteString(" FROM " + s.From.Name)
		if s.From.Alias != "" {
			b.WriteString(" " + s.From.Alias)
		}
		if j := s.From.Join; j != nil {
			b.WriteString(fmt.Sprintf(" %s %s", j.Type, j.Right.Name))
			if j.On != nil {
				b.WriteString(" ON " + j.On.String())
			}
		}
	}
	if s.Where != nil {
		b.WriteString(" WHERE " + s.Where.String())
	}
	return b.String()
}

// InsertStmt adds rows.
type InsertStmt struct {
	Table string
	// Columns is empty when the statement did not name them, meaning the
	// values are positional across every column.
	Columns []string
	Rows    [][]Expression
}

func (*InsertStmt) stmtNode() {}
func (s *InsertStmt) String() string {
	return fmt.Sprintf("INSERT INTO %s (%d rows)", s.Table, len(s.Rows))
}

// Assignment is one `column = expr` in an UPDATE.
type Assignment struct {
	Column string
	Value  Expression
}

// UpdateStmt modifies rows.
type UpdateStmt struct {
	Table string
	Set   []Assignment
	Where Expression
}

func (*UpdateStmt) stmtNode() {}
func (s *UpdateStmt) String() string {
	return fmt.Sprintf("UPDATE %s SET %d columns", s.Table, len(s.Set))
}

// DeleteStmt removes rows.
type DeleteStmt struct {
	Table string
	Where Expression
}

func (*DeleteStmt) stmtNode()        {}
func (s *DeleteStmt) String() string { return "DELETE FROM " + s.Table }

// ColumnDef is a column in a CREATE TABLE.
type ColumnDef struct {
	Name       string
	Type       types.Type
	NotNull    bool
	PrimaryKey bool
	Unique     bool
}

// CreateTableStmt defines a table.
type CreateTableStmt struct {
	Table       string
	Columns     []ColumnDef
	IfNotExists bool
}

func (*CreateTableStmt) stmtNode() {}
func (s *CreateTableStmt) String() string {
	return fmt.Sprintf("CREATE TABLE %s (%d columns)", s.Table, len(s.Columns))
}

// DropTableStmt removes a table.
type DropTableStmt struct {
	Table    string
	IfExists bool
}

func (*DropTableStmt) stmtNode()        {}
func (s *DropTableStmt) String() string { return "DROP TABLE " + s.Table }

// CreateIndexStmt defines a secondary index.
type CreateIndexStmt struct {
	Name    string
	Table   string
	Columns []string
	Unique  bool
}

func (*CreateIndexStmt) stmtNode() {}
func (s *CreateIndexStmt) String() string {
	return fmt.Sprintf("CREATE INDEX %s ON %s", s.Name, s.Table)
}

// TransactionStmt is BEGIN, COMMIT or ROLLBACK.
type TransactionStmt struct {
	Action string // BEGIN, COMMIT, ROLLBACK
}

func (*TransactionStmt) stmtNode()        {}
func (s *TransactionStmt) String() string { return s.Action }

// ExplainStmt asks for a plan instead of results.
type ExplainStmt struct{ Statement Statement }

func (*ExplainStmt) stmtNode()        {}
func (s *ExplainStmt) String() string { return "EXPLAIN " + s.Statement.String() }
