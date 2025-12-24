package tunnel

import "errors"

// AlreadyConnectedError indicates the user already has an active session on the server.
type AlreadyConnectedError struct {
	Message string
}

func (e *AlreadyConnectedError) Error() string {
	return e.Message
}

// IsAlreadyConnectedError checks if an error is an AlreadyConnectedError.
func IsAlreadyConnectedError(err error) bool {
	var acErr *AlreadyConnectedError
	return errors.As(err, &acErr)
}
