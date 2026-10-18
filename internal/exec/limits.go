package exec

import (
	"errors"
	"time"
)

// ErrStatementTimeout is returned when a statement runs past the time it was
// allowed. The wire protocol reports it as an ordinary error and the session
// carries on.
var ErrStatementTimeout = errors.New("canceling statement due to statement timeout")

// ErrResultTooLarge is returned when a query would hand back more rows than
// the server is willing to hold in memory.
var ErrResultTooLarge = errors.New("the result has too many rows; add a LIMIT or a WHERE")

// Limits bound what one statement may cost. They exist for a database that
// strangers can query: results are built in memory and a join is a nested
// loop, so a single careless self-join would otherwise stall every other
// connection, because statements run one at a time.
//
// The zero value means no limits.
type Limits struct {
	// Timeout is how long one statement may run. Zero means unlimited.
	Timeout time.Duration
	// MaxRows is how many rows a query may return. Zero means unlimited.
	MaxRows int

	deadline time.Time
	ticks    uint32
}

// begin starts the clock for a statement.
func (l *Limits) begin() {
	if l.Timeout > 0 {
		l.deadline = time.Now().Add(l.Timeout)
	} else {
		l.deadline = time.Time{}
	}
}

// tick is called for every row an operator touches. Reading the clock that
// often would cost more than the work, so it looks at it every 1,024th call.
func (l *Limits) tick() error {
	if l == nil || l.deadline.IsZero() {
		return nil
	}
	l.ticks++
	if l.ticks&0x3ff == 0 && time.Now().After(l.deadline) {
		return ErrStatementTimeout
	}
	return nil
}
