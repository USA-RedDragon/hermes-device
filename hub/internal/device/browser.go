package device

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/USA-RedDragon/hermes-device/protocol"
	"github.com/USA-RedDragon/hermes-hub/internal/hub"
	"github.com/coder/websocket"
)

// Reasons the hub gives for a browser that may not be used.
const (
	reasonNotSignedIn = "Firefox is not signed in to Hermes"
	reasonInvalid     = "This Firefox sign-in is no longer valid. Sign in to Hermes again."
	reasonOtherDevice = "This Firefox sign-in is for another computer. Sign in to Hermes again."
	reasonUnavailable = "The hub could not check the Firefox sign-in. Try again in a moment."
	reasonSandbox     = "a sandbox device has no browser"
	reasonWrongUser   = "Firefox is signed in to Hermes with another account than the one this computer belongs to. " +
		"Sign in to Hermes in Firefox with that account."
)

// browserTouchEvery is how often a sign-in in use updates its last used
// time.
const browserTouchEvery = time.Minute

const browserCheckTimeout = 10 * time.Second

// browserState is the browser sign-in of one exec leg: the credential the
// device presented, as the hub checked it, and the device's last
// browser_status.
type browserState struct {
	// send serializes the hub's own messages about the sign-in, so they
	// reach the device and the plugin in the order the state changed.
	send sync.Mutex

	mu        sync.Mutex
	presented bool
	sess      *hub.BrowserSession
	reason    string
	status    map[string]json.RawMessage
	touched   time.Time
}

// copyExec copies exec leg messages both ways and enforces the browser
// sign-in (README, Devices: Browser sign-in).
func (l *leg) copyExec() {
	go l.filter(l.down, l.up, true)
	go l.filter(l.up, l.down, false)
	l.run()
}

func (l *leg) filter(src, dst *websocket.Conn, fromDevice bool) {
	for {
		typ, data, err := src.Read(l.ctx)
		if err != nil {
			l.ended(err, fromDevice)
			return
		}
		out := l.inspect(data, fromDevice)
		if out == nil {
			continue
		}
		if err := dst.Write(l.ctx, typ, out); err != nil {
			l.ended(err, !fromDevice)
			return
		}
	}
}

// inspect returns what to forward of one message, or nil to drop it.
func (l *leg) inspect(data []byte, fromDevice bool) []byte {
	t, m, err := protocol.MessageType(data)
	if err != nil {
		hop := hopBackend
		if fromDevice {
			hop = hopDevice
		}
		l.s.log.Warn("dropping an exec leg message whose type is ambiguous", "device", l.dev.ID, "from", hop)
		return nil
	}
	switch {
	case fromDevice && t == protocol.TypeBrowserAuth:
		l.browserAuth(m)
		return nil
	case fromDevice && t == protocol.TypeBrowserStatus:
		return l.browserStatus(m)
	case !fromDevice && t == protocol.TypeBrowserAuthResult:
		l.s.log.Warn("dropping a browser_auth_result from the plugin", "device", l.dev.ID)
		return nil
	case !fromDevice && t == protocol.TypeBrowser:
		if !l.browserAllowed(m) {
			return nil
		}
	}
	return data
}

func (l *leg) writeTo(c *websocket.Conn, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(l.ctx, writeTimeout)
	defer cancel()
	if err := c.Write(ctx, websocket.MessageText, data); err != nil && l.ctx.Err() == nil {
		l.s.log.Debug("write browser sign-in message", "device", l.dev.ID, "error", err.Error())
	}
}

const writeTimeout = 10 * time.Second

func authFailure(code, msg string) protocol.BrowserAuthResult {
	return protocol.BrowserAuthResult{Type: protocol.TypeBrowserAuthResult, Code: code, Error: msg}
}

