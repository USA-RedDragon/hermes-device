package splice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/USA-RedDragon/hermes-device/internal/hubapi"
	"github.com/coder/websocket"
)

// ReasonNotFound is the data.reason of a resume whose session the splice
// could not find (docs/protocol.md 5.6).
const ReasonNotFound = "hermes_device_not_found"

const (
	// lookupTimeout bounds each call of a lookup.
	lookupTimeout = 10 * time.Second
	// maxProbes bounds the sessions a latest lookup checks: the listings
	// hold every session of the user, of every surface.
	maxProbes = 200
	listLimit = 200
	// controlLookupTimeout bounds a lookup of /control/lookup.
	controlLookupTimeout = 2 * time.Minute
)

// ErrNoSession is a lookup that found no session.
var ErrNoSession = errors.New("no session found")

// LookupError is a lookup that failed. HubError is the hub's error code,
// as in a bind error (docs/protocol.md 5.6).
type LookupError struct {
	HubError string
	Err      error
}

func (e *LookupError) Error() string { return "session lookup failed: " + e.Err.Error() }

func (e *LookupError) Unwrap() error { return e.Err }

// caller sends a request of the splice's own on a gateway leg and waits
// for its answer.
type caller interface {
	call(method string, params map[string]json.RawMessage, timeout time.Duration) (frame, error)
}

// resolveName returns the id of the session name names, as the gateway's
// session.resume resolves it: a session id first, then a title (the
// latest continuation of that title), projected to its compression tip.
// session.list with a title runs that resolver without attaching.
func resolveName(c caller, name string) (string, error) {
	if name == "" {
		return "", ErrNoSession
	}
	rows, err := listSessions(c, map[string]json.RawMessage{"title": mustRaw(name)})
	if err != nil {
		return "", err
	}
	if len(rows) == 0 {
		return "", ErrNoSession
	}
	return rows[0], nil
}

// resolveLatest returns the most recent session bound to this device: the
// first row of the stored listing (closed sessions, most recently active
// first) and then of the live listing that the hub lets through as bound
// to this device.
func resolveLatest(c caller) (string, error) {
	seen := map[string]bool{}
	probes := 0
	for _, params := range []map[string]json.RawMessage{
		{"scope": mustRaw("stored"), "limit": mustRaw(listLimit)},
		{"limit": mustRaw(listLimit)},
	} {
		rows, err := listSessions(c, params)
		if err != nil {
			return "", err
		}
		for _, id := range rows {
			if seen[id] {
				continue
			}
			seen[id] = true
			if probes == maxProbes {
				return "", ErrNoSession
			}
			probes++
			mine, err := boundHere(c, id)
			if err != nil {
				return "", err
			}
			if mine {
				return id, nil
			}
		}
	}
	return "", ErrNoSession
}

// listSessions runs session.list and returns the session ids of its rows
// in order. A gateway error answer counts as no rows.
func listSessions(c caller, params map[string]json.RawMessage) ([]string, error) {
	f, err := c.call("session.list", params, lookupTimeout)
	if err != nil {
		return nil, &LookupError{HubError: hubUnreachable, Err: err}
	}
	if isError(&f) {
		return nil, nil
	}
	return listRows(f.Result), nil
}

// listRows returns the session ids of a session.list result's rows.
func listRows(result json.RawMessage) []string {
	var res struct {
		Sessions []struct {
			ResolvedID string `json:"resolved_id"`
			SessionID  string `json:"session_id"`
			ID         string `json:"id"`
		} `json:"sessions"`
	}
	if json.Unmarshal(result, &res) != nil {
		return nil
	}
	ids := make([]string, 0, len(res.Sessions))
	for _, r := range res.Sessions {
		for _, id := range []string{r.ResolvedID, r.SessionID, r.ID} {
			if id != "" {
				ids = append(ids, id)
				break
			}
		}
	}
	return ids
}

