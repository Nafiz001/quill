package exec

import (
	"fmt"
	"strings"

	"github.com/Nafiz001/quill/internal/sql"
	"github.com/Nafiz001/quill/internal/types"
)

// Aggregation collapses many rows into one per group.
//
// Like sorting, it has to block: COUNT cannot be reported until the last row
// has been counted. Unlike sorting, it does not have to hold the rows -- only
// one accumulator per group -- so `SELECT country, COUNT(*) FROM users GROUP BY
// country` over a billion rows costs one entry per country, not a billion.
//
// The accumulators are kept in a hash map keyed by the encoded group values.
// A sort-based aggregation would avoid the map and produce sorted output for
// free, and is the better choice when the input already arrives ordered; that
// alternative is noted as future work rather than implemented twice.

// accumulator computes one aggregate over a stream of values.
type accumulator interface {
	// add folds in one value. A NULL is skipped by every aggregate except
	// COUNT(*), which counts rows rather than values -- that asymmetry is the
	// reason COUNT(*) and COUNT(col) can disagree.
	add(v types.Value) error
	result() types.Value
}

type countAcc struct {
	n int64
	// star counts rows regardless of nullness.
	star bool
	// distinct, when set, counts unique values only.
	seen map[string]struct{}
}

func (a *countAcc) add(v types.Value) error {
	if !a.star && v.IsNull() {
		return nil
	}
	if a.seen != nil {
		key := string(types.EncodeRow([]types.Value{v}))
		if _, dup := a.seen[key]; dup {
			return nil
		}
		a.seen[key] = struct{}{}
	}
	a.n++
	return nil
}

func (a *countAcc) result() types.Value { return types.NewInt(a.n) }

type sumAcc struct {
	intSum   int64
	floatSum float64
	// isFloat latches once any float arrives, so SUM over integers stays an
	// integer and does not silently gain a decimal point.
	isFloat bool
	any     bool
	seen    map[string]struct{}
}

func (a *sumAcc) add(v types.Value) error {
	if v.IsNull() {
		return nil
	}
	if a.seen != nil {
		key := string(types.EncodeRow([]types.Value{v}))
		if _, dup := a.seen[key]; dup {
			return nil
		}
		a.seen[key] = struct{}{}
	}
	switch v.Type {
	case types.TypeInt:
		if a.isFloat {
			a.floatSum += float64(v.Int)
		} else {
			a.intSum += v.Int
		}
	case types.TypeFloat:
		if !a.isFloat {
			a.floatSum = float64(a.intSum)
			a.isFloat = true
		}
		a.floatSum += v.Float
	default:
		return fmt.Errorf("SUM requires a numeric column, got %s", v.Type)
	}
	a.any = true
	return nil
}

func (a *sumAcc) result() types.Value {
	// SUM over no rows is NULL, not zero. It is the one place where the
	// intuitive answer and the standard disagree, and clients rely on the
	// standard's.
	if !a.any {
		return types.Null()
	}
	if a.isFloat {
		return types.NewFloat(a.floatSum)
	}
	return types.NewInt(a.intSum)
}

type avgAcc struct {
	sum   float64
	count int64
	seen  map[string]struct{}
}

func (a *avgAcc) add(v types.Value) error {
	if v.IsNull() {
		return nil
	}
	if a.seen != nil {
		key := string(types.EncodeRow([]types.Value{v}))
		if _, dup := a.seen[key]; dup {
			return nil
		}
		a.seen[key] = struct{}{}
	}
	switch v.Type {
	case types.TypeInt:
		a.sum += float64(v.Int)
	case types.TypeFloat:
		a.sum += v.Float
	default:
		return fmt.Errorf("AVG requires a numeric column, got %s", v.Type)
	}
	a.count++
	return nil
}

func (a *avgAcc) result() types.Value {
	if a.count == 0 {
		return types.Null()
	}
	// Always a float: the average of 1 and 2 is 1.5, and truncating it to
	// match the input type would be wrong.
	return types.NewFloat(a.sum / float64(a.count))
}

