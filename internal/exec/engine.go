package exec

import (
	"fmt"
	"strings"
	"time"

	"github.com/Nafiz001/quill/internal/catalog"
	"github.com/Nafiz001/quill/internal/sql"
	"github.com/Nafiz001/quill/internal/storage"
	"github.com/Nafiz001/quill/internal/types"
)

// Engine executes statements against a database.
type Engine struct {
	pager   *storage.Pager
	catalog *catalog.Catalog
	limits  Limits
}

// NewEngine builds an engine over an open database.
func NewEngine(pager *storage.Pager, cat *catalog.Catalog) *Engine {
	return &Engine{pager: pager, catalog: cat}
}

// SetLimits bounds what one statement may cost; see Limits.
func (e *Engine) SetLimits(timeout time.Duration, maxRows int) {
	e.limits.Timeout = timeout
	e.limits.MaxRows = maxRows
}

// Catalog exposes the schema, for tooling and the wire protocol.
func (e *Engine) Catalog() *catalog.Catalog { return e.catalog }

// Result is what a statement produced.
type Result struct {
	// Columns and Rows are set by a query.
	Columns []string
	Rows    []Row
	// RowsAffected is set by INSERT, UPDATE and DELETE.
	RowsAffected int64
	// Tag is the command tag a client displays: "SELECT 3", "INSERT 0 1".
	Tag string
}

// Execute runs one statement.
func (e *Engine) Execute(stmt sql.Statement) (*Result, error) {
	e.limits.begin()
	switch s := stmt.(type) {
	case *sql.SelectStmt:
		return e.executeSelect(s)
	case *sql.InsertStmt:
		return e.executeInsert(s)
	case *sql.UpdateStmt:
		return e.executeUpdate(s)
	case *sql.DeleteStmt:
		return e.executeDelete(s)
	case *sql.CreateTableStmt:
		return e.executeCreateTable(s)
	case *sql.DropTableStmt:
		return e.executeDropTable(s)
	case *sql.CreateIndexStmt:
		return e.executeCreateIndex(s)
	case *sql.ExplainStmt:
		return e.executeExplain(s)
	case *sql.TransactionStmt:
		// Transaction control is handled by the session above the engine,
		// which knows whether one is already open.
		return &Result{Tag: s.Action}, nil
	default:
		return nil, fmt.Errorf("unsupported statement %T", stmt)
	}
}

// ExecuteString parses and runs a single statement.
func (e *Engine) ExecuteString(query string) (*Result, error) {
	stmt, err := sql.Parse(query)
	if err != nil {
		return nil, err
	}
	return e.Execute(stmt)
}

// --- SELECT ---

