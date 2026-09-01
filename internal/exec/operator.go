package exec

import (
	"fmt"
	"sort"
	"strings"

	"github.com/nafizahmed/quill/internal/catalog"
	"github.com/nafizahmed/quill/internal/sql"
	"github.com/nafizahmed/quill/internal/storage"
	"github.com/nafizahmed/quill/internal/types"
)

// Operator is one node in a query plan.
//
// Next returns the next row, or nil when the input is exhausted. Returning nil
// rather than a boolean keeps the common case -- a row -- to one return value,
// and every operator's loop reads the same way.
type Operator interface {
	// Schema describes the rows this operator produces.
	Schema() *Schema
	// Next produces the next row, or nil at the end.
	Next() (Row, error)
	// Close releases resources. It is safe to call more than once.
	Close() error
	// Explain renders this node for EXPLAIN, without its children.
	Explain() string
	// Children returns the operators feeding this one.
	Children() []Operator
}

// --- sequential scan ---

// SeqScan reads every row of a table in key order.
type SeqScan struct {
	table  *catalog.Table
	schema *Schema
	cursor *storage.Cursor
	// rowsRead backs EXPLAIN ANALYZE-style reporting.
	rowsRead int
	done     bool
}

// NewSeqScan opens a scan over a whole table.
func NewSeqScan(cat *catalog.Catalog, table *catalog.Table, alias string) (*SeqScan, error) {
	tree := cat.TableTree(table)
	cursor, err := tree.Scan(nil, nil)
	if err != nil {
		return nil, err
	}
	return &SeqScan{table: table, schema: tableSchema(table, alias), cursor: cursor}, nil
}

// tableSchema builds the output schema of a scan, qualified by the alias the
// query used so that `t.col` resolves.
func tableSchema(table *catalog.Table, alias string) *Schema {
	if alias == "" {
		alias = table.Name
	}
	s := &Schema{Columns: make([]ColumnInfo, len(table.Columns))}
	for i, col := range table.Columns {
		s.Columns[i] = ColumnInfo{Table: alias, Name: col.Name, Type: col.Type}
	}
	return s
}

func (s *SeqScan) Schema() *Schema { return s.schema }

func (s *SeqScan) Next() (Row, error) {
	if s.done || !s.cursor.Valid() {
		s.done = true
		return nil, s.cursor.Err()
	}
	values, err := types.DecodeRow(s.cursor.Value())
	if err != nil {
		return nil, fmt.Errorf("decode row: %w", err)
	}
	s.cursor.Next()
	s.rowsRead++
	return Row(values), nil
}

func (s *SeqScan) Close() error         { return nil }
func (s *SeqScan) Children() []Operator { return nil }
func (s *SeqScan) Explain() string {
	return fmt.Sprintf("SeqScan on %s", s.table.Name)
}

// --- index scan ---

// IndexScan reads a bounded range of a table's primary key.
//
// This is the payoff for storing rows in a B+tree: a query with a bound on the
// key descends once and walks only the matching leaves, instead of reading
// every page and discarding most of them.
type IndexScan struct {
	table     *catalog.Table
	schema    *Schema
	cursor    *storage.Cursor
	low, high string
	rowsRead  int
	done      bool
}

// NewIndexScan opens a scan over [low, high]. A nil bound is open-ended.
func NewIndexScan(cat *catalog.Catalog, table *catalog.Table, alias string, low, high []byte, describe string) (*IndexScan, error) {
	tree := cat.TableTree(table)
	// The tree's range is half-open, so an inclusive upper bound is expressed
	// by extending the key with a zero byte -- the smallest thing that sorts
	// after it.
	var end []byte
	if high != nil {
		end = append(append([]byte(nil), high...), 0x00)
	}
	cursor, err := tree.Scan(low, end)
	if err != nil {
		return nil, err
	}
	return &IndexScan{table: table, schema: tableSchema(table, alias), cursor: cursor, low: describe}, nil
}

func (s *IndexScan) Schema() *Schema { return s.schema }

func (s *IndexScan) Next() (Row, error) {
	if s.done || !s.cursor.Valid() {
		s.done = true
		return nil, s.cursor.Err()
	}
	values, err := types.DecodeRow(s.cursor.Value())
	if err != nil {
		return nil, fmt.Errorf("decode row: %w", err)
	}
	s.cursor.Next()
	s.rowsRead++
	return Row(values), nil
}

func (s *IndexScan) Close() error         { return nil }
func (s *IndexScan) Children() []Operator { return nil }
func (s *IndexScan) Explain() string {
	return fmt.Sprintf("IndexScan on %s (%s)", s.table.Name, s.low)
}

// --- filter ---

