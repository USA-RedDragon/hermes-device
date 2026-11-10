package hub

import "errors"

// Errors returned by Service implementations.
var (
	ErrForbidden       = errors.New("backend not allowed for user")
	ErrUnknownBackend  = errors.New("unknown backend")
	ErrUnknownApproval = errors.New("unknown approval")
	ErrUnknownInput    = errors.New("unknown input")
)
