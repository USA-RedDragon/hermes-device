package hub

import (
	"context"
	"encoding/json"
	"time"
)

// ViewCache keeps the hub's last copy of what each backend showed: its
// chat list (key "") and the newest rows of each chat (key "s:" and the
// session ID). A page that opens while the backend restarts gets this copy
// at once, on any replica, and the fresh one when the backend answers.
type ViewCache interface {
	SaveView(ctx context.Context, backendID, key string, data []byte, at time.Time) error
	View(ctx context.Context, backendID, key string) (data []byte, at time.Time, ok bool, err error)
}

// ViewListKey is the ViewCache key of a backend's chat list.
const ViewListKey = ""

// ViewSnapshotKey is the ViewCache key of a chat's snapshot.
func ViewSnapshotKey(sessionID string) string { return "s:" + sessionID }

type cachedKey struct{}

// WithCached lets Sessions and Attach answer with the hub's last copy when
// the backend is slow to answer, for a page that shows it until the fresh
// one comes. Without it they wait for the backend as before.
func WithCached(ctx context.Context) context.Context {
	return context.WithValue(ctx, cachedKey{}, true)
}

// Cached reports whether WithCached is set on ctx.
func Cached(ctx context.Context) bool {
	v, _ := ctx.Value(cachedKey{}).(bool)
	return v
}

// The newest rows of a snapshot: back from the end until HeadRows rows or
// HeadBytes bytes, then back to the user message that starts the turn.
const (
	HeadRows  = 40
	HeadBytes = 96 << 10
)

const (
	roleUser      = "user"
	roleAssistant = "assistant"
)

// HeadCut is where the newest rows of msgs start (see HeadRows): 0 when
// they are all of them.
func HeadCut(msgs []json.RawMessage) int {
	i, size := len(msgs), 0
	for i > 0 && len(msgs)-i < HeadRows && size < HeadBytes {
		i--
		size += len(msgs[i])
	}
	// A long turn (hundreds of tool calls) is cut at an assistant row, whose
	// tool calls name the tool rows after it, rather than sent whole.
	at, firstAssistant := i, -1
	for i > 0 && size < 4*HeadBytes {
		r := rowRole(msgs[i])
		if r == roleUser {
			return i
		}
		if r == roleAssistant && firstAssistant < 0 {
			firstAssistant = i
		}
		i--
		size += len(msgs[i])
	}
	if i == 0 {
		return 0
	}
	if firstAssistant >= 0 {
		return firstAssistant
	}
	return at
}

func rowRole(m json.RawMessage) string {
	var r struct {
		Role string `json:"role"`
	}
	_ = json.Unmarshal(m, &r)
	return r.Role
}
