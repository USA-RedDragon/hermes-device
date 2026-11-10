package device

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/USA-RedDragon/hermes-device/assertion"
	"github.com/USA-RedDragon/hermes-device/protocol"
	"github.com/USA-RedDragon/hermes-hub/internal/gateway"
	"github.com/USA-RedDragon/hermes-hub/internal/hub"
	"github.com/coder/websocket"
)

const (
	hopBackend = "backend"
	hopDevice  = "device"
)

// maxMessage is the largest WebSocket message a leg carries.
const maxMessage = 64 << 20

const dialTimeout = 15 * time.Second

// AssertionLifetime is exp - iat of every assertion the hub signs.
const AssertionLifetime = assertion.MaxLifetime

// Sign makes a fresh assertion for one upgrade of d to path on leg.
func (s *Server) Sign(d hub.Device, leg, path string) (string, error) {
	now := s.opts.Now()
	return assertion.Sign(s.opts.Key, assertion.Claims{
		Aud: d.BackendID, Leg: leg, Method: http.MethodGet, Path: path,
		Sub: d.User, DeviceID: d.ID, DeviceName: d.Name,
		IssuedAt: now.Unix(), Expires: now.Add(AssertionLifetime).Unix(),
	})
}

func (s *Server) handleExec(w http.ResponseWriter, r *http.Request) {
	d := deviceFrom(r)
	b := s.backends[d.BackendID]
	s.proxy(w, r, d, assertion.LegExec, gateway.WebSocketURL(b.cfg.URL), protocol.PluginExecPath, (*leg).copyBoth)
}

func (s *Server) handleACP(w http.ResponseWriter, r *http.Request) {
	d := deviceFrom(r)
	b := s.backends[d.BackendID]
	if b.cfg.ACPURL == "" {
		apiError(w, http.StatusBadGateway, codeBackendUnavailable, "this device's Hermes has no acp-host set up in the hub")
		return
	}
	s.proxy(w, r, d, assertion.LegACP, gateway.WebSocketURL(b.cfg.ACPURL), protocol.ACPHostPath, func(l *leg) { //nolint:contextcheck // the leg outlives the upgrade request
		newACPGuard(s, b, d, l).run()
	})
}

// proxy dials the backend with a fresh assertion and, once that upgrade
// worked, accepts the device's and runs carry on the leg until it ends.
func (s *Server) proxy(w http.ResponseWriter, r *http.Request, d hub.Device, legName, base, path string, carry func(*leg)) {
	if s.isClosed() {
		apiError(w, http.StatusServiceUnavailable, codeBackendUnavailable, "the hub is shutting down")
		return
	}
	tok, err := s.Sign(d, legName, path)
	if err != nil {
		s.log.Error("sign assertion", "error", err.Error())
		apiError(w, http.StatusInternalServerError, codeInternal, "")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), dialTimeout)
	defer cancel()
	h := http.Header{}
	h.Set(assertion.Header, tok)
	up, resp, err := websocket.Dial(ctx, base+path, &websocket.DialOptions{HTTPHeader: h})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		s.log.Warn("dial backend leg", "leg", legName, "device", d.ID, "backend", d.BackendID, "error", err.Error())
		apiError(w, http.StatusBadGateway, codeBackendUnavailable, "the hub could not reach this device's Hermes")
		return
	}
	up.SetReadLimit(maxMessage)
	down, err := websocket.Accept(w, r, nil)
	if err != nil {
		_ = up.Close(websocket.StatusGoingAway, "device upgrade failed")
		return
	}
	down.SetReadLimit(maxMessage)
	l := s.newLeg(r.Context(), d, legName, down, up)
	s.log.Info("device leg open", "leg", legName, "device", d.ID, "backend", d.BackendID)
	carry(l)
}

// copyBoth copies messages unchanged both ways until the leg ends.
func (l *leg) copyBoth() {
	go l.pump(l.down, l.up, true)
	go l.pump(l.up, l.down, false)
	l.run()
}

