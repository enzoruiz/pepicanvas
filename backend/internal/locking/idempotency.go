package locking

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"time"
)

// ErrOperationConflict reports an operation ID reused for a different
// intent. A new intent requires a new operation ID.
var ErrOperationConflict = errors.New("locking: operation id reused with a different intent")

// Retention bounds how long and how many answers each account keeps per
// tenant. Reusing an operation ID after its answer expires starts a new
// operation.
type Retention struct {
	TTL           time.Duration
	MaxOperations int
}

// Default retention applied when a Retention field is zero.
const (
	DefaultRetentionTTL           = 10 * time.Minute
	DefaultRetentionMaxOperations = 4096
)

// scope keys retained answers by account, so a retry over a reconnected
// session finds the answer its previous session got.
type scope struct {
	tenantID  string
	accountID string
}

type kind string

const (
	kindAcquire kind = "acquire"
	kindRelease kind = "release"
)

type fingerprint [sha256.Size]byte

// fingerprintOf identifies the intent of a request. The session and the
// correlation are excluded so a retry over a new connection stays
// equivalent.
func fingerprintOf(k kind, request Request) fingerprint {
	hash := sha256.New()
	for _, part := range []string{string(k), request.CanvasID, request.ObjectID, string(request.Reason)} {
		_ = binary.Write(hash, binary.BigEndian, uint64(len(part)))
		hash.Write([]byte(part))
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

// operations retains one account's answers in answer order, so expiry and
// the count bound both evict from the front.
type operations struct {
	byID  map[string]*retained
	order []*retained
}

// lookupLocked returns the retained answer for an equivalent retry, or a
// conflict.
func (m *Manager) lookupLocked(s scope, k kind, request Request, now time.Time) (Result, bool, error) {
	ops := m.operations[s]
	if ops == nil {
		return Result{}, false, nil
	}
	m.expireOperationsLocked(s, ops, now)
	record, ok := ops.byID[request.OperationID]
	if !ok {
		return Result{}, false, nil
	}
	if record.fingerprint != fingerprintOf(k, request) {
		return Result{}, false, ErrOperationConflict
	}
	return record.result, true, nil
}

func (m *Manager) rememberLocked(s scope, k kind, request Request, result Result, now time.Time) {
	ops := m.operations[s]
	if ops == nil {
		ops = &operations{byID: map[string]*retained{}}
		m.operations[s] = ops
	}
	record := &retained{
		operationID: request.OperationID,
		fingerprint: fingerprintOf(k, request),
		result:      result,
		expires:     now.Add(m.config.Retention.TTL),
	}
	ops.byID[record.operationID] = record
	ops.order = append(ops.order, record)
	if overflow := len(ops.order) - m.config.Retention.MaxOperations; overflow > 0 {
		ops.evict(overflow)
	}
}

func (m *Manager) expireOperationsLocked(s scope, ops *operations, now time.Time) {
	count := 0
	for count < len(ops.order) && !now.Before(ops.order[count].expires) {
		count++
	}
	ops.evict(count)
	if len(ops.order) == 0 {
		delete(m.operations, s)
	}
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
