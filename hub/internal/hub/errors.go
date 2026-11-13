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
	// ErrHoldFull is a prompt refused because the session already has as
	// many held prompts as the hub keeps.
	ErrHoldFull = errors.New("too many messages waiting for the backend")
	// ErrShuttingDown is a prompt the hub could not hold because it is
	// shutting down.
	ErrShuttingDown = errors.New("hub shutting down")
	// ErrBackendAway is a request that needs the backend at once while it
	// restarts or cannot be reached.
	ErrBackendAway = errors.New("backend away")
	// ErrNoModel is a new chat on a Hermes that has no model set up.
	ErrNoModel = errors.New("no model configured")
)