// Plan builds the operator tree for a query.
//
// The order the operators are stacked in is the logical order of a SELECT,
// which is not the order it is written: FROM, then WHERE, then GROUP BY, then
// HAVING, then the select list, then DISTINCT, then ORDER BY, then LIMIT. That
// is why a column alias defined in the select list cannot be used in WHERE --
// the filter runs before the projection that would have created it.
func (e *Engine) Plan(stmt *sql.SelectStmt) (Operator, error) {
	var op Operator
	var err error

	if stmt.From == nil {
		// `SELECT 1` with no table: one empty row to project over.
		op = NewValues([]Row{{}}, &Schema{})
	} else {
		op, err = e.planFrom(stmt.From, stmt.Where)
		if err != nil {
			return nil, err
		}
	}

	// WHERE, unless the scan already absorbed it as a key range.
	if stmt.Where != nil && !e.whereAbsorbed(op) {
		op, err = NewFilter(op, stmt.Where)
		if err != nil {
			return nil, err
		}
	}

	// sortedBelow records that ORDER BY was applied before the projection.
	sortedBelow := false

	// Aggregation, if the select list or HAVING mentions an aggregate.
	var aggregates []*sql.FunctionCall
	for _, item := range stmt.Items {
		collectAggregates(item.Expr, &aggregates)
	}
	if stmt.Having != nil {
		collectAggregates(stmt.Having, &aggregates)
	}

	if len(aggregates) > 0 || len(stmt.GroupBy) > 0 {
		agg, err := NewAggregate(op, stmt.GroupBy, aggregates)
		if err != nil {
			return nil, err
		}
		op = agg

		if stmt.Having != nil {
			// HAVING filters groups, so it runs against the aggregate's
			// output, where the aggregate calls have become plain columns.
			rewritten := rewriteAggregates(stmt.Having, stmt.GroupBy, aggregates)
			op, err = NewFilter(op, rewritten)
			if err != nil {
				return nil, err
			}
		}

		// The select list refers to aggregate calls; rewrite them to point at
		// the aggregate's output columns.
		items := make([]sql.SelectItem, len(stmt.Items))
		for i, item := range stmt.Items {
			items[i] = sql.SelectItem{
				Expr:  rewriteAggregates(item.Expr, stmt.GroupBy, aggregates),
				Alias: item.Alias,
			}
			if items[i].Alias == "" {
				items[i].Alias = outputName(stmt.Items[i])
			}
		}
		op, err = NewProject(op, items)
		if err != nil {
			return nil, err
		}
	} else {
		// ORDER BY is allowed to name columns the SELECT list does not
		// return -- `SELECT name FROM t ORDER BY score` is valid SQL. Sorting
		// below the projection is what makes that work, since the scan's
		// columns are all still present there. Projection is one row in, one
		// row out, so it preserves the order it is handed.
		//
		// When the sort keys do not resolve there, they are aliases defined by
		// the select list itself, and the sort has to happen above it instead.
		if len(stmt.OrderBy) > 0 && canResolveAll(stmt.OrderBy, op.Schema()) {
			op, err = NewSort(op, stmt.OrderBy)
			if err != nil {
				return nil, err
			}
			sortedBelow = true
		}

		op, err = NewProject(op, stmt.Items)
		if err != nil {
			return nil, err
		}
	}

	if stmt.Distinct {
		op = NewDistinct(op)
	}

	if len(stmt.OrderBy) > 0 && !sortedBelow {
		op, err = NewSort(op, stmt.OrderBy)
		if err != nil {
			return nil, err
		}
	}

	if stmt.Limit != nil || stmt.Offset != nil {
		limit, offset := int64(-1), int64(0)
		if stmt.Limit != nil {
			v, err := constantInt(stmt.Limit)
			if err != nil {
				return nil, fmt.Errorf("LIMIT: %w", err)
			}
			limit = v
		}
		if stmt.Offset != nil {
			v, err := constantInt(stmt.Offset)
			if err != nil {
				return nil, fmt.Errorf("OFFSET: %w", err)
			}
			offset = v
		}
		op = NewLimit(op, limit, offset)
	}

	return op, nil
}

// canResolveAll reports whether every ORDER BY expression can be evaluated
// against a schema, which is how the planner decides where the sort belongs.
func canResolveAll(items []sql.OrderByItem, schema *Schema) bool {
	for _, item := range items {
		if _, err := NewEvaluator(item.Expr, schema); err != nil {
			return false
		}
	}
	return true
}

// whereAbsorbed reports whether the scan already applied the predicate as a
// key range, so the filter would be redundant.
//
// It is deliberately conservative: an index scan on a key range still needs
// the filter when the predicate had other conjuncts, so this only returns true
// for the exact-match case the planner recognises.
func (e *Engine) whereAbsorbed(op Operator) bool {
	scan, ok := op.(*IndexScan)
	return ok && strings.HasPrefix(scan.low, "exact ")
}