type minMaxAcc struct {
	best types.Value
	any  bool
	max  bool
}

func (a *minMaxAcc) add(v types.Value) error {
	if v.IsNull() {
		return nil
	}
	if !a.any {
		a.best = v
		a.any = true
		return nil
	}
	cmp, ok := types.Compare(v, a.best)
	if !ok {
		return fmt.Errorf("cannot compare %s with %s", v.Type, a.best.Type)
	}
	if (a.max && cmp > 0) || (!a.max && cmp < 0) {
		a.best = v
	}
	return nil
}

func (a *minMaxAcc) result() types.Value {
	if !a.any {
		return types.Null()
	}
	return a.best
}

func newAccumulator(call *sql.FunctionCall) (accumulator, error) {
	var seen map[string]struct{}
	if call.Distinct {
		seen = make(map[string]struct{})
	}
	switch strings.ToUpper(call.Name) {
	case "COUNT":
		return &countAcc{star: call.Star, seen: seen}, nil
	case "SUM":
		return &sumAcc{seen: seen}, nil
	case "AVG":
		return &avgAcc{seen: seen}, nil
	case "MIN":
		return &minMaxAcc{}, nil
	case "MAX":
		return &minMaxAcc{max: true}, nil
	default:
		return nil, fmt.Errorf("unknown aggregate function %s", call.Name)
	}
}

// group holds one group's accumulators and the values that identify it.
type group struct {
	keys []types.Value
	accs []accumulator
}

// Aggregate computes aggregates, optionally grouped.
type Aggregate struct {
	child      Operator
	groupBy    []*Evaluator
	groupExprs []sql.Expression
	calls      []*sql.FunctionCall
	argEvals   []*Evaluator
	schema     *Schema

	groups map[string]*group
	// order preserves the order groups were first seen, so output is stable
	// run to run rather than following Go's randomised map iteration.
	order    []string
	computed bool
	pos      int
	results  []Row
}

// NewAggregate builds an aggregation over child.
//
// calls are the aggregate function nodes found in the select list; the caller
// has already rewritten those nodes to reference the aggregate's output.
func NewAggregate(child Operator, groupBy []sql.Expression, calls []*sql.FunctionCall) (*Aggregate, error) {
	a := &Aggregate{
		child:      child,
		groupExprs: groupBy,
		calls:      calls,
		groups:     make(map[string]*group),
	}

	for _, expr := range groupBy {
		eval, err := NewEvaluator(expr, child.Schema())
		if err != nil {
			return nil, err
		}
		a.groupBy = append(a.groupBy, eval)
	}

	for _, call := range calls {
		if call.Star || len(call.Args) == 0 {
			a.argEvals = append(a.argEvals, nil)
			continue
		}
		eval, err := NewEvaluator(call.Args[0], child.Schema())
		if err != nil {
			return nil, err
		}
		a.argEvals = append(a.argEvals, eval)
	}

	// The output is the grouping expressions followed by the aggregates, named
	// positionally. The planner rewrites the select list to reference these
	// names, so they have to be predictable rather than derived from the
	// expression text -- two spellings of the same expression would otherwise
	// fail to line up.
	a.schema = &Schema{}
	for i, expr := range groupBy {
		info := ColumnInfo{Name: groupColumnName(i)}
		if ref, ok := expr.(*sql.ColumnRef); ok {
			if idx, err := child.Schema().Index(ref.Table, ref.Column); err == nil {
				// Carry the table qualifier through, so a projection above
				// this one can still be ordered by `t.col`. Dropping it here
				// is what made `GROUP BY u.name ORDER BY u.name` fail to
				// resolve.
				info.Table = child.Schema().Columns[idx].Table
				info.Type = child.Schema().Columns[idx].Type
			}
		}
		a.schema.Columns = append(a.schema.Columns, info)
	}
	for i := range calls {
		a.schema.Columns = append(a.schema.Columns, ColumnInfo{Name: aggregateColumnName(i)})
	}
	return a, nil
}

func (a *Aggregate) Schema() *Schema { return a.schema }

