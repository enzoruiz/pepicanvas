package realtime

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"time"
)

// ErrOperationConflict reports an operation ID reused for a different intent.
// A new intent requires a new operation ID.
var ErrOperationConflict = errors.New("realtime: operation id reused with a different intent")

// Retention bounds how long and how many operation results a canvas keeps.
// Reusing an operation ID after its result expires starts a new operation.
type Retention struct {
	TTL           time.Duration
	MaxOperations int
}

// Default retention applied when a Retention field is zero.
const (
	DefaultRetentionTTL           = 10 * time.Minute
	DefaultRetentionMaxOperations = 4096
)

type fingerprint [sha256.Size]byte

// fingerprintOf identifies the intent of a command. The correlation ID is
// excluded so a retry over a new connection stays equivalent.
func fingerprintOf(command Command) fingerprint {
	hash := sha256.New()
	for _, part := range [][]byte{[]byte(command.Kind), []byte(command.ObjectID), command.Payload} {
		_ = binary.Write(hash, binary.BigEndian, uint64(len(part)))
		hash.Write(part)
	}
	var sum fingerprint
	hash.Sum(sum[:0])
	return sum
}

type retained struct {
	operationID string
	fingerprint fingerprint
	result      Result
	expires     time.Time
}

// operations retains results in acceptance order, so expiry and the count
// bound both evict from the front.
type operations struct {
	retention Retention
	byID      map[string]*retained
	order     []*retained
}

func newOperations(retention Retention) *operations {
	return &operations{retention: retention, byID: map[string]*retained{}}
}

// lookup returns the retained result for an equivalent retry, or a conflict.
func (o *operations) lookup(command Command, now time.Time) (Result, bool, error) {
	o.expire(now)
	record, ok := o.byID[command.OperationID]
	if !ok {
		return Result{}, false, nil
	}
	if record.fingerprint != fingerprintOf(command) {
		return Result{}, false, ErrOperationConflict
	}
	return record.result, true, nil
}

func (o *operations) remember(command Command, result Result, now time.Time) {
	record := &retained{
		operationID: command.OperationID,
		fingerprint: fingerprintOf(command),
		result:      result,
		expires:     now.Add(o.retention.TTL),
	}
	o.byID[record.operationID] = record
	o.order = append(o.order, record)
	if overflow := len(o.order) - o.retention.MaxOperations; overflow > 0 {
		o.evict(overflow)
	}
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
	for _, record := range o.order[:count] {
		delete(o.byID, record.operationID)
	}
	o.order = append(o.order[:0:0], o.order[count:]...)
}
