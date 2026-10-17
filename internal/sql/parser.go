package sql

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/Nafiz001/quill/internal/types"
)

// Parser turns tokens into a syntax tree by recursive descent.
//
// Each grammar rule is a method, and the call stack does the work a parse table
// would otherwise do. The one place that needs care is expressions, where
// precedence is handled by climbing: each level parses the level below it and
// then loops while it sees an operator of its own precedence, so `a OR b AND c`
// binds as `a OR (b AND c)` without any special-casing.
type Parser struct {
	tokens []Token
	pos    int
}

// NewParser prepares a parser over already-lexed tokens.
func NewParser(tokens []Token) *Parser { return &Parser{tokens: tokens} }

// Parse parses a single statement.
func Parse(input string) (Statement, error) {
	tokens, err := Tokenize(input)
	if err != nil {
		return nil, err
	}
	p := NewParser(tokens)
	stmt, err := p.parseStatement()
	if err != nil {
		return nil, err
	}
	p.skipSemicolons()
	if !p.atEOF() {
		return nil, p.errorf("unexpected %s after the end of the statement", p.peek())
	}
	return stmt, nil
}

// ParseMany parses a semicolon-separated script.
func ParseMany(input string) ([]Statement, error) {
	tokens, err := Tokenize(input)
	if err != nil {
		return nil, err
	}
	p := NewParser(tokens)

	var out []Statement
	for {
		p.skipSemicolons()
		if p.atEOF() {
			return out, nil
		}
		stmt, err := p.parseStatement()
		if err != nil {
			return nil, err
		}
		out = append(out, stmt)
	}
}

// --- token helpers ---

func (p *Parser) peek() Token {
	if p.pos >= len(p.tokens) {
		return Token{Type: TokenEOF}
	}
	return p.tokens[p.pos]
}

func (p *Parser) peekAt(n int) Token {
	if p.pos+n >= len(p.tokens) {
		return Token{Type: TokenEOF}
	}
	return p.tokens[p.pos+n]
}

func (p *Parser) next() Token {
	t := p.peek()
	if p.pos < len(p.tokens) {
		p.pos++
	}
	return t
}

func (p *Parser) atEOF() bool { return p.peek().Type == TokenEOF }

// isKeyword reports whether the next token is the given keyword.
func (p *Parser) isKeyword(kw string) bool {
	t := p.peek()
	return t.Type == TokenKeyword && t.Literal == kw
}

// acceptKeyword consumes the keyword if present.
func (p *Parser) acceptKeyword(kw string) bool {
	if p.isKeyword(kw) {
		p.pos++
		return true
	}
	return false
}

func (p *Parser) expectKeyword(kw string) error {
	if !p.acceptKeyword(kw) {
		return p.errorf("expected %s, found %s", kw, p.peek())
	}
	return nil
}

func (p *Parser) acceptPunct(s string) bool {
	t := p.peek()
	if t.Type == TokenPunct && t.Literal == s {
		p.pos++
		return true
	}
	return false
}

func (p *Parser) expectPunct(s string) error {
	if !p.acceptPunct(s) {
		return p.errorf("expected %q, found %s", s, p.peek())
	}
	return nil
}

func (p *Parser) acceptOperator(s string) bool {
	t := p.peek()
	if t.Type == TokenOperator && t.Literal == s {
		p.pos++
		return true
	}
	return false
}

func (p *Parser) skipSemicolons() {
	for p.acceptPunct(";") {
	}
}

// parseIdent reads a name.
//
// A keyword is accepted here on purpose: `SELECT count FROM t` must work,
// because most SQL keywords are also plausible column names. Reserving them
// globally would be stricter than the standard and more annoying than useful.
func (p *Parser) parseIdent(what string) (string, error) {
	t := p.peek()
	if t.Type == TokenIdent || t.Type == TokenKeyword {
		p.pos++
		return t.Literal, nil
	}
	return "", p.errorf("expected %s, found %s", what, t)
}

func (p *Parser) errorf(format string, args ...any) error {
	pos := p.peek().Pos
	return fmt.Errorf("%s (at position %d)", fmt.Sprintf(format, args...), pos)
}

