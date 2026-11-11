package device

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/USA-RedDragon/hermes-hub/internal/hub"
)

// Sessions starts and deletes the sessions the hub starts on computers.
// *service.Service is one.
type Sessions interface {
	NewSessionAt(ctx context.Context, user, backendID, cwd, rowCWD string) (hub.Session, error)
	DeleteSession(ctx context.Context, user, backendID, sessionID string) error
}

const (
	// resolvedTTL is how long the hub keeps the plugin's resolution of a
	// session that has no binding of its own. Such a session changes place
	// only when it or an ancestor is bound or unbound; the hub forgets its
	// own changes at once.
	resolvedTTL = 5 * time.Minute
	maxResolved = 5000
	// resolveWorkers bound the plugin requests one Places call sends at once.
	resolveWorkers = 8
	placesTimeout  = 5 * time.Second
	onlineTimeout  = 3 * time.Second
	createTimeout  = 30 * time.Second
)

const otherComputer = "another computer"

type resolvedEntry struct {
	b  Binding
	at time.Time
}

type resolvedCache struct {
	mu sync.Mutex
	m  map[hub.SessionRef]resolvedEntry
}

func (c *resolvedCache) get(k hub.SessionRef, now time.Time) (Binding, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[k]
	if !ok || now.Sub(e.at) >= resolvedTTL {
		return Binding{}, false
	}
	return e.b, true
}

func (c *resolvedCache) put(k hub.SessionRef, b Binding, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[hub.SessionRef]resolvedEntry{}
	}
	if len(c.m) >= maxResolved {
		for k, e := range c.m {
			if now.Sub(e.at) >= resolvedTTL {
				delete(c.m, k)
			}
		}
		if len(c.m) >= maxResolved {
			c.m = map[hub.SessionRef]resolvedEntry{}
		}
	}
	c.m[k] = resolvedEntry{b: b, at: now}
}

// forget drops a session and every session whose binding came through
// it, after the hub bound or unbound it.
func (c *resolvedCache) forget(k hub.SessionRef) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, k)
	for o, e := range c.m {
		if o.BackendID == k.BackendID && e.b.SessionID == k.SessionID {
			delete(c.m, o)
		}
	}
}

// view is what one request knows about a backend's devices: the user's
// paired devices by id and the devices connected to the plugin. online is
// nil when the plugin could not be asked.
type view struct {
	mine   map[string]hub.Device
	names  map[string]string
	online map[string]ConnectedDevice
}

func (s *Server) view(ctx context.Context, user string, b *backend) (view, error) {
	ds, err := s.store.Devices(ctx, user)
	if err != nil {
		return view{}, err
	}
	v := view{mine: map[string]hub.Device{}, names: map[string]string{}}
	for _, d := range ds {
		if d.BackendID == b.cfg.ID {
			v.mine[d.ID] = d
		}
	}
	for _, sd := range b.cfg.ServiceDevices {
		v.names[sd.ID] = sd.Name
		if sd.Name == "" {
			v.names[sd.ID] = sd.ID
		}
	}
	cctx, cancel := context.WithTimeout(ctx, onlineTimeout)
	defer cancel()
	conns, err := b.plugin.Devices(cctx)
	if err != nil {
		s.log.Warn("list connected devices", "backend", b.cfg.ID, "error", err.Error())
		return v, nil
	}
	v.online = map[string]ConnectedDevice{}
	for _, c := range conns {
		v.online[c.DeviceID] = c
	}
	return v, nil
}

func (v view) isOnline(id string) bool {
	_, ok := v.online[id]
	return ok
}

