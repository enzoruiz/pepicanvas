package revocation

import (
	"errors"
	"slices"
	"time"
)

// ErrOperationConflict reports an operation ID reused for a different cut.
// A new cut requires a new operation ID.
var ErrOperationConflict = errors.New("revocation: operation id reused with a different cut")

// Retention bounds how long and how many cut results a tenant keeps.
// Reusing an operation ID after its result expires starts a new cut.
type Retention struct {
	TTL           time.Duration
	MaxOperations int
}

// Default retention applied when a Retention field is zero.
const (
	DefaultRetentionTTL           = 10 * time.Minute
	DefaultRetentionMaxOperations = 4096
)

// intent identifies a cut; the correlation ID is excluded so a retry over
// a new connection stays equivalent.
type intent struct {
	kind    Type
	target  Target
	version uint64
}

func intentOf(cut Cut) intent {
	return intent{kind: cut.Type, target: cut.Target, version: cut.Version}
}

// operation is one cut in flight or completed. Waiters block on done.
type operation struct {
	id      string
	intent  intent
	expires time.Time
	done    chan struct{}
	report  Report
	err     error
}

// answer returns a copy of the retained result, so callers cannot alter it.
func (op *operation) answer() (Report, error) {
	report := op.report
	report.Closures = slices.Clone(report.Closures)
	return report, op.err
}

// operations retains cut results in arrival order, so expiry and the count
// bound both evict from the front.
type operations struct {
	retention Retention
	byID      map[string]*operation
	order     []*operation
}

func newOperations(retention Retention) *operations {
	return &operations{retention: retention, byID: map[string]*operation{}}
}

// start returns the operation already retained for an equivalent cut, or
// registers a new one and reports that the caller must run it.
func (o *operations) start(cut Cut, now time.Time) (*operation, bool, error) {
	o.expire(now)
	if existing, ok := o.byID[cut.OperationID]; ok {
		if existing.intent != intentOf(cut) {
			return nil, false, ErrOperationConflict
		}
		return existing, false, nil
	}
	op := &operation{
		id:      cut.OperationID,
		intent:  intentOf(cut),
		expires: now.Add(o.retention.TTL),
		done:    make(chan struct{}),
	}
	o.byID[op.id] = op
	o.order = append(o.order, op)
	if overflow := len(o.order) - o.retention.MaxOperations; overflow > 0 {
		o.evict(overflow)
	}
	return op, true, nil
}

func (o *operations) expire(now time.Time) {
	count := 0
	for count < len(o.order) && !now.Before(o.order[count].expires) {
		count++
	}
	o.evict(count)
}

func (o *operations) evict(count int) {
	if count == 0 {
		return
	}
	for _, op := range o.order[:count] {
		delete(o.byID, op.id)
	}
	o.order = append(o.order[:0:0], o.order[count:]...)
}