// checkToken checks a credential the device presented for this leg.
func (l *leg) checkToken(token string) (protocol.BrowserAuthResult, *hub.BrowserSession) {
	if l.dev.Service {
		return authFailure(protocol.BrowserAuthInvalid, reasonSandbox), nil
	}
	if token == "" {
		return authFailure(protocol.BrowserAuthSignedOut, reasonNotSignedIn), nil
	}
	ctx, cancel := context.WithTimeout(l.ctx, browserCheckTimeout)
	defer cancel()
	sess, ok, err := l.s.store.BrowserSessionByToken(ctx, token)
	switch {
	case err != nil:
		l.s.log.Warn("look up browser sign-in", "device", l.dev.ID, "error", err.Error())
		return authFailure(protocol.BrowserAuthUnavailable, reasonUnavailable), nil
	case !ok:
		return authFailure(protocol.BrowserAuthInvalid, reasonInvalid), nil
	case sess.DeviceID != l.dev.ID:
		return authFailure(protocol.BrowserAuthOtherDevice, reasonOtherDevice), nil
	case sess.User != l.dev.User:
		return authFailure(protocol.BrowserAuthWrongUser, reasonWrongUser), nil
	}
	if err := l.s.store.TouchBrowserSession(ctx, sess.ID); err != nil {
		l.s.log.Warn("record browser sign-in use", "device", l.dev.ID, "error", err.Error())
	}
	return protocol.BrowserAuthResult{Type: protocol.TypeBrowserAuthResult, OK: true, User: sess.User, SessionID: sess.ID}, &sess
}

// browserAuth answers the device's browser_auth.
func (l *leg) browserAuth(m map[string]json.RawMessage) {
	var token string
	if raw, ok := m["token"]; ok {
		_ = json.Unmarshal(raw, &token)
	}
	res, sess := l.checkToken(token)
	br := l.br
	br.send.Lock()
	defer br.send.Unlock()
	br.mu.Lock()
	br.presented = token != ""
	br.sess = sess
	br.reason = res.Error
	br.touched = l.s.opts.Now()
	br.mu.Unlock()
	switch {
	case res.OK:
		l.s.log.Info("browser signed in on device leg", "device", l.dev.ID, "user", res.User, "browser_session", res.SessionID)
	case res.Code == protocol.BrowserAuthSignedOut:
		l.s.log.Debug("browser signed out on device leg", "device", l.dev.ID)
	default:
		l.s.log.Info("browser sign-in refused on device leg", "device", l.dev.ID, "code", res.Code)
	}
	l.writeTo(l.down, res)
	l.sendStatusLocked()
}

// dropBrowser ends the leg's sign-in id, or any sign-in when id is "",
// and tells the device and the plugin.
func (l *leg) dropBrowser(id, reason string) {
	br := l.br
	if br == nil {
		return
	}
	br.send.Lock()
	defer br.send.Unlock()
	br.mu.Lock()
	if br.sess == nil || (id != "" && br.sess.ID != id) {
		br.mu.Unlock()
		return
	}
	old := br.sess.ID
	br.sess, br.reason = nil, reason
	br.mu.Unlock()
	l.s.log.Info("browser sign-in dropped on device leg", "device", l.dev.ID, "browser_session", old)
	l.writeTo(l.down, authFailure(protocol.BrowserAuthInvalid, reason))
	l.sendStatusLocked()
}

// rewriteStatus is the device's browser_status m as the plugin gets it:
// the hub's view of the sign-in decides signed_in, user, reason and
// connected. A status of an extension that is not connected keeps no
// reason, so the plugin tells the two apart. Call with br.mu held.
func (br *browserState) rewriteStatus(m map[string]json.RawMessage) []byte {
	out := make(map[string]json.RawMessage, len(m)+3)
	for k, v := range m {
		out[k] = v
	}
	var connected bool
	_ = json.Unmarshal(m["connected"], &connected)
	signedIn := br.sess != nil
	encBool := func(v bool) json.RawMessage {
		if v {
			return json.RawMessage("true")
		}
		return json.RawMessage("false")
	}
	enc := func(v string) json.RawMessage {
		b, err := json.Marshal(v)
		if err != nil {
			return json.RawMessage(`""`)
		}
		return b
	}
	delete(out, "user")
	delete(out, "reason")
	out["signed_in"] = encBool(connected && signedIn)
	out["connected"] = encBool(connected && signedIn)
	switch {
	case !connected:
	case signedIn:
		out["user"] = enc(br.sess.User)
	default:
		reason := br.reason
		if !br.presented || reason == "" {
			reason = reasonNotSignedIn
		}
		out["reason"] = enc(reason)
	}
	data, err := json.Marshal(out)
	if err != nil {
		return nil
	}
	return data
}