// --- statements ---

func (p *Parser) parseStatement() (Statement, error) {
	t := p.peek()
	if t.Type != TokenKeyword {
		return nil, p.errorf("expected a statement, found %s", t)
	}

	switch t.Literal {
	case "SELECT":
		return p.parseSelect()
	case "INSERT":
		return p.parseInsert()
	case "UPDATE":
		return p.parseUpdate()
	case "DELETE":
		return p.parseDelete()
	case "CREATE":
		return p.parseCreate()
	case "DROP":
		return p.parseDrop()
	case "BEGIN", "COMMIT", "ROLLBACK":
		p.pos++
		// `BEGIN TRANSACTION` and `BEGIN` mean the same thing.
		p.acceptKeyword("TRANSACTION")
		return &TransactionStmt{Action: t.Literal}, nil
	case "EXPLAIN":
		p.pos++
		inner, err := p.parseStatement()
		if err != nil {
			return nil, err
		}
		return &ExplainStmt{Statement: inner}, nil
	default:
		return nil, p.errorf("unsupported statement %s", t)
	}
}

func (p *Parser) parseSelect() (*SelectStmt, error) {
	if err := p.expectKeyword("SELECT"); err != nil {
		return nil, err
	}
	stmt := &SelectStmt{}
	stmt.Distinct = p.acceptKeyword("DISTINCT")

	for {
		item, err := p.parseSelectItem()
		if err != nil {
			return nil, err
		}
		stmt.Items = append(stmt.Items, item)
		if !p.acceptPunct(",") {
			break
		}
	}

	if p.acceptKeyword("FROM") {
		from, err := p.parseTableRef()
		if err != nil {
			return nil, err
		}
		stmt.From = from
	}

	if p.acceptKeyword("WHERE") {
		expr, err := p.parseExpression()
		if err != nil {
			return nil, err
		}
		stmt.Where = expr
	}

	if p.acceptKeyword("GROUP") {
		if err := p.expectKeyword("BY"); err != nil {
			return nil, err
		}
		for {
			expr, err := p.parseExpression()
			if err != nil {
				return nil, err
			}
			stmt.GroupBy = append(stmt.GroupBy, expr)
			if !p.acceptPunct(",") {
				break
			}
		}
	}

	if p.acceptKeyword("HAVING") {
		expr, err := p.parseExpression()
		if err != nil {
			return nil, err
		}
		stmt.Having = expr
	}

	if p.acceptKeyword("ORDER") {
		if err := p.expectKeyword("BY"); err != nil {
			return nil, err
		}
		for {
			expr, err := p.parseExpression()
			if err != nil {
				return nil, err
			}
			item := OrderByItem{Expr: expr}
			if p.acceptKeyword("DESC") {
				item.Desc = true
			} else {
				p.acceptKeyword("ASC")
			}
			stmt.OrderBy = append(stmt.OrderBy, item)
			if !p.acceptPunct(",") {
				break
			}
		}
	}

	if p.acceptKeyword("LIMIT") {
		expr, err := p.parseExpression()
		if err != nil {
			return nil, err
		}
		stmt.Limit = expr
	}
	if p.acceptKeyword("OFFSET") {
		expr, err := p.parseExpression()
		if err != nil {
			return nil, err
		}
		stmt.Offset = expr
	}

	return stmt, nil
}

func (p *Parser) parseSelectItem() (SelectItem, error) {
	// `*` and `t.*` are handled here rather than in the expression grammar,
	// because they are only legal in a select list.
	if p.acceptOperator("*") {
		return SelectItem{Expr: &Star{}}, nil
	}
	if p.peek().Type == TokenIdent && p.peekAt(1).Literal == "." &&
		p.peekAt(2).Type == TokenOperator && p.peekAt(2).Literal == "*" {
		table := p.next().Literal
		p.next() // .
		p.next() // *
		return SelectItem{Expr: &Star{Table: table}}, nil
	}

	expr, err := p.parseExpression()
	if err != nil {
		return SelectItem{}, err
	}
	item := SelectItem{Expr: expr}

	if p.acceptKeyword("AS") {
		alias, err := p.parseIdent("an alias")
		if err != nil {
			return SelectItem{}, err
		}
		item.Alias = alias
	} else if t := p.peek(); t.Type == TokenIdent {
		// An alias without AS, which SQL allows.
		item.Alias = t.Literal
		p.pos++
	}
	return item, nil
}

