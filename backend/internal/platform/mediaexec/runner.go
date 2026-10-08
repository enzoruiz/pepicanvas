package mediaexec

import (
	"context"
	"io"
	"reflect"
	"sync"

	"github.com/enzoruiz/pepicanvas/backend/internal/media"
)

// FailureKind is the closed vocabulary returned by contained execution.
type FailureKind uint8

const (
	FailureContainmentUnavailable FailureKind = iota + 1
	FailureContainmentSetup
	FailureContainmentCleanup
	FailureTimeout
	FailureCancelled
	FailureMemoryLimit
	FailureTaskLimit
	FailureCPULimit
	FailureToolExit
	FailureInternal
)

type resultState uint8

const (
	resultSuccess resultState = iota + 1
	resultFailure
)

// Result reports one fixed execution outcome without carrying private values.
// Its zero value and unknown failure kinds are invalid and must fail closed.
type Result struct {
	state   resultState
	failure FailureKind
}

// SuccessfulResult constructs the only valid successful result.
func SuccessfulResult() Result {
	return Result{state: resultSuccess}
}

// FailedResult constructs a failed result. Consumers still validate kind so an
// unknown value supplied by a faulty implementation fails closed.
func FailedResult(kind FailureKind) Result {
	return Result{state: resultFailure, failure: kind}
}

func (result Result) valid() bool {
	switch result.state {
	case resultSuccess:
		return result.failure == 0
	case resultFailure:
		return result.failure >= FailureContainmentUnavailable && result.failure <= FailureInternal
	default:
		return false
	}
}

func (result Result) succeeded() bool {
	return result.valid() && result.state == resultSuccess
}

func (result Result) failureKind() FailureKind {
	if !result.valid() || result.state != resultFailure {
		return 0
	}
	return result.failure
}

// Runner begins one inspection-scoped containment lifecycle. The context and
// limits apply to every sequential command run through the returned Execution.
type Runner interface {
	Begin(context.Context, media.Limits) (Execution, Result)
}

// Execution runs sequential shell-free commands in one inspection containment
// state and releases that state through Close.
type Execution interface {
	Run(Command, io.Writer, io.Writer) Result
	Close() Result
}

type guardedExecution struct {
	mu     sync.Mutex
	raw    Execution
	closed bool
}

func guardExecution(raw Execution) (*guardedExecution, bool) {
	if nilInterface(raw) {
		return nil, false
	}
	return &guardedExecution{raw: raw}, true
}

func (execution *guardedExecution) Run(command Command, stdout, stderr io.Writer) Result {
	execution.mu.Lock()
	defer execution.mu.Unlock()
	if execution.closed {
		return FailedResult(FailureInternal)
	}
	result := execution.raw.Run(command, stdout, stderr)
	if !result.valid() {
		return FailedResult(FailureInternal)
	}
	return result
}

func (execution *guardedExecution) Close() Result {
	execution.mu.Lock()
	defer execution.mu.Unlock()
	if execution.closed {
		return FailedResult(FailureInternal)
	}
	execution.closed = true
	result := execution.raw.Close()
	if !result.valid() {
		return FailedResult(FailureInternal)
	}
	return result
}

func nilInterface(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}