// browserStatus remembers the device's browser_status and returns it
// rewritten.
func (l *leg) browserStatus(m map[string]json.RawMessage) []byte {
	br := l.br
	br.send.Lock()
	defer br.send.Unlock()
	br.mu.Lock()
	br.status = m
	data := br.rewriteStatus(m)
	br.mu.Unlock()
	if data == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(l.ctx, writeTimeout)
	defer cancel()
	if err := l.up.Write(ctx, websocket.MessageText, data); err != nil && l.ctx.Err() == nil {
		l.ended(err, false)
	}
	return nil
}

// sendStatusLocked sends the last browser_status again with the current
// sign-in. Call with br.send held.
func (l *leg) sendStatusLocked() {
	br := l.br
	br.mu.Lock()
	if br.status == nil {
		br.mu.Unlock()
		return
	}
	data := br.rewriteStatus(br.status)
	br.mu.Unlock()
	if data == nil {
		return
	}
	ctx, cancel := context.WithTimeout(l.ctx, writeTimeout)
	defer cancel()
	_ = l.up.Write(ctx, websocket.MessageText, data)
}

// browserAllowed checks the leg's sign-in in the store before a browser
// request goes to the device. When it may not, it answers the plugin.
func (l *leg) browserAllowed(m map[string]json.RawMessage) bool {
	br := l.br
	br.mu.Lock()
	var sess *hub.BrowserSession
	if br.sess != nil {
		c := *br.sess
		sess = &c
	}
	reason := br.reason
	if !br.presented {
		reason = ""
	}
	br.mu.Unlock()
	if sess != nil {
		ctx, cancel := context.WithTimeout(l.ctx, browserCheckTimeout)
		cur, ok, err := l.s.store.BrowserSession(ctx, sess.ID)
		cancel()
		switch {
		case err != nil:
			l.s.log.Warn("check browser sign-in", "device", l.dev.ID, "error", err.Error())
			reason = reasonUnavailable
		case !ok || cur.DeviceID != l.dev.ID || cur.User != l.dev.User:
			l.dropBrowser(sess.ID, reasonInvalid)
			reason = reasonInvalid
		default:
			l.touchBrowser(sess.ID)
			return true
		}
	}
	var id string
	if raw, ok := m["id"]; ok {
		_ = json.Unmarshal(raw, &id)
	}
	msg := reasonNotSignedIn + " on " + l.dev.Name
	if reason != "" && reason != reasonNotSignedIn {
		msg += ": " + reason
	}
	l.writeTo(l.up, protocol.BrowserResult{Type: protocol.TypeBrowserResult, ID: id, Code: protocol.BrowserNotSignedIn, Error: msg})
	return false
}

func (l *leg) touchBrowser(id string) {
	br := l.br
	now := l.s.opts.Now()
	br.mu.Lock()
	due := now.Sub(br.touched) >= browserTouchEvery
	if due {
		br.touched = now
	}
	br.mu.Unlock()
	if !due {
		return
	}
	ctx, cancel := context.WithTimeout(l.ctx, browserCheckTimeout)
	defer cancel()
	if err := l.s.store.TouchBrowserSession(ctx, id); err != nil {
		l.s.log.Warn("record browser sign-in use", "device", l.dev.ID, "error", err.Error())
	}
}

// recheckBrowser drops the leg's sign-in once it is revoked or expired.
func (l *leg) recheckBrowser() {
	br := l.br
	if br == nil {
		return
	}
	br.mu.Lock()
	var id string
	if br.sess != nil {
		id = br.sess.ID
	}
	br.mu.Unlock()
	if id == "" {
		return
	}
	ctx, cancel := context.WithTimeout(l.ctx, browserCheckTimeout)
	cur, ok, err := l.s.store.BrowserSession(ctx, id)
	cancel()
	if err != nil {
		return
	}
	if !ok || cur.DeviceID != l.dev.ID || cur.User != l.dev.User {
		l.dropBrowser(id, reasonInvalid)
	}
}

// DropBrowser ends the browser sign-in sessionID on this replica's legs of
// a device at once, as after a revoke.
func (s *Server) DropBrowser(deviceID, sessionID string) {
	s.mu.Lock()
	ls := make([]*leg, 0, len(s.legs[deviceID]))
	for l := range s.legs[deviceID] {
		ls = append(ls, l)
	}
	s.mu.Unlock()
	for _, l := range ls {
		l.dropBrowser(sessionID, reasonInvalid)
	}
}
