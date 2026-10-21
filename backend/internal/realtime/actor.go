package realtime

import (
	"context"
	"sync"
)

// actor owns the order and state of one canvas. Only its goroutine mutates
// them; mu lets Events read the replay log concurrently.
type actor struct {
	config Config
	inbox  chan request

	mu       sync.Mutex
	state    *State
	ops      *operations
	sequence uint64
	log      []Event
	// resync makes every replay at or below floor require resync.
	resync bool
	floor  uint64
}

// markResync sets the resync floor at the current sequence.
func (a *actor) markResync() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.resync, a.floor = true, a.sequence
}

// run serves the inbox until Close closes it. After abort, remaining
// requests are answered with ErrClosed without being applied.
func (a *actor) run(abort context.Context) {
	for next := range a.inbox {
		if abort.Err() != nil {
			next.reply <- reply{err: ErrClosed}
			continue
		}
		result, err := a.handle(abort, next)
		next.reply <- reply{result: result, err: err}
	}
}

func (a *actor) handle(abort context.Context, next request) (Result, error) {
	command := next.command
	ctx, cancel := context.WithCancel(next.ctx)
	defer cancel()
	stop := context.AfterFunc(abort, cancel)
	defer stop()
	if err := a.config.Authorize(ctx, command); err != nil {
		return Result{}, err
	}
	if abort.Err() != nil {
		return Result{}, ErrClosed
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.config.Now()
	if previous, ok, err := a.ops.lookup(command, now); err != nil || ok {
		return previous, err
	}
	result := Result{OperationID: command.OperationID, CorrelationID: command.CorrelationID}
	if reason := a.state.Decide(command); reason != "" {
		result.Outcome, result.Reason = OutcomeRejected, reason
		a.ops.remember(command, result, now)
		return result, nil
	}
	a.sequence++
	event := command.event(a.sequence)
	a.state.Apply(event)
	a.log = append(a.log, event)
	if overflow := len(a.log) - a.config.LogSize; overflow > 0 {
		a.log = append(a.log[:0:0], a.log[overflow:]...)
	}
	result.Outcome, result.Sequence = OutcomeAccepted, a.sequence
	a.ops.remember(command, result, now)
	return result, nil
}

func (a *actor) events(after uint64) ([]Event, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if (a.resync && after <= a.floor) || (len(a.log) > 0 && after+1 < a.log[0].Sequence) {
		return nil, ErrResyncRequired
	}
	var events []Event
	for _, event := range a.log {
		if event.Sequence > after {
			event.Payload = clonePayload(event.Payload)
			events = append(events, event)
		}
	}
	return events, nil
}