// place turns the plugin's resolution of a session into a Place.
func (v view) place(sid string, b Binding) hub.Place {
	switch b.Resolved.State {
	case StateBound:
		p := hub.Place{Kind: hub.PlaceComputer, DeviceID: b.DeviceID, Dir: b.Dir, Online: v.isOnline(b.DeviceID),
			Inherited: b.SessionID != "" && b.SessionID != sid}
		if d, ok := v.mine[b.DeviceID]; ok {
			p.Name, p.Mine = d.Name, true
		} else if n, ok := v.names[b.DeviceID]; ok {
			p.Name = n
		} else {
			p.Name, p.DeviceID, p.Dir = otherComputer, "", ""
		}
		return p
	case StateFailClosed:
		return hub.Place{Kind: hub.PlaceNone}
	}
	if len(b.Resolved.Sandbox) == 0 {
		return hub.Place{Kind: hub.PlaceServer, Online: true}
	}
	p := hub.Place{Kind: hub.PlaceSandbox}
	if len(v.names) == 1 {
		for id, n := range v.names {
			p.DeviceID, p.Name = id, n
		}
	}
	var sb SandboxBinding
	if json.Unmarshal(b.Resolved.Sandbox, &sb) == nil && sb.DeviceID != "" {
		p.DeviceID, p.Dir, p.Online = sb.DeviceID, sb.Dir, v.isOnline(sb.DeviceID)
		p.Name = v.names[sb.DeviceID]
		if p.Name == "" {
			p.Name = sb.DeviceID
		}
	}
	return p
}

// deviceBackend returns the backend of a device backend id that user may
// use.
func (s *Server) deviceBackend(ctx context.Context, user, backendID string) (*backend, error) {
	b := s.backends[backendID]
	if b == nil {
		return nil, hub.ErrUnknownBackend
	}
	if err := s.opts.Access.Allowed(ctx, user, backendID); err != nil {
		return nil, err
	}
	return b, nil
}

// computer returns one of user's paired computers and its backend.
func (s *Server) computer(ctx context.Context, user, id string) (hub.Device, *backend, error) {
	d, ok, err := s.store.Device(ctx, id)
	if err != nil {
		return hub.Device{}, nil, err
	}
	if !ok || d.User != user || !d.Paired || d.Service {
		return hub.Device{}, nil, hub.ErrUnknownDevice
	}
	b, err := s.deviceBackend(ctx, user, d.BackendID)
	if errors.Is(err, hub.ErrUnknownBackend) {
		return hub.Device{}, nil, hub.ErrUnknownDevice
	}
	return d, b, err
}