func (s *Server) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// leg is one proxied WebSocket pair: the device hop (down) and the
// backend hop (up).
type leg struct {
	s      *Server
	dev    hub.Device
	name   string
	down   *websocket.Conn
	up     *websocket.Conn
	ctx    context.Context //nolint:containedctx // the leg's lifetime, shared by its goroutines
	cancel context.CancelFunc
	once   sync.Once
	done   chan struct{}
	opened time.Time
	// closed is how the leg ended, set once by finish.
	closed legClose
}

// legClose is how a leg ended: the close code and reason the device hop
// got, the code the backend hop got, and which side ended it.
type legClose struct {
	code, upCode websocket.StatusCode
	reason, by   string
}

func (s *Server) newLeg(ctx context.Context, d hub.Device, name string, down, up *websocket.Conn) *leg {
	lctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	l := &leg{s: s, dev: d, name: name, down: down, up: up, ctx: lctx, cancel: cancel, done: make(chan struct{}), opened: s.opts.Now()}
	s.mu.Lock()
	if s.legs[d.ID] == nil {
		s.legs[d.ID] = map[*leg]struct{}{}
	}
	s.legs[d.ID][l] = struct{}{}
	closed := s.closed
	s.mu.Unlock()
	if l.records() {
		s.recordLeg(ctx, d.ID, func(ctx context.Context) error { return s.store.ExecLegOpened(ctx, d.ID, l.opened) })
	}
	if closed {
		l.finish(hub.CloseByHub, websocket.StatusGoingAway, websocket.StatusGoingAway, "hub shutting down")
	}
	return l
}

// run pings both hops and rechecks the device until the leg ends.
func (l *leg) run() {
	go l.ping(l.down, true)
	go l.ping(l.up, false)
	if !l.dev.Service {
		go l.recheck()
	}
	<-l.done
	l.s.mu.Lock()
	delete(l.s.legs[l.dev.ID], l)
	if len(l.s.legs[l.dev.ID]) == 0 {
		delete(l.s.legs, l.dev.ID)
	}
	l.s.mu.Unlock()
	c := l.closed
	l.s.log.Info("device leg closed", "leg", l.name, "device", l.dev.ID, "code", int(c.code), "reason", c.reason,
		"backend_code", int(c.upCode), "closed_by", c.by)
	if l.records() && c.code != protocol.CloseReplaced {
		l.s.recordLeg(l.ctx, l.dev.ID, func(ctx context.Context) error {
			return l.s.store.ExecLegClosed(ctx, l.dev.ID, l.opened, hub.LegClose{Code: int(c.code), Reason: c.reason, By: c.by,
				At: l.s.opts.Now()})
		})
	}
}

// records reports whether the leg's opens and closes are kept for the
// computer list: exec legs of paired devices. A 4409 close is not kept,
// since the device is connected again on another leg.
func (l *leg) records() bool { return l.name == assertion.LegExec && !l.dev.Service }

// finish closes both hops once, the device hop with downCode and the
// backend hop with upCode. by is the side that ended the leg (hub.CloseBy*).
func (l *leg) finish(by string, downCode, upCode websocket.StatusCode, reason string) {
	l.once.Do(func() {
		reason = truncateUTF8(strings.ToValidUTF8(reason, ""), maxCloseReason)
		l.closed = legClose{code: downCode, upCode: upCode, reason: reason, by: by}
		var wg sync.WaitGroup
		wg.Go(func() { _ = l.down.Close(downCode, reason) })
		wg.Go(func() { _ = l.up.Close(upCode, reason) })
		go func() {
			t := time.AfterFunc(5*time.Second, func() {
				_ = l.down.CloseNow()
				_ = l.up.CloseNow()
			})
			wg.Wait()
			t.Stop()
			_ = l.down.CloseNow()
			_ = l.up.CloseNow()
			l.cancel()
			close(l.done)
		}()
	})
}

// maxCloseReason is the longest close reason the hub sends or keeps, in
// bytes: a close frame carries at most 123.
const maxCloseReason = 120

