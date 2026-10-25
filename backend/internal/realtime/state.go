package realtime

// Object is the converged state of one canvas object.
type Object struct {
	Payload  []byte
	Deleted  bool
	Sequence uint64
}

// State applies the canvas conflict rules: the last accepted command wins,
// and a delete wins over every later command for the same object. The actor
// and every replica use the same rules, so they converge on the same events.
type State struct {
	objects  map[string]Object
	sequence uint64
}

// NewState returns an empty canvas state.
func NewState() *State {
	return &State{objects: map[string]Object{}}
}

// Decide returns the rejection reason for a command, or "" when it can be accepted.
func (s *State) Decide(command Command) Reason {
	if command.Kind == KindUsage {
		return ""
	}
	if object, ok := s.objects[command.ObjectID]; ok && object.Deleted {
		return ReasonObjectDeleted
	}
	return ""
}

// Apply folds one accepted event into the state. A usage event only
// advances the sequence.
func (s *State) Apply(event Event) {
	if event.Sequence > s.sequence {
		s.sequence = event.Sequence
	}
	if event.Kind == KindUsage {
		return
	}
	if current, ok := s.objects[event.ObjectID]; ok && current.Deleted {
		return
	}
	object := Object{Sequence: event.Sequence}
	if event.Kind == KindDelete {
		object.Deleted = true
	} else {
		object.Payload = clonePayload(event.Payload)
	}
	s.objects[event.ObjectID] = object
}

// Object returns the state of one object and whether it was ever addressed.
func (s *State) Object(objectID string) (Object, bool) {
	object, ok := s.objects[objectID]
	object.Payload = clonePayload(object.Payload)
	return object, ok
}

// Sequence returns the highest applied sequence.
func (s *State) Sequence() uint64 {
	return s.sequence
}

// Objects returns a copy of every addressed object keyed by object ID.
func (s *State) Objects() map[string]Object {
	objects := make(map[string]Object, len(s.objects))
	for id := range s.objects {
		objects[id], _ = s.Object(id)
	}
	return objects
}