// Filter passes through only the rows whose predicate is true.
type Filter struct {
	child     Operator
	predicate *Evaluator
	expr      sql.Expression
}

func NewFilter(child Operator, expr sql.Expression) (*Filter, error) {
	eval, err := NewEvaluator(expr, child.Schema())
	if err != nil {
		return nil, err
	}
	return &Filter{child: child, predicate: eval, expr: expr}, nil
}

func (f *Filter) Schema() *Schema { return f.child.Schema() }

func (f *Filter) Next() (Row, error) {
	// The loop is what makes filtering streaming: it keeps pulling until it
	// has something to return, so no intermediate result is ever built.
	for {
		row, err := f.child.Next()
		if err != nil || row == nil {
			return nil, err
		}
		v, err := f.predicate.Eval(row)
		if err != nil {
			return nil, err
		}
		// A NULL predicate is not true, so the row is dropped -- which is why
		// `WHERE x <> 1` does not return rows where x is NULL.
		if !v.IsNull() && v.Truthy() {
			return row, nil
		}
	}
}

func (f *Filter) Close() error         { return f.child.Close() }
func (f *Filter) Children() []Operator { return []Operator{f.child} }
func (f *Filter) Explain() string      { return "Filter: " + f.expr.String() }

// --- projection ---

// Project evaluates the SELECT list.
type Project struct {
	child  Operator
	evals  []*Evaluator
	schema *Schema
	items  []sql.SelectItem
}

func NewProject(child Operator, items []sql.SelectItem) (*Project, error) {
	p := &Project{child: child, items: items}

	// Expand any star into the child's columns, so the rest of the operator
	// works on a concrete list.
	var expanded []sql.SelectItem
	for _, item := range items {
		star, ok := item.Expr.(*sql.Star)
		if !ok {
			expanded = append(expanded, item)
			continue
		}
		for _, col := range child.Schema().Columns {
			if star.Table != "" && !strings.EqualFold(star.Table, col.Table) {
				continue
			}
			expanded = append(expanded, sql.SelectItem{
				Expr: &sql.ColumnRef{Table: col.Table, Column: col.Name},
			})
		}
	}
	p.items = expanded

	p.schema = &Schema{Columns: make([]ColumnInfo, 0, len(expanded))}
	for _, item := range expanded {
		eval, err := NewEvaluator(item.Expr, child.Schema())
		if err != nil {
			return nil, err
		}
		p.evals = append(p.evals, eval)

		info := ColumnInfo{Name: outputName(item)}
		if ref, ok := item.Expr.(*sql.ColumnRef); ok {
			if idx, err := child.Schema().Index(ref.Table, ref.Column); err == nil {
				info.Table = child.Schema().Columns[idx].Table
				info.Type = child.Schema().Columns[idx].Type
			}
		}
		p.schema.Columns = append(p.schema.Columns, info)
	}
	return p, nil
}

// outputName decides the header a select item gets.
func outputName(item sql.SelectItem) string {
	if item.Alias != "" {
		return item.Alias
	}
	if ref, ok := item.Expr.(*sql.ColumnRef); ok {
		return ref.Column
	}
	// An unnamed expression is labelled with its own text, which is what
	// PostgreSQL does for anything it cannot name.
	return item.Expr.String()
}

func (p *Project) Schema() *Schema { return p.schema }