// truncateUTF8 cuts s to at most n bytes without splitting a character.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// legID is a new random id for a leg.
func legID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// closeOf is the close code and reason that ended a read, and whether the
// peer sent one.
func closeOf(err error) (websocket.StatusCode, string, bool) {
	var ce websocket.CloseError
	if !errors.As(err, &ce) {
		return 0, "", false
	}
	if ce.Code == websocket.StatusNoStatusRcvd {
		return websocket.StatusNormalClosure, "", true
	}
	if ce.Code == websocket.StatusAbnormalClosure || ce.Code == websocket.StatusTLSHandshake {
		return 0, "", false
	}
	return ce.Code, ce.Reason, true
}

// ended relays how one hop ended to the other.
func (l *leg) ended(err error, fromDevice bool) {
	code, reason, ok := closeOf(err)
	by := hub.CloseByBackend
	if fromDevice {
		by = hub.CloseByDevice
	}
	switch {
	case ok:
		l.finish(by, code, code, reason)
	case fromDevice:
		l.finish(by, websocket.StatusGoingAway, websocket.StatusGoingAway, "device connection lost")
	default:
		l.finish(by, protocol.CloseBackendUnavailable, websocket.StatusGoingAway, "backend connection lost")
	}
}

// pump copies messages from src to dst unchanged until either fails.
func (l *leg) pump(src, dst *websocket.Conn, fromDevice bool) {
	for {
		typ, data, err := src.Read(l.ctx)
		if err != nil {
			l.ended(err, fromDevice)
			return
		}
		if err := dst.Write(l.ctx, typ, data); err != nil {
			l.ended(err, !fromDevice)
			return
		}
	}
}

func (l *leg) ping(c *websocket.Conn, device bool) {
	t := time.NewTicker(l.s.opts.PingInterval)
	defer t.Stop()
	for {
		select {
		case <-l.ctx.Done():
			return
		case <-t.C:
		}
		ctx, cancel := context.WithTimeout(l.ctx, l.s.opts.PingTimeout)
		err := c.Ping(ctx)
		timedOut := errors.Is(ctx.Err(), context.DeadlineExceeded)
		cancel()
		if err == nil {
			continue
		}
		if l.ctx.Err() == nil && timedOut {
			hop := hopBackend
			if device {
				hop = hopDevice
			}
			l.s.log.Info("no pong; closing the device leg", "leg", l.name, "device", l.dev.ID, "hop", hop)
			by := hub.CloseByBackend
			if device {
				by = hub.CloseByDevice
			}
			l.finish(by, protocol.ClosePingTimeout, protocol.ClosePingTimeout, "ping timeout")
		}
		return
	}
}

// recheck closes the leg with 4401 once the device is revoked or its user
// may no longer use its backend.
func (l *leg) recheck() {
	t := time.NewTicker(l.s.opts.Config.RecheckInterval)
	defer t.Stop()
	for {
		select {
		case <-l.ctx.Done():
			return
		case <-l.s.stopped:
			return
		case <-t.C:
		}
		ctx, cancel := context.WithTimeout(l.ctx, 10*time.Second)
		d, ok, err := l.s.store.Device(ctx, l.dev.ID)
		if err == nil && ok {
			err = l.s.allowed(ctx, d)
			if errors.Is(err, hub.ErrForbidden) || errors.Is(err, hub.ErrUnknownBackend) {
				ok = false
				err = nil
			}
		}
		cancel()
		if err != nil {
			if l.ctx.Err() == nil {
				l.s.log.Warn("recheck device", "device", l.dev.ID, "error", err.Error())
			}
			continue
		}
		if !ok {
			l.s.log.Info("device revoked or no longer allowed; closing its leg", "leg", l.name, "device", l.dev.ID)
			l.finish(hub.CloseByHub, protocol.CloseUnauthorized, websocket.StatusNormalClosure, "device revoked")
			return
		}
	}
}