func (e *Engine) planFrom(ref *sql.TableRef, where sql.Expression) (Operator, error) {
	left, err := e.planTable(ref, where)
	if err != nil {
		return nil, err
	}
	if ref.Join == nil {
		return left, nil
	}

	right, err := e.planTable(ref.Join.Right, nil)
	if err != nil {
		return nil, err
	}
	join, err := NewNestedLoopJoin(left, right, ref.Join.Type, ref.Join.On)
	if join != nil {
		join.lim = &e.limits
	}
	return join, err
}

// planTable chooses how to read one table.
//
// This is the whole of the optimiser: if the WHERE clause pins the primary key
// to a constant, descend the tree straight to it; otherwise read everything.
// A real planner would estimate cardinalities and compare costs across several
// candidate plans. What is here is the one decision that changes a query from
// O(n) to O(log n), and it is honest about being the only one.
func (e *Engine) planTable(ref *sql.TableRef, where sql.Expression) (Operator, error) {
	table, ok := e.catalog.Table(ref.Name)
	if !ok {
		return nil, fmt.Errorf("table %q does not exist", ref.Name)
	}

	if where != nil {
		if key, describe, ok := e.primaryKeyLookup(table, ref.Ref(), where); ok {
			scan, err := NewIndexScan(e.catalog, table, ref.Alias, key, key, describe)
			if scan != nil {
				scan.lim = &e.limits
			}
			return scan, err
		}
	}
	scan, err := NewSeqScan(e.catalog, table, ref.Alias)
	if scan != nil {
		scan.lim = &e.limits
	}
	return scan, err
}

// primaryKeyLookup recognises `WHERE pk = constant` and returns the encoded key.
func (e *Engine) primaryKeyLookup(table *catalog.Table, alias string, where sql.Expression) ([]byte, string, bool) {
	pkIndex := table.PrimaryKeyIndex()
	if pkIndex < 0 {
		return nil, "", false
	}

	// Only a top-level equality, or one joined by AND, counts.
	var find func(expr sql.Expression) ([]byte, string, bool)
	find = func(expr sql.Expression) ([]byte, string, bool) {
		bin, ok := expr.(*sql.BinaryOp)
		if !ok {
			return nil, "", false
		}
		if bin.Op == "AND" {
			if key, d, ok := find(bin.Left); ok {
				return key, d, true
			}
			return find(bin.Right)
		}
		if bin.Op != "=" {
			return nil, "", false
		}

		ref, lit := matchColumnLiteral(bin.Left, bin.Right)
		if ref == nil {
			return nil, "", false
		}
		if ref.Table != "" && !strings.EqualFold(ref.Table, alias) && !strings.EqualFold(ref.Table, table.Name) {
			return nil, "", false
		}
		if !strings.EqualFold(ref.Column, table.Columns[pkIndex].Name) {
			return nil, "", false
		}
		value, err := lit.Value.Cast(table.Columns[pkIndex].Type)
		if err != nil {
			return nil, "", false
		}
		return types.EncodeKey([]types.Value{value}),
			fmt.Sprintf("exact %s = %s", ref.Column, value), true
	}
	return find(where)
}

// matchColumnLiteral accepts `col = 1` and `1 = col` alike.
func matchColumnLiteral(a, b sql.Expression) (*sql.ColumnRef, *sql.Literal) {
	if ref, ok := a.(*sql.ColumnRef); ok {
		if lit, ok := b.(*sql.Literal); ok {
			return ref, lit
		}
	}
	if ref, ok := b.(*sql.ColumnRef); ok {
		if lit, ok := a.(*sql.Literal); ok {
			return ref, lit
		}
	}
	return nil, nil
}

