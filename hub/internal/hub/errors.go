package hub

import "errors"

// Errors returned by Service implementations.
var (
	ErrForbidden       = errors.New("backend not allowed for user")
	ErrUnknownBackend  = errors.New("unknown backend")
	ErrUnknownApproval = errors.New("unknown approval")
	ErrUnknownInput    = errors.New("unknown input")
	ErrUnknownSession  = errors.New("unknown session")
	// ErrBusy is a change refused while the session runs or has inputs
	// waiting.
	ErrBusy = errors.New("session busy")
	// ErrBadValue is a change the backend refused as invalid.
	ErrBadValue = errors.New("invalid value")
	// ErrTitleInUse is a rename to a title another session holds.
	ErrTitleInUse = errors.New("title in use")
)