func (p *Parser) parseTableRef() (*TableRef, error) {
	name, err := p.parseIdent("a table name")
	if err != nil {
		return nil, err
	}
	ref := &TableRef{Name: name}

	if p.acceptKeyword("AS") {
		alias, err := p.parseIdent("an alias")
		if err != nil {
			return nil, err
		}
		ref.Alias = alias
	} else if t := p.peek(); t.Type == TokenIdent {
		ref.Alias = t.Literal
		p.pos++
	}

	// One join is supported; chained joins are future work.
	joinType, ok := p.parseJoinType()
	if !ok {
		return ref, nil
	}
	right, err := p.parseTableRef()
	if err != nil {
		return nil, err
	}
	join := &JoinClause{Type: joinType, Right: right}
	if p.acceptKeyword("ON") {
		cond, err := p.parseExpression()
		if err != nil {
			return nil, err
		}
		join.On = cond
	} else if joinType != JoinCross {
		return nil, p.errorf("%s requires an ON condition", joinType)
	}
	ref.Join = join
	return ref, nil
}

func (p *Parser) parseJoinType() (JoinType, bool) {
	switch {
	case p.acceptKeyword("JOIN"):
		return JoinInner, true
	case p.acceptKeyword("INNER"):
		p.acceptKeyword("JOIN")
		return JoinInner, true
	case p.acceptKeyword("LEFT"):
		p.acceptKeyword("OUTER")
		p.acceptKeyword("JOIN")
		return JoinLeft, true
	case p.acceptKeyword("CROSS"):
		p.acceptKeyword("JOIN")
		return JoinCross, true
	}
	return JoinInner, false
}

func (p *Parser) parseInsert() (*InsertStmt, error) {
	if err := p.expectKeyword("INSERT"); err != nil {
		return nil, err
	}
	if err := p.expectKeyword("INTO"); err != nil {
		return nil, err
	}
	table, err := p.parseIdent("a table name")
	if err != nil {
		return nil, err
	}
	stmt := &InsertStmt{Table: table}

	// The column list is optional; without it the values are positional.
	if p.acceptPunct("(") {
		for {
			col, err := p.parseIdent("a column name")
			if err != nil {
				return nil, err
			}
			stmt.Columns = append(stmt.Columns, col)
			if !p.acceptPunct(",") {
				break
			}
		}
		if err := p.expectPunct(")"); err != nil {
			return nil, err
		}
	}

	if err := p.expectKeyword("VALUES"); err != nil {
		return nil, err
	}
	for {
		if err := p.expectPunct("("); err != nil {
			return nil, err
		}
		var row []Expression
		for {
			expr, err := p.parseExpression()
			if err != nil {
				return nil, err
			}
			row = append(row, expr)
			if !p.acceptPunct(",") {
				break
			}
		}
		if err := p.expectPunct(")"); err != nil {
			return nil, err
		}
		stmt.Rows = append(stmt.Rows, row)
		if !p.acceptPunct(",") {
			break
		}
	}
	return stmt, nil
}

func (p *Parser) parseUpdate() (*UpdateStmt, error) {
	if err := p.expectKeyword("UPDATE"); err != nil {
		return nil, err
	}
	table, err := p.parseIdent("a table name")
	if err != nil {
		return nil, err
	}
	if err := p.expectKeyword("SET"); err != nil {
		return nil, err
	}
	stmt := &UpdateStmt{Table: table}

	for {
		col, err := p.parseIdent("a column name")
		if err != nil {
			return nil, err
		}
		if !p.acceptOperator("=") {
			return nil, p.errorf("expected = after the column name, found %s", p.peek())
		}
		expr, err := p.parseExpression()
		if err != nil {
			return nil, err
		}
		stmt.Set = append(stmt.Set, Assignment{Column: col, Value: expr})
		if !p.acceptPunct(",") {
			break
		}
	}

	if p.acceptKeyword("WHERE") {
		expr, err := p.parseExpression()
		if err != nil {
			return nil, err
		}
		stmt.Where = expr
	}
	return stmt, nil
}