// rewriteAggregates replaces aggregate calls with references to the
// aggregate operator's output columns.
func rewriteAggregates(expr sql.Expression, groupBy []sql.Expression, calls []*sql.FunctionCall) sql.Expression {
	switch node := expr.(type) {
	case *sql.FunctionCall:
		if node.IsAggregate() {
			for i, call := range calls {
				if call == node {
					return &sql.ColumnRef{Column: aggregateColumnName(i)}
				}
			}
		}
		return node
	case *sql.BinaryOp:
		return &sql.BinaryOp{
			Op:    node.Op,
			Left:  rewriteAggregates(node.Left, groupBy, calls),
			Right: rewriteAggregates(node.Right, groupBy, calls),
		}
	case *sql.UnaryOp:
		return &sql.UnaryOp{Op: node.Op, Expr: rewriteAggregates(node.Expr, groupBy, calls)}
	}

	// A grouping expression appears verbatim in the aggregate's output.
	for i, g := range groupBy {
		if g.String() == expr.String() {
			return &sql.ColumnRef{Column: groupColumnName(i)}
		}
	}
	return expr
}

func groupColumnName(i int) string     { return fmt.Sprintf("__group%d", i) }
func aggregateColumnName(i int) string { return fmt.Sprintf("__agg%d", i) }

func (e *Engine) executeSelect(stmt *sql.SelectStmt) (*Result, error) {
	op, err := e.Plan(stmt)
	if err != nil {
		return nil, err
	}
	defer op.Close()

	result := &Result{Columns: op.Schema().Names()}
	for {
		row, err := op.Next()
		if err != nil {
			return nil, err
		}
		if row == nil {
			break
		}
		result.Rows = append(result.Rows, row)
		if e.limits.MaxRows > 0 && len(result.Rows) > e.limits.MaxRows {
			return nil, ErrResultTooLarge
		}
	}
	result.Tag = fmt.Sprintf("SELECT %d", len(result.Rows))
	return result, nil
}

// --- INSERT ---

func (e *Engine) executeInsert(stmt *sql.InsertStmt) (*Result, error) {
	table, ok := e.catalog.Table(stmt.Table)
	if !ok {
		return nil, fmt.Errorf("table %q does not exist", stmt.Table)
	}

	// Map the statement's column list onto table positions. Without one, the
	// values are positional across every column.
	positions := make([]int, 0, len(table.Columns))
	if len(stmt.Columns) == 0 {
		for i := range table.Columns {
			positions = append(positions, i)
		}
	} else {
		for _, name := range stmt.Columns {
			idx := table.ColumnIndex(name)
			if idx < 0 {
				return nil, fmt.Errorf("column %q does not exist in table %q", name, table.Name)
			}
			positions = append(positions, idx)
		}
	}

	tree := e.catalog.TableTree(table)
	pkIndex := table.PrimaryKeyIndex()
	emptySchema := &Schema{}
	inserted := int64(0)

	for _, exprs := range stmt.Rows {
		if len(exprs) != len(positions) {
			return nil, fmt.Errorf("row has %d values but %d columns were named",
				len(exprs), len(positions))
		}

		// Every column starts NULL, then the named ones are filled.
		row := make([]types.Value, len(table.Columns))
		for i := range row {
			row[i] = types.Null()
		}
		for i, expr := range exprs {
			eval, err := NewEvaluator(expr, emptySchema)
			if err != nil {
				return nil, err
			}
			v, err := eval.Eval(nil)
			if err != nil {
				return nil, err
			}
			col := table.Columns[positions[i]]
			casted, err := v.Cast(col.Type)
			if err != nil {
				return nil, fmt.Errorf("column %q: %w", col.Name, err)
			}
			row[positions[i]] = casted
		}

		for i, col := range table.Columns {
			if col.NotNull && row[i].IsNull() {
				return nil, fmt.Errorf("column %q cannot be NULL", col.Name)
			}
		}

		// The row's key is its primary key, or an implicit row id when the
		// table declared none. Either way the table is a B+tree.
		var key []byte
		if pkIndex >= 0 {
			if row[pkIndex].IsNull() {
				return nil, fmt.Errorf("primary key %q cannot be NULL", table.Columns[pkIndex].Name)
			}
			key = types.EncodeKey([]types.Value{row[pkIndex]})
			if _, exists, err := tree.Get(key); err != nil {
				return nil, err
			} else if exists {
				return nil, fmt.Errorf("duplicate key value violates the primary key on %q", table.Name)
			}
		} else {
			id, err := e.catalog.NextRowID(table)
			if err != nil {
				return nil, err
			}
			key = types.EncodeRowID(id)
		}

		if err := tree.Put(key, types.EncodeRow(row)); err != nil {
			return nil, err
		}
		if err := e.updateIndexes(table, row, key, true); err != nil {
			return nil, err
		}
		inserted++
	}

	return &Result{RowsAffected: inserted, Tag: fmt.Sprintf("INSERT 0 %d", inserted)}, nil
}

