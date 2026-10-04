package access

// RequestLoader fetches the current facts for an authorization decision.
type RequestLoader func() (Request, error)

// Callback performs an operation only after its current facts are authorized.
type Callback func() error

// GuardMutation reauthorizes immediately before invoking a mutation.
func GuardMutation(load RequestLoader, mutate Callback) error {
	return guard(load, mutate)
}

// GuardReadDelivery reauthorizes immediately before delivering read data.
func GuardReadDelivery(load RequestLoader, deliver Callback) error {
	return guard(load, deliver)
}

// GuardEventDelivery reauthorizes immediately before delivering an event.
func GuardEventDelivery(load RequestLoader, deliver Callback) error {
	return guard(load, deliver)
}

func guard(load RequestLoader, callback Callback) error {
	if load == nil || callback == nil {
		return ErrAccessDenied
	}

	request, err := load()
	if err != nil {
		return ErrAccessDenied
	}
	if err := Check(request); err != nil {
		return err
	}

	return callback()
}