func (p *Parser) parseDelete() (*DeleteStmt, error) {
	if err := p.expectKeyword("DELETE"); err != nil {
		return nil, err
	}
	if err := p.expectKeyword("FROM"); err != nil {
		return nil, err
	}
	table, err := p.parseIdent("a table name")
	if err != nil {
		return nil, err
	}
	stmt := &DeleteStmt{Table: table}
	if p.acceptKeyword("WHERE") {
		expr, err := p.parseExpression()
		if err != nil {
			return nil, err
		}
		stmt.Where = expr
	}
	return stmt, nil
}

func (p *Parser) parseCreate() (Statement, error) {
	if err := p.expectKeyword("CREATE"); err != nil {
		return nil, err
	}
	unique := p.acceptKeyword("UNIQUE")

	switch {
	case p.acceptKeyword("TABLE"):
		if unique {
			return nil, p.errorf("UNIQUE is not valid on CREATE TABLE")
		}
		return p.parseCreateTable()
	case p.acceptKeyword("INDEX"):
		return p.parseCreateIndex(unique)
	default:
		return nil, p.errorf("expected TABLE or INDEX after CREATE, found %s", p.peek())
	}
}

func (p *Parser) parseCreateTable() (*CreateTableStmt, error) {
	stmt := &CreateTableStmt{}
	if p.acceptKeyword("IF") {
		if err := p.expectKeyword("NOT"); err != nil {
			return nil, err
		}
		if err := p.expectKeyword("EXISTS"); err != nil {
			return nil, err
		}
		stmt.IfNotExists = true
	}

	name, err := p.parseIdent("a table name")
	if err != nil {
		return nil, err
	}
	stmt.Table = name

	if err := p.expectPunct("("); err != nil {
		return nil, err
	}
	for {
		col, err := p.parseColumnDef()
		if err != nil {
			return nil, err
		}
		stmt.Columns = append(stmt.Columns, col)
		if !p.acceptPunct(",") {
			break
		}
	}
	if err := p.expectPunct(")"); err != nil {
		return nil, err
	}
	return stmt, nil
}

func (p *Parser) parseColumnDef() (ColumnDef, error) {
	name, err := p.parseIdent("a column name")
	if err != nil {
		return ColumnDef{}, err
	}
	col := ColumnDef{Name: name}

	typeName, err := p.parseIdent("a column type")
	if err != nil {
		return ColumnDef{}, err
	}
	// A length like VARCHAR(255) is accepted and ignored: this database stores
	// text as variable-length regardless, so enforcing the limit would be a
	// separate feature rather than a storage decision.
	if p.acceptPunct("(") {
		for !p.acceptPunct(")") {
			if p.atEOF() {
				return ColumnDef{}, p.errorf("unterminated type length")
			}
			p.next()
		}
	}
	t, ok := types.ParseType(typeName)
	if !ok {
		return ColumnDef{}, p.errorf("unknown column type %q", typeName)
	}
	col.Type = t

	for {
		switch {
		case p.acceptKeyword("PRIMARY"):
			if err := p.expectKeyword("KEY"); err != nil {
				return ColumnDef{}, err
			}
			col.PrimaryKey = true
			// A primary key is implicitly NOT NULL, which is part of what
			// "primary" means rather than a separate constraint.
			col.NotNull = true
		case p.acceptKeyword("NOT"):
			if err := p.expectKeyword("NULL"); err != nil {
				return ColumnDef{}, err
			}
			col.NotNull = true
		case p.acceptKeyword("UNIQUE"):
			col.Unique = true
		case p.acceptKeyword("NULL"):
			// Explicitly nullable, which is already the default.
		default:
			return col, nil
		}
	}
}