// updateIndexes adds or removes a row's entries in every secondary index.
//
// An index entry maps the indexed values to the row's primary key, so a lookup
// is two descents: one in the index to find the key, one in the table to find
// the row. That indirection is why an index costs storage and write time, and
// why creating one on every column is a mistake rather than a free win.
func (e *Engine) updateIndexes(table *catalog.Table, row []types.Value, primaryKey []byte, adding bool) error {
	for i := range table.Indexes {
		idx := &table.Indexes[i]
		tree := e.catalog.IndexTree(table, idx)

		keyValues := make([]types.Value, len(idx.Columns))
		for j, col := range idx.Columns {
			keyValues[j] = row[col]
		}
		// The primary key is appended so that two rows with the same indexed
		// value get distinct index entries; without it the second would
		// overwrite the first.
		entryKey := append(types.EncodeKey(keyValues), primaryKey...)

		if adding {
			if idx.Unique {
				prefix := types.EncodeKey(keyValues)
				cursor, err := tree.Scan(prefix, append(append([]byte(nil), prefix...), 0xFF))
				if err != nil {
					return err
				}
				if cursor.Valid() {
					return fmt.Errorf("duplicate key value violates the unique index %q", idx.Name)
				}
			}
			if err := tree.Put(entryKey, primaryKey); err != nil {
				return err
			}
		} else {
			if _, err := tree.Delete(entryKey); err != nil {
				return err
			}
		}
	}
	return nil
}

// --- UPDATE and DELETE ---

func (e *Engine) executeUpdate(stmt *sql.UpdateStmt) (*Result, error) {
	table, ok := e.catalog.Table(stmt.Table)
	if !ok {
		return nil, fmt.Errorf("table %q does not exist", stmt.Table)
	}

	targets, err := e.collectTargets(table, stmt.Where)
	if err != nil {
		return nil, err
	}

	schema := tableSchema(table, table.Name)
	assignments := make([]*Evaluator, len(stmt.Set))
	columns := make([]int, len(stmt.Set))
	for i, a := range stmt.Set {
		idx := table.ColumnIndex(a.Column)
		if idx < 0 {
			return nil, fmt.Errorf("column %q does not exist in table %q", a.Column, table.Name)
		}
		columns[i] = idx
		eval, err := NewEvaluator(a.Value, schema)
		if err != nil {
			return nil, err
		}
		assignments[i] = eval
	}

	tree := e.catalog.TableTree(table)
	pkIndex := table.PrimaryKeyIndex()
	updated := int64(0)

	for _, target := range targets {
		newRow := append([]types.Value(nil), target.row...)
		for i, eval := range assignments {
			v, err := eval.Eval(target.row)
			if err != nil {
				return nil, err
			}
			col := table.Columns[columns[i]]
			casted, err := v.Cast(col.Type)
			if err != nil {
				return nil, fmt.Errorf("column %q: %w", col.Name, err)
			}
			if col.NotNull && casted.IsNull() {
				return nil, fmt.Errorf("column %q cannot be NULL", col.Name)
			}
			newRow[columns[i]] = casted
		}

		// Changing the primary key moves the row, because the key is where the
		// row lives. It is a delete plus an insert, not an update in place.
		newKey := target.key
		if pkIndex >= 0 {
			candidate := types.EncodeKey([]types.Value{newRow[pkIndex]})
			if string(candidate) != string(target.key) {
				if _, exists, err := tree.Get(candidate); err != nil {
					return nil, err
				} else if exists {
					return nil, fmt.Errorf("duplicate key value violates the primary key on %q", table.Name)
				}
				if _, err := tree.Delete(target.key); err != nil {
					return nil, err
				}
				newKey = candidate
			}
		}

		if err := e.updateIndexes(table, target.row, target.key, false); err != nil {
			return nil, err
		}
		if err := tree.Put(newKey, types.EncodeRow(newRow)); err != nil {
			return nil, err
		}
		if err := e.updateIndexes(table, newRow, newKey, true); err != nil {
			return nil, err
		}
		updated++
	}

	return &Result{RowsAffected: updated, Tag: fmt.Sprintf("UPDATE %d", updated)}, nil
}