// Computers lists user's paired computers. See hub.Computers.
func (s *Server) Computers(ctx context.Context, user string) ([]hub.Computer, error) {
	ds, err := s.store.Devices(ctx, user)
	if err != nil {
		return nil, err
	}
	online := map[string]map[string]ConnectedDevice{}
	out := []hub.Computer{}
	for _, d := range ds {
		b := s.backends[d.BackendID]
		if !d.Paired || b == nil || s.opts.Access.Allowed(ctx, user, d.BackendID) != nil {
			continue
		}
		conns, asked := online[b.cfg.ID]
		if !asked {
			conns = map[string]ConnectedDevice{}
			cctx, cancel := context.WithTimeout(ctx, onlineTimeout)
			list, err := b.plugin.Devices(cctx)
			cancel()
			if err != nil {
				s.log.Warn("list connected devices", "backend", b.cfg.ID, "error", err.Error())
			}
			for _, c := range list {
				conns[c.DeviceID] = c
			}
			online[b.cfg.ID] = conns
		}
		c := hub.Computer{ID: d.ID, Name: d.Name, BackendID: d.BackendID, Hostname: d.Hostname, OS: d.OS}
		if cd, ok := conns[d.ID]; ok {
			c.Online, c.Home, c.Roots = true, cd.Home, cd.Roots
		}
		if c.Recent, err = s.store.DeviceDirs(ctx, d.ID); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

// Folders lists the subdirectories of a directory on a computer. See
// hub.Computers.
func (s *Server) Folders(ctx context.Context, user, deviceID, dir string) (hub.Folder, error) {
	_, b, err := s.computer(ctx, user, deviceID)
	if err != nil {
		return hub.Folder{}, err
	}
	if dir != "" && !path.IsAbs(dir) {
		return hub.Folder{}, fmt.Errorf("%w: pick a full folder path", hub.ErrFolderRefused)
	}
	cctx, cancel := context.WithTimeout(ctx, pluginTimeout)
	defer cancel()
	ls, err := b.plugin.Dirs(cctx, deviceID, dir)
	if err != nil {
		return hub.Folder{}, computerError(err)
	}
	out := hub.Folder{Path: ls.Path, Dirs: make([]string, 0, len(ls.Entries))}
	for _, e := range ls.Entries {
		out.Dirs = append(out.Dirs, e.Name)
	}
	return out, nil
}

// computerError turns a plugin refusal into a hub error.
func computerError(err error) error {
	var pe *PluginError
	if !errors.As(err, &pe) {
		return err
	}
	switch pe.Code {
	case codeOffline, "unknown_device":
		return hub.ErrComputerOffline
	case "bad_dir":
		return fmt.Errorf("%w: pick a full folder path", hub.ErrFolderRefused)
	case "dir_refused":
		msg := "the computer does not allow that folder"
		if pe.Message != "" {
			msg += " (" + pe.Message + ")"
		}
		return fmt.Errorf("%w: %s", hub.ErrFolderRefused, msg)
	case "not_device_session":
		return fmt.Errorf("%w: this chat cannot run on a computer. Start a new chat on the computer instead", hub.ErrCannotMove)
	case "bound_to_other_device":
		return fmt.Errorf("%w: this chat is on another computer", hub.ErrCannotMove)
	}
	return err
}

// Places says where each session runs. See hub.Computers.
func (s *Server) Places(ctx context.Context, user string, sessions []hub.SessionRef) map[hub.SessionRef]hub.Place {
	ctx, cancel := context.WithTimeout(ctx, placesTimeout)
	defer cancel()
	byBackend := map[string][]string{}
	for _, r := range sessions {
		byBackend[r.BackendID] = append(byBackend[r.BackendID], r.SessionID)
	}
	out := map[hub.SessionRef]hub.Place{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for id, sids := range byBackend {
		b, err := s.deviceBackend(ctx, user, id)
		if err != nil {
			continue
		}
		wg.Go(func() {
			got := s.backendPlaces(ctx, user, b, sids)
			mu.Lock()
			for sid, p := range got {
				out[hub.SessionRef{BackendID: id, SessionID: sid}] = p
			}
			mu.Unlock()
		})
	}
	wg.Wait()
	return out
}

func (s *Server) backendPlaces(ctx context.Context, user string, b *backend, sids []string) map[string]hub.Place {
	v, err := s.view(ctx, user, b)
	if err != nil {
		s.log.Warn("look up devices for places", "backend", b.cfg.ID, "error", err.Error())
		return nil
	}
	bindings, err := b.plugin.Bindings(ctx)
	if err != nil {
		s.log.Warn("list bindings", "backend", b.cfg.ID, "error", err.Error())
		return nil
	}
	own := make(map[string]Binding, len(bindings))
	for _, x := range bindings {
		x.Resolved = Resolved{State: StateBound}
		own[x.SessionID] = x
	}
	out := map[string]hub.Place{}
	var todo []string
	now := s.opts.Now()
	for _, sid := range sids {
		if x, ok := own[sid]; ok {
			out[sid] = v.place(sid, x)
		} else if x, ok := s.resolved.get(hub.SessionRef{BackendID: b.cfg.ID, SessionID: sid}, now); ok {
			out[sid] = v.place(sid, x)
		} else {
			todo = append(todo, sid)
		}
	}
	if len(todo) == 0 {
		return out
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	work := make(chan string)
	for range min(resolveWorkers, len(todo)) {
		wg.Go(func() {
			for sid := range work {
				x, err := b.plugin.Resolve(ctx, sid)
				if err != nil {
					continue
				}
				s.resolved.put(hub.SessionRef{BackendID: b.cfg.ID, SessionID: sid}, x, s.opts.Now())
				mu.Lock()
				out[sid] = v.place(sid, x)
				mu.Unlock()
			}
		})
	}
	for _, sid := range todo {
		if ctx.Err() != nil {
			break
		}
		work <- sid
	}
	close(work)
	wg.Wait()
	return out
}

// Place says where one session runs. See hub.Computers.
func (s *Server) Place(ctx context.Context, user, backendID, sessionID string) (hub.Place, error) {
	if s.backends[backendID] == nil {
		if err := s.opts.Access.Allowed(ctx, user, backendID); err != nil {
			return hub.Place{}, err
		}
		return hub.Place{}, nil
	}
	b, err := s.deviceBackend(ctx, user, backendID)
	if err != nil {
		return hub.Place{}, err
	}
	cctx, cancel := context.WithTimeout(ctx, pluginTimeout)
	defer cancel()
	x, err := b.plugin.Resolve(cctx, sessionID)
	if err != nil {
		return hub.Place{}, err
	}
	k := hub.SessionRef{BackendID: backendID, SessionID: sessionID}
	if x.Resolved.State == StateBound && x.SessionID == sessionID {
		s.resolved.forget(k)
	} else {
		s.resolved.put(k, x, s.opts.Now())
	}
	v, err := s.view(ctx, user, b)
	if err != nil {
		return hub.Place{}, err
	}
	return v.place(sessionID, x), nil
}

// cleanDir checks a directory a user picked: "" is the computer's first
// root, which an online computer reports.
func cleanDir(dir string, c ConnectedDevice) (string, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		if len(c.Roots) == 0 {
			return "", fmt.Errorf("%w: pick a folder", hub.ErrFolderRefused)
		}
		return c.Roots[0], nil
	}
	if !path.IsAbs(dir) {
		return "", fmt.Errorf("%w: pick a full folder path", hub.ErrFolderRefused)
	}
	return path.Clean(dir), nil
}

// StartOn starts a session on a computer. It runs session.create in the
// plugin placeholder with the device directory as the row cwd, as the
// gateway leg does, and binds the session before it returns, so no prompt
// reaches it unbound. A failed bind deletes the session again. See
// hub.Computers.
func (s *Server) StartOn(ctx context.Context, user, deviceID, dir string) (hub.Session, hub.Place, error) {
	d, b, err := s.computer(ctx, user, deviceID)
	if err != nil {
		return hub.Session{}, hub.Place{}, err
	}
	if s.opts.Sessions == nil {
		return hub.Session{}, hub.Place{}, errors.New("device: no session starter")
	}
	v, err := s.view(ctx, user, b)
	if err != nil {
		return hub.Session{}, hub.Place{}, err
	}
	conn, ok := v.online[d.ID]
	if !ok {
		return hub.Session{}, hub.Place{}, hub.ErrComputerOffline
	}
	if dir, err = cleanDir(dir, conn); err != nil {
		return hub.Session{}, hub.Place{}, err
	}
	ph, err := s.placeholderDir(ctx, b)
	if err != nil {
		return hub.Session{}, hub.Place{}, fmt.Errorf("device plugin placeholder: %w", err)
	}
	// From here on the steps run to the end even when the caller goes
	// away, so a session the gateway made is bound or deleted again.
	ctx = context.WithoutCancel(ctx)
	cctx, cancel := context.WithTimeout(ctx, createTimeout)
	sess, err := s.opts.Sessions.NewSessionAt(cctx, user, b.cfg.ID, ph, dir)
	cancel()
	if err != nil {
		b.forgetPlaceholder()
		return hub.Session{}, hub.Place{}, err
	}
	bctx, cancel := context.WithTimeout(ctx, pluginTimeout)
	bound, err := b.plugin.Bind(bctx, sess.ID, d.ID, dir)
	cancel()
	if err != nil {
		s.log.Warn("bind after starting a session on a computer failed; deleting the session", "device", d.ID,
			"session_id", sess.ID, "error", err.Error())
		dctx, cancel := context.WithTimeout(ctx, createTimeout)
		if derr := s.opts.Sessions.DeleteSession(dctx, user, b.cfg.ID, sess.ID); derr != nil {
			s.log.Warn("deleting the unbound session failed", "device", d.ID, "session_id", sess.ID, "error", derr.Error())
		}
		cancel()
		return hub.Session{}, hub.Place{}, computerError(err)
	}
	s.usedDir(ctx, d.ID, bound.Dir)
	s.log.Info("session started on a computer", "user", user, "device", d.ID, "session_id", sess.ID)
	bound.Resolved = Resolved{State: StateBound}
	return sess, v.place(sess.ID, bound), nil
}

func (s *Server) usedDir(ctx context.Context, deviceID, dir string) {
	if err := s.store.UseDeviceDir(ctx, deviceID, dir); err != nil {
		s.log.Warn("remember folder", "device", deviceID, "error", err.Error())
	}
}

// owned reports whether user may unbind a binding of deviceID: one of
// their own computers, a sandbox, or a device the hub no longer knows.
func (s *Server) owned(ctx context.Context, user string, v view, deviceID string) (bool, error) {
	if _, ok := v.mine[deviceID]; ok {
		return true, nil
	}
	if _, ok := v.names[deviceID]; ok {
		return true, nil
	}
	d, ok, err := s.store.Device(ctx, deviceID)
	if err != nil {
		return false, err
	}
	return !ok || d.User == user, nil
}

// MoveTo binds a session to a computer. A session bound to another of
// user's computers is unbound from it first; if the new bind then fails,
// the hub binds it back. See hub.Computers.
func (s *Server) MoveTo(ctx context.Context, user, backendID, sessionID, deviceID, dir string) (hub.Place, error) {
	b, err := s.deviceBackend(ctx, user, backendID)
	if err != nil {
		return hub.Place{}, err
	}
	d, db, err := s.computer(ctx, user, deviceID)
	if err != nil {
		return hub.Place{}, err
	}
	if db != b {
		return hub.Place{}, hub.ErrOtherBackend
	}
	v, err := s.view(ctx, user, b)
	if err != nil {
		return hub.Place{}, err
	}
	conn, ok := v.online[d.ID]
	if !ok {
		return hub.Place{}, hub.ErrComputerOffline
	}
	if dir, err = cleanDir(dir, conn); err != nil {
		return hub.Place{}, err
	}
	// The steps run to the end even when the caller goes away, each with
	// a time limit of its own, so a failed bind still has time to go back.
	ctx = context.WithoutCancel(ctx)
	defer s.resolved.forget(hub.SessionRef{BackendID: backendID, SessionID: sessionID})
	cur, err := withTimeout(ctx, func(ctx context.Context) (Binding, error) { return b.plugin.Resolve(ctx, sessionID) })
	if err != nil {
		return hub.Place{}, err
	}
	if cur.Resolved.State == StateBound && cur.DeviceID != d.ID {
		mine, err := s.owned(ctx, user, v, cur.DeviceID)
		if err != nil {
			return hub.Place{}, err
		}
		if !mine {
			return hub.Place{}, fmt.Errorf("%w: this chat is on someone else's computer", hub.ErrCannotMove)
		}
	}
	var prev *Binding
	if cur.Resolved.State == StateBound && cur.SessionID == sessionID && cur.DeviceID != d.ID {
		if _, err := withTimeout(ctx, func(ctx context.Context) (Dirs, error) { return b.plugin.Dirs(ctx, d.ID, dir) }); err != nil {
			return hub.Place{}, computerError(err)
		}
		if _, err := withTimeout(ctx, func(ctx context.Context) (struct{}, error) { return struct{}{}, b.plugin.Unbind(ctx, sessionID) }); err != nil {
			return hub.Place{}, computerError(err)
		}
		prev = &cur
	}
	bound, err := withTimeout(ctx, func(ctx context.Context) (Binding, error) { return b.plugin.Bind(ctx, sessionID, d.ID, dir) })
	if err != nil {
		if prev == nil {
			return hub.Place{}, computerError(err)
		}
		return s.moveBack(ctx, v, b, sessionID, d.ID, *prev, err)
	}
	s.usedDir(ctx, d.ID, bound.Dir)
	s.log.Info("session moved to a computer", "user", user, "device", d.ID, "session_id", sessionID)
	bound.Resolved = Resolved{State: StateBound}
	return v.place(sessionID, bound), nil
}

// withTimeout runs one plugin step with a time limit of its own.
func withTimeout[T any](ctx context.Context, fn func(context.Context) (T, error)) (T, error) {
	ctx, cancel := context.WithTimeout(ctx, pluginTimeout)
	defer cancel()
	return fn(ctx)
}

// moveBack binds a session back to the computer it left after the bind
// to deviceID failed. A bind that timed out may still have gone through,
// so when the way back fails the hub reports where the session is now.
func (s *Server) moveBack(ctx context.Context, v view, b *backend, sessionID, deviceID string, prev Binding, bindErr error) (hub.Place, error) {
	_, err := withTimeout(ctx, func(ctx context.Context) (Binding, error) {
		return b.plugin.Bind(ctx, sessionID, prev.DeviceID, prev.Dir)
	})
	if err == nil {
		return hub.Place{}, computerError(bindErr)
	}
	s.log.Warn("binding a session back after a failed move failed", "session_id", sessionID, "device", prev.DeviceID,
		"error", err.Error())
	now, err := withTimeout(ctx, func(ctx context.Context) (Binding, error) { return b.plugin.Resolve(ctx, sessionID) })
	if err == nil && now.Resolved.State == StateBound && now.SessionID == sessionID && now.DeviceID == deviceID {
		s.usedDir(ctx, deviceID, now.Dir)
		return v.place(sessionID, now), nil
	}
	return hub.Place{}, fmt.Errorf("%w: the move failed (%s) and the chat could not go back, so it is on no computer now. "+
		"Move it again", hub.ErrCannotMove, userReason(bindErr))
}

// userReason is the part of a computer error a user reads.
func userReason(err error) string {
	_, msg, ok := strings.Cut(computerError(err).Error(), ": ")
	if !ok {
		return "the computer did not answer"
	}
	return msg
}

// Release unbinds a session from one of user's computers. See
// hub.Computers.
func (s *Server) Release(ctx context.Context, user, backendID, sessionID string) error {
	b, err := s.deviceBackend(ctx, user, backendID)
	if err != nil {
		return err
	}
	ctx = context.WithoutCancel(ctx)
	defer s.resolved.forget(hub.SessionRef{BackendID: backendID, SessionID: sessionID})
	cur, err := withTimeout(ctx, func(ctx context.Context) (Binding, error) { return b.plugin.Resolve(ctx, sessionID) })
	if err != nil {
		return err
	}
	if cur.Resolved.State != StateBound {
		return hub.ErrNotOnComputer
	}
	if cur.SessionID != sessionID {
		return hub.ErrInheritedPlace
	}
	v, err := s.view(ctx, user, b)
	if err != nil {
		return err
	}
	mine, err := s.owned(ctx, user, v, cur.DeviceID)
	if err != nil {
		return err
	}
	if !mine {
		return hub.ErrForbidden
	}
	if _, err := withTimeout(ctx, func(ctx context.Context) (struct{}, error) { return struct{}{}, b.plugin.Unbind(ctx, sessionID) }); err != nil {
		var pe *PluginError
		if errors.As(err, &pe) && pe.Status == http.StatusNotFound {
			return hub.ErrNotOnComputer
		}
		return err
	}
	// An earlier session of the chat's lineage with a binding of its own
	// would now serve it (protocol.md section 8.3 rule 2), so the chat
	// would not stop: bind it back and refuse.
	now, err := withTimeout(ctx, func(ctx context.Context) (Binding, error) { return b.plugin.Resolve(ctx, sessionID) })
	if err == nil && now.Resolved.State == StateBound {
		if _, err := withTimeout(ctx, func(ctx context.Context) (Binding, error) {
			return b.plugin.Bind(ctx, sessionID, cur.DeviceID, cur.Dir)
		}); err != nil {
			s.log.Warn("binding a session back after a release it would not stop failed", "session_id", sessionID, "error", err.Error())
		}
		return hub.ErrInheritedPlace
	}
	s.log.Info("session released from its computer", "user", user, "device", cur.DeviceID, "session_id", sessionID)
	return nil
}