func (a *Aggregate) Next() (Row, error) {
	if !a.computed {
		if err := a.compute(); err != nil {
			return nil, err
		}
	}
	if a.pos >= len(a.results) {
		return nil, nil
	}
	row := a.results[a.pos]
	a.pos++
	return row, nil
}

func (a *Aggregate) compute() error {
	rows := 0
	for {
		row, err := a.child.Next()
		if err != nil {
			return err
		}
		if row == nil {
			break
		}
		rows++

		keys := make([]types.Value, len(a.groupBy))
		for i, eval := range a.groupBy {
			v, err := eval.Eval(row)
			if err != nil {
				return err
			}
			keys[i] = v
		}
		// The encoded group values double as the map key, which handles every
		// type and multi-column grouping without a custom hash.
		key := string(types.EncodeRow(keys))

		g, ok := a.groups[key]
		if !ok {
			g = &group{keys: keys}
			for _, call := range a.calls {
				acc, err := newAccumulator(call)
				if err != nil {
					return err
				}
				g.accs = append(g.accs, acc)
			}
			a.groups[key] = g
			a.order = append(a.order, key)
		}

		for i, acc := range g.accs {
			var v types.Value
			if a.argEvals[i] == nil {
				// COUNT(*) has no argument; any non-null stands in for "a row
				// happened".
				v = types.NewInt(1)
			} else {
				v, err = a.argEvals[i].Eval(row)
				if err != nil {
					return err
				}
			}
			if err := acc.add(v); err != nil {
				return err
			}
		}
	}

	// An aggregate with no GROUP BY over an empty input still produces one
	// row -- `SELECT COUNT(*) FROM empty` is 0, not no rows at all. With a
	// GROUP BY, an empty input produces no groups and therefore no rows.
	if len(a.groups) == 0 && len(a.groupBy) == 0 {
		out := make(Row, 0, len(a.calls))
		for _, call := range a.calls {
			acc, err := newAccumulator(call)
			if err != nil {
				return err
			}
			out = append(out, acc.result())
		}
		a.results = append(a.results, out)
		a.computed = true
		return nil
	}

	for _, key := range a.order {
		g := a.groups[key]
		out := make(Row, 0, len(g.keys)+len(g.accs))
		out = append(out, g.keys...)
		for _, acc := range g.accs {
			out = append(out, acc.result())
		}
		a.results = append(a.results, out)
	}
	a.computed = true
	return nil
}

func (a *Aggregate) Close() error         { return a.child.Close() }
func (a *Aggregate) Children() []Operator { return []Operator{a.child} }

func (a *Aggregate) Explain() string {
	var parts []string
	for _, call := range a.calls {
		parts = append(parts, call.String())
	}
	if len(a.groupExprs) == 0 {
		return "Aggregate: " + strings.Join(parts, ", ")
	}
	var groups []string
	for _, g := range a.groupExprs {
		groups = append(groups, g.String())
	}
	return fmt.Sprintf("GroupAggregate by %s: %s",
		strings.Join(groups, ", "), strings.Join(parts, ", "))
}

// collectAggregates finds every aggregate call in an expression tree.
//
// It is what tells the planner whether a query needs an aggregation step at
// all, and which accumulators to build.
func collectAggregates(expr sql.Expression, out *[]*sql.FunctionCall) {
	switch node := expr.(type) {
	case *sql.FunctionCall:
		if node.IsAggregate() {
			*out = append(*out, node)
			return
		}
		for _, a := range node.Args {
			collectAggregates(a, out)
		}
	case *sql.BinaryOp:
		collectAggregates(node.Left, out)
		collectAggregates(node.Right, out)
	case *sql.UnaryOp:
		collectAggregates(node.Expr, out)
	case *sql.IsNull:
		collectAggregates(node.Expr, out)
	case *sql.InList:
		collectAggregates(node.Expr, out)
		for _, v := range node.Values {
			collectAggregates(v, out)
		}
	case *sql.Between:
		collectAggregates(node.Expr, out)
		collectAggregates(node.Low, out)
		collectAggregates(node.High, out)
	}
}