func (e *Engine) executeDelete(stmt *sql.DeleteStmt) (*Result, error) {
	table, ok := e.catalog.Table(stmt.Table)
	if !ok {
		return nil, fmt.Errorf("table %q does not exist", stmt.Table)
	}

	targets, err := e.collectTargets(table, stmt.Where)
	if err != nil {
		return nil, err
	}

	tree := e.catalog.TableTree(table)
	deleted := int64(0)
	for _, target := range targets {
		if err := e.updateIndexes(table, target.row, target.key, false); err != nil {
			return nil, err
		}
		ok, err := tree.Delete(target.key)
		if err != nil {
			return nil, err
		}
		if ok {
			deleted++
		}
	}
	return &Result{RowsAffected: deleted, Tag: fmt.Sprintf("DELETE %d", deleted)}, nil
}

// target is one row selected for modification.
type target struct {
	key []byte
	row []types.Value
}

// collectTargets finds the rows a WHERE clause selects.
//
// The matching rows are gathered before any are changed, deliberately.
// Modifying a B+tree while a cursor is walking it can move entries out from
// under the cursor -- a row could be skipped, or visited twice, and an UPDATE
// that shifts a key forward could update the same row repeatedly. The cost is
// holding the matches in memory; the alternative is a correctness bug that only
// shows up on large tables.
func (e *Engine) collectTargets(table *catalog.Table, where sql.Expression) ([]target, error) {
	tree := e.catalog.TableTree(table)
	schema := tableSchema(table, table.Name)

	var predicate *Evaluator
	if where != nil {
		eval, err := NewEvaluator(where, schema)
		if err != nil {
			return nil, err
		}
		predicate = eval
	}

	cursor, err := tree.Scan(nil, nil)
	if err != nil {
		return nil, err
	}

	var targets []target
	for cursor.Valid() {
		row, err := types.DecodeRow(cursor.Value())
		if err != nil {
			return nil, err
		}
		keep := true
		if predicate != nil {
			v, err := predicate.Eval(Row(row))
			if err != nil {
				return nil, err
			}
			keep = !v.IsNull() && v.Truthy()
		}
		if keep {
			targets = append(targets, target{
				key: append([]byte(nil), cursor.Key()...),
				row: row,
			})
		}
		cursor.Next()
	}
	return targets, cursor.Err()
}

// --- DDL ---

func (e *Engine) executeCreateTable(stmt *sql.CreateTableStmt) (*Result, error) {
	if _, exists := e.catalog.Table(stmt.Table); exists {
		if stmt.IfNotExists {
			return &Result{Tag: "CREATE TABLE"}, nil
		}
		return nil, fmt.Errorf("table %q already exists", stmt.Table)
	}

	columns := make([]catalog.Column, len(stmt.Columns))
	for i, c := range stmt.Columns {
		columns[i] = catalog.Column{
			Name:       c.Name,
			Type:       c.Type,
			NotNull:    c.NotNull,
			PrimaryKey: c.PrimaryKey,
			Unique:     c.Unique,
		}
	}
	if _, err := e.catalog.CreateTable(stmt.Table, columns); err != nil {
		return nil, err
	}
	return &Result{Tag: "CREATE TABLE"}, nil
}