func (p *Parser) parseCreateIndex(unique bool) (*CreateIndexStmt, error) {
	name, err := p.parseIdent("an index name")
	if err != nil {
		return nil, err
	}
	if err := p.expectKeyword("ON"); err != nil {
		return nil, err
	}
	table, err := p.parseIdent("a table name")
	if err != nil {
		return nil, err
	}
	stmt := &CreateIndexStmt{Name: name, Table: table, Unique: unique}

	if err := p.expectPunct("("); err != nil {
		return nil, err
	}
	for {
		col, err := p.parseIdent("a column name")
		if err != nil {
			return nil, err
		}
		stmt.Columns = append(stmt.Columns, col)
		if !p.acceptPunct(",") {
			break
		}
	}
	if err := p.expectPunct(")"); err != nil {
		return nil, err
	}
	return stmt, nil
}

func (p *Parser) parseDrop() (Statement, error) {
	if err := p.expectKeyword("DROP"); err != nil {
		return nil, err
	}
	if err := p.expectKeyword("TABLE"); err != nil {
		return nil, err
	}
	stmt := &DropTableStmt{}
	if p.acceptKeyword("IF") {
		if err := p.expectKeyword("EXISTS"); err != nil {
			return nil, err
		}
		stmt.IfExists = true
	}
	name, err := p.parseIdent("a table name")
	if err != nil {
		return nil, err
	}
	stmt.Table = name
	return stmt, nil
}

// --- expressions ---
//
// Precedence, loosest first:
//
//	OR
//	AND
//	NOT
//	= <> < <= > >= IS IN BETWEEN LIKE
//	+ -
//	* / %
//	unary -
//	primary

// reservedInExpression are the keywords that introduce a clause, and so cannot
// double as a bare column name.
//
// Everything outside this set is fair game as an identifier, because most SQL
// keywords are also plausible column names and reserving them all would be
// stricter than the standard and more annoying than useful.
var reservedInExpression = map[string]bool{
	"FROM": true, "WHERE": true, "GROUP": true, "HAVING": true,
	"ORDER": true, "LIMIT": true, "OFFSET": true, "JOIN": true,
	"INNER": true, "LEFT": true, "RIGHT": true, "OUTER": true,
	"CROSS": true, "ON": true, "AS": true, "VALUES": true,
	"SET": true, "INTO": true, "SELECT": true, "INSERT": true,
	"UPDATE": true, "DELETE": true, "CREATE": true, "DROP": true,
}

func (p *Parser) parseExpression() (Expression, error) { return p.parseOr() }

func (p *Parser) parseOr() (Expression, error) {
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for p.acceptKeyword("OR") {
		right, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		left = &BinaryOp{Op: "OR", Left: left, Right: right}
	}
	return left, nil
}

func (p *Parser) parseAnd() (Expression, error) {
	left, err := p.parseNot()
	if err != nil {
		return nil, err
	}
	for p.acceptKeyword("AND") {
		right, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		left = &BinaryOp{Op: "AND", Left: left, Right: right}
	}
	return left, nil
}

func (p *Parser) parseNot() (Expression, error) {
	if p.acceptKeyword("NOT") {
		expr, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		return &UnaryOp{Op: "NOT", Expr: expr}, nil
	}
	return p.parseComparison()
}

