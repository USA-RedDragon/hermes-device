package hub

import (
	"errors"
	"maps"
	"slices"
	"strings"
)

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

// UnreachableError comes with a session list that misses backends: their
// list call failed. Backends maps each such backend to why. The rows that
// come with it hold the backends that answered, plus the last list of a
// failed backend when the hub still has one; Stale names those backends.
// A caller must never take the rows for a failed backend as its whole list.
// Pending names the backends whose rows are the hub's copy because their
// fresh list is still on its way (see WithCached); they are not in
// Backends until they have been away long enough to tell the user.
type UnreachableError struct {
	Backends map[string]string
	Stale    map[string]bool
	Pending  map[string]bool
	// Down names the backends in Backends that are down, not restarting:
	// away for the whole restart budget, or failing in a way a restart
	// does not explain. The others get a quiet note.
	Down map[string]bool
}

func (e *UnreachableError) Error() string {
	ids := slices.Sorted(maps.Keys(e.Backends))
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, id+": "+e.Backends[id])
	}
	return "cannot list the chats of " + strings.Join(parts, "; ")
}

// Unreachable returns the UnreachableError in err, or nil.
func Unreachable(err error) *UnreachableError {
	var ue *UnreachableError
	if errors.As(err, &ue) {
		return ue
	}
	return nil
}