// boundHere asks the hub whether sid is bound to this device. The hub
// checks the session_id of every request before it forwards it, so a
// session.info is refused with hermes_device_not_bound or
// hermes_device_other_device unless the session is this device's. The
// extra member makes the gateway answer invalid_params at once instead of
// building the session's info.
func boundHere(c caller, sid string) (bool, error) {
	f, err := c.call("session.info", map[string]json.RawMessage{
		keySessionID: mustRaw(sid), "hermes_device_probe": json.RawMessage("true"),
	}, lookupTimeout)
	if err != nil {
		return false, &LookupError{HubError: hubUnreachable, Err: err}
	}
	if !isError(&f) {
		return true, nil
	}
	var e struct {
		Message string         `json:"message"`
		Data    map[string]any `json:"data"`
	}
	_ = json.Unmarshal(f.Error, &e)
	switch e.Data[keyReason] {
	case "hermes_device_not_bound", "hermes_device_other_device":
		return false, nil
	case ReasonBind:
		code, _ := e.Data["hub_error"].(string)
		if code == "" {
			code = hubUnreachable
		}
		return false, &LookupError{HubError: code, Err: errors.New(e.Message)}
	}
	return true, nil
}

func notFoundAnswer(id json.RawMessage, message string) []byte {
	return answerError(id, CodeDevice, message, map[string]any{keyReason: ReasonNotFound})
}

func lookupAnswer(id json.RawMessage, err error) []byte {
	code := hubUnreachable
	var le *LookupError
	if errors.As(err, &le) && le.HubError != "" {
		code = le.HubError
	}
	return answerError(id, CodeDevice, "talaria could not look up the session: "+err.Error(),
		map[string]any{keyReason: ReasonBind, "hub_error": code})
}

// sendCall writes a request of the splice's own with id on ws and waits for
// its answer on ch.
func sendCall(ctx context.Context, ws *websocket.Conn, id, method string, params map[string]json.RawMessage,
	ch <-chan frame, gone <-chan struct{},
) (frame, error) {
	msg, err := json.Marshal(map[string]any{"jsonrpc": jsonRPCVersion, "id": id, "method": method, "params": params})
	if err != nil {
		return frame{}, err
	}
	if err := ws.Write(ctx, websocket.MessageText, msg); err != nil {
		return frame{}, fmt.Errorf("%s: %w", method, err)
	}
	select {
	case f := <-ch:
		return f, nil
	case <-gone:
		return frame{}, fmt.Errorf("%s: gateway leg lost", method)
	case <-ctx.Done():
		return frame{}, fmt.Errorf("%s: %w", method, ctx.Err())
	}
}

// lookupLeg is a gateway leg of its own that the daemon opens for a
// lookup of chat (section 5.5) and closes after it.
type lookupLeg struct {
	ws   *websocket.Conn
	gone chan struct{}

	mu   sync.Mutex
	own  map[string]chan frame
	next uint64
}

func (l *lookupLeg) read(ctx context.Context) {
	defer close(l.gone)
	for {
		_, data, err := l.ws.Read(ctx)
		if err != nil {
			return
		}
		f, err := parseFrame(data)
		if err != nil || f.Method != "" {
			continue
		}
		id, ok := ownID(f.ID)
		if !ok {
			continue
		}
		l.mu.Lock()
		ch := l.own[id]
		delete(l.own, id)
		l.mu.Unlock()
		if ch != nil {
			ch <- f
		}
	}
}

func (l *lookupLeg) call(method string, params map[string]json.RawMessage, timeout time.Duration) (frame, error) {
	ch := make(chan frame, 1)
	l.mu.Lock()
	l.next++
	id := fmt.Sprintf("%slookup-%d", OwnIDPrefix, l.next)
	l.own[id] = ch
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		delete(l.own, id)
		l.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return sendCall(ctx, l.ws, id, method, params, ch, l.gone)
}

// Lookup returns the id of the session name names (a session id or a
// title), or of the most recent session bound to this device when name is
// empty, over a gateway leg of its own. It returns ErrNoSession when there
// is none.
func (s *Server) Lookup(ctx context.Context, name string) (string, error) {
	ctx, cancelAll := context.WithTimeout(ctx, controlLookupTimeout)
	defer cancelAll()
	dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	ws, err := s.opts.Hub.DialLeg(dialCtx, hubapi.PathGatewayLeg)
	cancel()
	if err != nil {
		return "", &LookupError{HubError: hubErrorCode(err), Err: err}
	}
	ws.SetReadLimit(maxFrame)
	ctx, stop := context.WithCancel(ctx)
	l := &lookupLeg{ws: ws, gone: make(chan struct{}), own: map[string]chan frame{}}
	go l.read(ctx)
	defer func() {
		_ = ws.Close(websocket.StatusNormalClosure, "")
		stop()
		<-l.gone
	}()
	if name == "" {
		return resolveLatest(l)
	}
	return resolveName(l, name)
}