func (p *Parser) parseComparison() (Expression, error) {
	left, err := p.parseAdditive()
	if err != nil {
		return nil, err
	}

	for {
		switch {
		case p.acceptKeyword("IS"):
			negated := p.acceptKeyword("NOT")
			if err := p.expectKeyword("NULL"); err != nil {
				return nil, err
			}
			left = &IsNull{Expr: left, Negated: negated}

		case p.isKeyword("NOT") && p.peekAt(1).Literal == "IN":
			p.pos += 2
			values, err := p.parseValueList()
			if err != nil {
				return nil, err
			}
			left = &InList{Expr: left, Values: values, Negated: true}

		case p.acceptKeyword("IN"):
			values, err := p.parseValueList()
			if err != nil {
				return nil, err
			}
			left = &InList{Expr: left, Values: values}

		case p.isKeyword("NOT") && p.peekAt(1).Literal == "BETWEEN":
			p.pos += 2
			low, high, err := p.parseBetweenBounds()
			if err != nil {
				return nil, err
			}
			left = &Between{Expr: left, Low: low, High: high, Negated: true}

		case p.acceptKeyword("BETWEEN"):
			low, high, err := p.parseBetweenBounds()
			if err != nil {
				return nil, err
			}
			left = &Between{Expr: left, Low: low, High: high}

		case p.isKeyword("NOT") && p.peekAt(1).Literal == "LIKE":
			p.pos += 2
			right, err := p.parseAdditive()
			if err != nil {
				return nil, err
			}
			left = &UnaryOp{Op: "NOT", Expr: &BinaryOp{Op: "LIKE", Left: left, Right: right}}

		case p.acceptKeyword("LIKE"):
			right, err := p.parseAdditive()
			if err != nil {
				return nil, err
			}
			left = &BinaryOp{Op: "LIKE", Left: left, Right: right}

		default:
			t := p.peek()
			if t.Type != TokenOperator {
				return left, nil
			}
			switch t.Literal {
			case "=", "<>", "!=", "<", "<=", ">", ">=":
				p.pos++
				right, err := p.parseAdditive()
				if err != nil {
					return nil, err
				}
				op := t.Literal
				if op == "!=" {
					op = "<>" // one spelling reaches the evaluator
				}
				left = &BinaryOp{Op: op, Left: left, Right: right}
			default:
				return left, nil
			}
		}
	}
}

func (p *Parser) parseValueList() ([]Expression, error) {
	if err := p.expectPunct("("); err != nil {
		return nil, err
	}
	var values []Expression
	for {
		expr, err := p.parseExpression()
		if err != nil {
			return nil, err
		}
		values = append(values, expr)
		if !p.acceptPunct(",") {
			break
		}
	}
	if err := p.expectPunct(")"); err != nil {
		return nil, err
	}
	return values, nil
}

// parseBetweenBounds reads `low AND high`.
//
// The bounds are parsed at additive precedence rather than as full
// expressions, because BETWEEN's AND is a separator, not the boolean operator:
// parsing `x BETWEEN 1 AND 5` at full precedence would swallow the AND and
// leave BETWEEN without a second bound.
func (p *Parser) parseBetweenBounds() (Expression, Expression, error) {
	low, err := p.parseAdditive()
	if err != nil {
		return nil, nil, err
	}
	if err := p.expectKeyword("AND"); err != nil {
		return nil, nil, err
	}
	high, err := p.parseAdditive()
	if err != nil {
		return nil, nil, err
	}
	return low, high, nil
}

func (p *Parser) parseAdditive() (Expression, error) {
	left, err := p.parseMultiplicative()
	if err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		if t.Type != TokenOperator || (t.Literal != "+" && t.Literal != "-" && t.Literal != "||") {
			return left, nil
		}
		p.pos++
		right, err := p.parseMultiplicative()
		if err != nil {
			return nil, err
		}
		left = &BinaryOp{Op: t.Literal, Left: left, Right: right}
	}
}

func (p *Parser) parseMultiplicative() (Expression, error) {
	left, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		if t.Type != TokenOperator || (t.Literal != "*" && t.Literal != "/" && t.Literal != "%") {
			return left, nil
		}
		p.pos++
		right, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		left = &BinaryOp{Op: t.Literal, Left: left, Right: right}
	}
}

func (p *Parser) parseUnary() (Expression, error) {
	if p.acceptOperator("-") {
		expr, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		// Fold a negated literal at parse time, so -1 is one node rather than
		// a negation wrapped around a positive.
		if lit, ok := expr.(*Literal); ok {
			switch lit.Value.Type {
			case types.TypeInt:
				return &Literal{Value: types.NewInt(-lit.Value.Int)}, nil
			case types.TypeFloat:
				return &Literal{Value: types.NewFloat(-lit.Value.Float)}, nil
			}
		}
		return &UnaryOp{Op: "-", Expr: expr}, nil
	}
	if p.acceptOperator("+") {
		return p.parseUnary()
	}
	return p.parsePrimary()
}