func (e *Engine) executeDropTable(stmt *sql.DropTableStmt) (*Result, error) {
	if _, exists := e.catalog.Table(stmt.Table); !exists {
		if stmt.IfExists {
			return &Result{Tag: "DROP TABLE"}, nil
		}
		return nil, fmt.Errorf("table %q does not exist", stmt.Table)
	}
	if err := e.catalog.DropTable(stmt.Table); err != nil {
		return nil, err
	}
	return &Result{Tag: "DROP TABLE"}, nil
}

func (e *Engine) executeCreateIndex(stmt *sql.CreateIndexStmt) (*Result, error) {
	table, ok := e.catalog.Table(stmt.Table)
	if !ok {
		return nil, fmt.Errorf("table %q does not exist", stmt.Table)
	}

	columns := make([]int, len(stmt.Columns))
	for i, name := range stmt.Columns {
		idx := table.ColumnIndex(name)
		if idx < 0 {
			return nil, fmt.Errorf("column %q does not exist in table %q", name, stmt.Table)
		}
		columns[i] = idx
	}

	idx, err := e.catalog.CreateIndex(stmt.Table, stmt.Name, columns, stmt.Unique)
	if err != nil {
		return nil, err
	}

	// Backfill: an index created on a table that already holds rows has to be
	// populated, or it would silently omit everything written before it.
	tree := e.catalog.TableTree(table)
	cursor, err := tree.Scan(nil, nil)
	if err != nil {
		return nil, err
	}
	indexTree := e.catalog.IndexTree(table, idx)
	for cursor.Valid() {
		row, err := types.DecodeRow(cursor.Value())
		if err != nil {
			return nil, err
		}
		keyValues := make([]types.Value, len(idx.Columns))
		for j, col := range idx.Columns {
			keyValues[j] = row[col]
		}
		primaryKey := append([]byte(nil), cursor.Key()...)
		if err := indexTree.Put(append(types.EncodeKey(keyValues), primaryKey...), primaryKey); err != nil {
			return nil, err
		}
		cursor.Next()
	}
	if err := cursor.Err(); err != nil {
		return nil, err
	}
	return &Result{Tag: "CREATE INDEX"}, nil
}

// --- EXPLAIN ---

func (e *Engine) executeExplain(stmt *sql.ExplainStmt) (*Result, error) {
	sel, ok := stmt.Statement.(*sql.SelectStmt)
	if !ok {
		return &Result{
			Columns: []string{"QUERY PLAN"},
			Rows:    []Row{{types.NewText(stmt.Statement.String())}},
			Tag:     "EXPLAIN",
		}, nil
	}

	op, err := e.Plan(sel)
	if err != nil {
		return nil, err
	}
	defer op.Close()

	var lines []Row
	var walk func(op Operator, depth int)
	walk = func(op Operator, depth int) {
		lines = append(lines, Row{types.NewText(strings.Repeat("  ", depth) + "-> " + op.Explain())})
		for _, child := range op.Children() {
			walk(child, depth+1)
		}
	}
	walk(op, 0)

	return &Result{Columns: []string{"QUERY PLAN"}, Rows: lines, Tag: "EXPLAIN"}, nil
}

// constantInt evaluates a constant expression to an integer, for LIMIT and
// OFFSET which cannot depend on a row.
func constantInt(expr sql.Expression) (int64, error) {
	lit, ok := expr.(*sql.Literal)
	if !ok {
		return 0, fmt.Errorf("must be a constant")
	}
	v, err := lit.Value.Cast(types.TypeInt)
	if err != nil {
		return 0, err
	}
	if v.Int < 0 {
		return 0, fmt.Errorf("must not be negative")
	}
	return v.Int, nil
}