func (p *Project) Next() (Row, error) {
	row, err := p.child.Next()
	if err != nil || row == nil {
		return nil, err
	}
	out := make(Row, len(p.evals))
	for i, eval := range p.evals {
		v, err := eval.Eval(row)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

func (p *Project) Close() error         { return p.child.Close() }
func (p *Project) Children() []Operator { return []Operator{p.child} }
func (p *Project) Explain() string {
	parts := make([]string, len(p.items))
	for i, item := range p.items {
		parts[i] = item.String()
	}
	return "Project: " + strings.Join(parts, ", ")
}

// --- sort ---

// Sort orders rows.
//
// It is a blocking operator: nothing can be returned until every input row has
// arrived, because the last row read might belong first. That is inherent to
// sorting, and it is why a query with ORDER BY has a memory cost proportional
// to its result -- and why an index that already provides the order is worth
// so much.
type Sort struct {
	child  Operator
	keys   []*Evaluator
	descs  []bool
	items  []sql.OrderByItem
	rows   []Row
	sorted bool
	pos    int
}

func NewSort(child Operator, items []sql.OrderByItem) (*Sort, error) {
	s := &Sort{child: child, items: items}
	for _, item := range items {
		eval, err := NewEvaluator(item.Expr, child.Schema())
		if err != nil {
			return nil, err
		}
		s.keys = append(s.keys, eval)
		s.descs = append(s.descs, item.Desc)
	}
	return s, nil
}

func (s *Sort) Schema() *Schema { return s.child.Schema() }

func (s *Sort) Next() (Row, error) {
	if !s.sorted {
		if err := s.buffer(); err != nil {
			return nil, err
		}
	}
	if s.pos >= len(s.rows) {
		return nil, nil
	}
	row := s.rows[s.pos]
	s.pos++
	return row, nil
}

func (s *Sort) buffer() error {
	for {
		row, err := s.child.Next()
		if err != nil {
			return err
		}
		if row == nil {
			break
		}
		s.rows = append(s.rows, row)
	}

	// Precompute the sort keys once per row instead of on every comparison.
	// A comparison sort makes O(n log n) comparisons, so evaluating the
	// expression inside the comparator would evaluate it n log n times.
	keyed := make([][]types.Value, len(s.rows))
	for i, row := range s.rows {
		keys := make([]types.Value, len(s.keys))
		for j, eval := range s.keys {
			v, err := eval.Eval(row)
			if err != nil {
				return err
			}
			keys[j] = v
		}
		keyed[i] = keys
	}

	indexes := make([]int, len(s.rows))
	for i := range indexes {
		indexes[i] = i
	}
	var sortErr error
	sort.SliceStable(indexes, func(a, b int) bool {
		ka, kb := keyed[indexes[a]], keyed[indexes[b]]
		for i := range ka {
			cmp, ok := types.Compare(ka[i], kb[i])
			if !ok {
				if sortErr == nil {
					sortErr = fmt.Errorf("cannot order by a mix of %s and %s", ka[i].Type, kb[i].Type)
				}
				return false
			}
			if cmp == 0 {
				continue
			}
			if s.descs[i] {
				return cmp > 0
			}
			return cmp < 0
		}
		return false
	})
	if sortErr != nil {
		return sortErr
	}

	ordered := make([]Row, len(s.rows))
	for i, idx := range indexes {
		ordered[i] = s.rows[idx]
	}
	s.rows = ordered
	s.sorted = true
	return nil
}

func (s *Sort) Close() error         { return s.child.Close() }
func (s *Sort) Children() []Operator { return []Operator{s.child} }
func (s *Sort) Explain() string {
	parts := make([]string, len(s.items))
	for i, item := range s.items {
		parts[i] = item.Expr.String()
		if item.Desc {
			parts[i] += " DESC"
		}
	}
	return "Sort: " + strings.Join(parts, ", ")
}

// --- limit and offset ---

// Limit stops after n rows, discarding the first offset of them.
type Limit struct {
	child   Operator
	limit   int64
	offset  int64
	skipped int64
	emitted int64
}

func NewLimit(child Operator, limit, offset int64) *Limit {
	return &Limit{child: child, limit: limit, offset: offset}
}

func (l *Limit) Schema() *Schema { return l.child.Schema() }

func (l *Limit) Next() (Row, error) {
	for l.skipped < l.offset {
		row, err := l.child.Next()
		if err != nil || row == nil {
			return nil, err
		}
		l.skipped++
	}
	// Stopping early is the point: LIMIT 10 over a million-row table should
	// read eleven rows, not a million, and the iterator model gives that for
	// free because nothing below was ever asked for more.
	if l.limit >= 0 && l.emitted >= l.limit {
		return nil, nil
	}
	row, err := l.child.Next()
	if err != nil || row == nil {
		return nil, err
	}
	l.emitted++
	return row, nil
}

func (l *Limit) Close() error         { return l.child.Close() }
func (l *Limit) Children() []Operator { return []Operator{l.child} }
func (l *Limit) Explain() string {
	if l.offset > 0 {
		return fmt.Sprintf("Limit: %d offset %d", l.limit, l.offset)
	}
	return fmt.Sprintf("Limit: %d", l.limit)
}

// --- distinct ---

// Distinct removes duplicate rows.
type Distinct struct {
	child Operator
	seen  map[string]struct{}
}

func NewDistinct(child Operator) *Distinct {
	return &Distinct{child: child, seen: make(map[string]struct{})}
}

func (d *Distinct) Schema() *Schema { return d.child.Schema() }

func (d *Distinct) Next() (Row, error) {
	for {
		row, err := d.child.Next()
		if err != nil || row == nil {
			return nil, err
		}
		// The encoded row doubles as a hash key, which avoids writing a
		// separate comparison for every type combination.
		key := string(types.EncodeRow(row))
		if _, dup := d.seen[key]; dup {
			continue
		}
		d.seen[key] = struct{}{}
		return row, nil
	}
}

func (d *Distinct) Close() error         { return d.child.Close() }
func (d *Distinct) Children() []Operator { return []Operator{d.child} }
func (d *Distinct) Explain() string      { return "Distinct" }

// --- nested loop join ---

// NestedLoopJoin joins two inputs by scanning the right side for every row of
// the left.
//
// It is O(n*m) and the simplest thing that is always correct: it works for any
// join condition, not just equality. A hash join would be far faster for equi-
// joins on large inputs, and choosing between them is the planner's job -- that
// choice is noted as future work rather than pretended at.
//
// The right side is materialised once rather than re-scanned, because
// re-opening a B+tree cursor per left row would multiply the I/O as well as
// the comparisons.
type NestedLoopJoin struct {
	left, right Operator
	joinType    sql.JoinType
	condition   *Evaluator
	condExpr    sql.Expression
	schema      *Schema

	rightRows   []Row
	buffered    bool
	currentLeft Row
	rightPos    int
	// matched tracks whether the current left row found any partner, which is
	// what a LEFT JOIN needs to know before emitting nulls.
	matched bool
}

func NewNestedLoopJoin(left, right Operator, joinType sql.JoinType, condition sql.Expression) (*NestedLoopJoin, error) {
	j := &NestedLoopJoin{left: left, right: right, joinType: joinType, condExpr: condition}

	j.schema = &Schema{}
	j.schema.Columns = append(j.schema.Columns, left.Schema().Columns...)
	j.schema.Columns = append(j.schema.Columns, right.Schema().Columns...)

	if condition != nil {
		eval, err := NewEvaluator(condition, j.schema)
		if err != nil {
			return nil, err
		}
		j.condition = eval
	}
	return j, nil
}

func (j *NestedLoopJoin) Schema() *Schema { return j.schema }

func (j *NestedLoopJoin) Next() (Row, error) {
	if !j.buffered {
		for {
			row, err := j.right.Next()
			if err != nil {
				return nil, err
			}
			if row == nil {
				break
			}
			j.rightRows = append(j.rightRows, row)
		}
		j.buffered = true
	}

	for {
		if j.currentLeft == nil {
			row, err := j.left.Next()
			if err != nil || row == nil {
				return nil, err
			}
			j.currentLeft = row
			j.rightPos = 0
			j.matched = false
		}

		for j.rightPos < len(j.rightRows) {
			right := j.rightRows[j.rightPos]
			j.rightPos++

			combined := make(Row, 0, len(j.currentLeft)+len(right))
			combined = append(combined, j.currentLeft...)
			combined = append(combined, right...)

			if j.condition == nil {
				j.matched = true
				return combined, nil
			}
			v, err := j.condition.Eval(combined)
			if err != nil {
				return nil, err
			}
			if !v.IsNull() && v.Truthy() {
				j.matched = true
				return combined, nil
			}
		}

		// The left row is exhausted. A LEFT JOIN still has to emit it, padded
		// with nulls, when nothing matched -- that is the whole difference
		// between LEFT and INNER.
		left := j.currentLeft
		j.currentLeft = nil
		if j.joinType == sql.JoinLeft && !j.matched {
			combined := make(Row, 0, len(left)+len(j.right.Schema().Columns))
			combined = append(combined, left...)
			for range j.right.Schema().Columns {
				combined = append(combined, types.Null())
			}
			return combined, nil
		}
	}
}

func (j *NestedLoopJoin) Close() error {
	if err := j.left.Close(); err != nil {
		return err
	}
	return j.right.Close()
}

func (j *NestedLoopJoin) Children() []Operator { return []Operator{j.left, j.right} }

func (j *NestedLoopJoin) Explain() string {
	if j.condExpr != nil {
		return fmt.Sprintf("NestedLoopJoin (%s) on %s", j.joinType, j.condExpr)
	}
	return fmt.Sprintf("NestedLoopJoin (%s)", j.joinType)
}

// --- values ---

// Values emits a fixed set of rows, for INSERT ... VALUES and for a SELECT
// with no FROM clause.
type Values struct {
	rows   []Row
	schema *Schema
	pos    int
}

func NewValues(rows []Row, schema *Schema) *Values {
	return &Values{rows: rows, schema: schema}
}

func (v *Values) Schema() *Schema { return v.schema }

func (v *Values) Next() (Row, error) {
	if v.pos >= len(v.rows) {
		return nil, nil
	}
	row := v.rows[v.pos]
	v.pos++
	return row, nil
}

func (v *Values) Close() error         { return nil }
func (v *Values) Children() []Operator { return nil }
func (v *Values) Explain() string      { return fmt.Sprintf("Values (%d rows)", len(v.rows)) }