func (p *Parser) parsePrimary() (Expression, error) {
	t := p.peek()

	switch t.Type {
	case TokenNumber:
		p.pos++
		if strings.ContainsAny(t.Literal, ".eE") {
			f, err := strconv.ParseFloat(t.Literal, 64)
			if err != nil {
				return nil, p.errorf("invalid number %q", t.Literal)
			}
			return &Literal{Value: types.NewFloat(f)}, nil
		}
		n, err := strconv.ParseInt(t.Literal, 10, 64)
		if err != nil {
			// Out of int64 range, so keep it as a float rather than refusing.
			f, ferr := strconv.ParseFloat(t.Literal, 64)
			if ferr != nil {
				return nil, p.errorf("invalid number %q", t.Literal)
			}
			return &Literal{Value: types.NewFloat(f)}, nil
		}
		return &Literal{Value: types.NewInt(n)}, nil

	case TokenString:
		p.pos++
		return &Literal{Value: types.NewText(t.Literal)}, nil

	case TokenKeyword:
		switch t.Literal {
		case "NULL":
			p.pos++
			return &Literal{Value: types.Null()}, nil
		case "TRUE":
			p.pos++
			return &Literal{Value: types.NewBool(true)}, nil
		case "FALSE":
			p.pos++
			return &Literal{Value: types.NewBool(false)}, nil
		case "COUNT", "SUM", "AVG", "MIN", "MAX":
			// Only a function call if a parenthesis follows. `SELECT count
			// FROM t` has to work, because count is a perfectly ordinary
			// column name.
			if next := p.peekAt(1); next.Type == TokenPunct && next.Literal == "(" {
				return p.parseFunctionCall()
			}
		}
		// A keyword that introduces a clause cannot also be a bare column
		// name, or `SELECT FROM t` would silently parse as selecting a column
		// called "FROM" instead of being the error it is.
		if reservedInExpression[t.Literal] {
			return nil, p.errorf("expected a value, found the keyword %s", t.Literal)
		}
		// Any other keyword in value position is a column named after one.
		return p.parseColumnOrFunction()

	case TokenIdent:
		return p.parseColumnOrFunction()

	case TokenPunct:
		if t.Literal == "(" {
			p.pos++
			expr, err := p.parseExpression()
			if err != nil {
				return nil, err
			}
			if err := p.expectPunct(")"); err != nil {
				return nil, err
			}
			return expr, nil
		}
	}
	return nil, p.errorf("expected a value, found %s", t)
}

// parseColumnOrFunction resolves the ambiguity between `name`, `table.column`
// and `name(...)`, which cannot be told apart until the token after the name.
func (p *Parser) parseColumnOrFunction() (Expression, error) {
	name := p.next().Literal

	if p.peek().Type == TokenPunct && p.peek().Literal == "(" {
		p.pos--
		return p.parseFunctionCall()
	}

	if p.acceptPunct(".") {
		column, err := p.parseIdent("a column name")
		if err != nil {
			return nil, err
		}
		return &ColumnRef{Table: name, Column: column}, nil
	}
	return &ColumnRef{Column: name}, nil
}

func (p *Parser) parseFunctionCall() (Expression, error) {
	name := p.next().Literal
	if err := p.expectPunct("("); err != nil {
		return nil, err
	}

	call := &FunctionCall{Name: strings.ToUpper(name)}

	// COUNT(*) counts rows, including those where every column is NULL, which
	// is why it is tracked separately from COUNT(expr).
	if p.acceptOperator("*") {
		call.Star = true
		if err := p.expectPunct(")"); err != nil {
			return nil, err
		}
		return call, nil
	}

	call.Distinct = p.acceptKeyword("DISTINCT")
	if p.acceptPunct(")") {
		return call, nil
	}
	for {
		arg, err := p.parseExpression()
		if err != nil {
			return nil, err
		}
		call.Args = append(call.Args, arg)
		if !p.acceptPunct(",") {
			break
		}
	}
	if err := p.expectPunct(")"); err != nil {
		return nil, err
	}
	return call, nil
}
