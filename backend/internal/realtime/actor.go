package realtime

import "sync"

// actor owns the order and state of one canvas. Only its goroutine mutates
// them; mu lets Events read the replay log concurrently.
type actor struct {
	config Config
	inbox  chan request

	mu       sync.Mutex
	state    *State
	sequence uint64
	log      []Event
}

func (a *actor) run(stop <-chan struct{}) {
	for {
		select {
		case <-stop:
			return
		case next := <-a.inbox:
			result, err := a.handle(next)
			next.reply <- reply{result: result, err: err}
		}
	}
}

func (a *actor) handle(next request) (Result, error) {
	command := next.command
	if err := a.config.Authorize(next.ctx, command); err != nil {
		return Result{}, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	result := Result{OperationID: command.OperationID, CorrelationID: command.CorrelationID}
	if reason := a.state.Decide(command); reason != "" {
		result.Outcome, result.Reason = OutcomeRejected, reason
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
	return result, nil
}

func (a *actor) events(after uint64) ([]Event, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.log) > 0 && after+1 < a.log[0].Sequence {
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
