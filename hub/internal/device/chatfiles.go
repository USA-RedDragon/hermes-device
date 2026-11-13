package device

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/USA-RedDragon/talaria/hub/internal/hub"
)

// browseTimeout bounds a folder listing or diff: the plugin gives each of
// its device commands 65 seconds and a diff runs two.
const browseTimeout = 3 * time.Minute

var _ hub.ChatFiles = (*Server)(nil)

// folder is the binding of a chat whose folder user may see: the chat
// runs on one of user's own computers or in the sandbox.
type folder struct {
	b     *backend
	place hub.Place
	conn  ConnectedDevice
}

func (s *Server) chatFolder(ctx context.Context, user, backendID, sessionID string) (folder, error) {
	if s.backends[backendID] == nil {
		if err := s.opts.Access.Allowed(ctx, user, backendID); err != nil {
			return folder{}, err
		}
		return folder{}, hub.ErrNoFolder
	}
	b, err := s.deviceBackend(ctx, user, backendID)
	if err != nil {
		return folder{}, err
	}
	cctx, cancel := context.WithTimeout(ctx, pluginTimeout)
	defer cancel()
	x, err := b.plugin.Resolve(cctx, sessionID)
	if err != nil {
		return folder{}, filesError(err)
	}
	v, err := s.view(ctx, user, b)
	if err != nil {
		return folder{}, err
	}
	p := v.place(sessionID, x)
	if ((p.Kind != hub.PlaceComputer || !p.Mine) && p.Kind != hub.PlaceSandbox) || p.DeviceID == "" {
		return folder{}, hub.ErrNoFolder
	}
	return folder{b: b, place: p, conn: v.online[p.DeviceID]}, nil
}

// filesError turns a plugin refusal into a hub error.
func filesError(err error) error {
	var pe *PluginError
	if !errors.As(err, &pe) {
		return err
	}
	switch pe.Code {
	case codeOffline:
		return hub.ErrMediaOffline
	case codeRunsOnPod, StateFailClosed:
		return hub.ErrNoFolder
	case codeTooLarge:
		return hub.ErrMediaTooLarge
	case codeBusy:
		return hub.ErrFilesBusy
	case codeNotFound, "unknown_session":
		return hub.ErrFileNotFound
	case "read_refused", "bad_path":
		return fmt.Errorf("%w: %s", hub.ErrFileRefused, pe.Message)
	}
	if pe.Status == http.StatusNotFound || pe.Status == http.StatusMethodNotAllowed {
		return hub.ErrFilesTooOld
	}
	return err
}

// ChatFolder lists a folder of the chat's computer; "" is the chat's own
// folder. See hub.ChatFiles.
func (s *Server) ChatFolder(ctx context.Context, user, backendID, sessionID, p string) (hub.ChatFolder, error) {
	f, err := s.chatFolder(ctx, user, backendID, sessionID)
	if err != nil {
		return hub.ChatFolder{}, err
	}
	cctx, cancel := context.WithTimeout(ctx, browseTimeout)
	defer cancel()
	t, err := f.b.plugin.Tree(cctx, sessionID, p)
	if err != nil {
		return hub.ChatFolder{}, filesError(err)
	}
	if t.DeviceID != f.place.DeviceID {
		return hub.ChatFolder{}, hub.ErrNoFolder
	}
	out := hub.ChatFolder{Path: t.Path, Dir: t.Dir, Roots: f.conn.Roots, Truncated: t.Truncated,
		Entries: make([]hub.FolderEntry, 0, len(t.Entries))}
	for _, e := range t.Entries {
		out.Entries = append(out.Entries, hub.FolderEntry{Name: e.Name, Kind: e.Kind, Link: e.Link, Size: e.Size})
	}
	return out, nil
}

// ChatFile opens a file of the chat's computer. See hub.ChatFiles.
func (s *Server) ChatFile(ctx context.Context, user, backendID, sessionID, p string) (*hub.MediaFile, error) {
	f, err := s.chatFolder(ctx, user, backendID, sessionID)
	if err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, readTimeout)
	resp, release, err := f.b.readFile(cctx, sessionID, p)
	if err != nil {
		cancel()
		return nil, filesError(err)
	}
	done := func() {
		cancel()
		release()
	}
	return &hub.MediaFile{Name: hub.CleanName(path.Base(p)), Size: resp.ContentLength, NoStore: true,
		Body: &doneBody{ReadCloser: resp.Body, done: done}}, nil
}

// ChatDiff returns what git shows as changed in the chat's folder. See
// hub.ChatFiles.
func (s *Server) ChatDiff(ctx context.Context, user, backendID, sessionID string) (hub.ChatDiff, error) {
	f, err := s.chatFolder(ctx, user, backendID, sessionID)
	if err != nil {
		return hub.ChatDiff{}, err
	}
	cctx, cancel := context.WithTimeout(ctx, browseTimeout)
	defer cancel()
	d, err := f.b.plugin.Diff(cctx, sessionID)
	if err != nil {
		return hub.ChatDiff{}, filesError(err)
	}
	if d.DeviceID != f.place.DeviceID {
		return hub.ChatDiff{}, hub.ErrNoFolder
	}
	out := hub.ChatDiff{Dir: d.Dir, Git: d.Git, Refused: d.Refused, Diff: d.Diff, Truncated: d.Truncated, Hidden: d.Hidden,
		Files: make([]hub.DiffFile, 0, len(d.Files)), Untracked: d.Untracked}
	if out.Untracked == nil {
		out.Untracked = []string{}
	}
	for _, x := range d.Files {
		out.Files = append(out.Files, hub.DiffFile{Path: x.Path, Status: x.Status})
	}
	return out, nil
}

// ChatChanges lists the files the agent wrote in the chat. See
// hub.ChatFiles.
func (s *Server) ChatChanges(ctx context.Context, user, backendID, sessionID string) (hub.ChatChanges, error) {
	f, err := s.chatFolder(ctx, user, backendID, sessionID)
	if err != nil {
		return hub.ChatChanges{}, err
	}
	cctx, cancel := context.WithTimeout(ctx, pluginTimeout)
	defer cancel()
	c, err := f.b.plugin.Changes(cctx, sessionID)
	if err != nil {
		return hub.ChatChanges{}, filesError(err)
	}
	out := hub.ChatChanges{Truncated: c.Truncated, Files: make([]hub.WrittenFile, 0, len(c.Files))}
	for _, x := range c.Files {
		w := hub.WrittenFile{Path: x.Path, Abs: absPath(x.Path, f.place.Dir, f.conn.Home), Tools: x.Tools, Count: x.Count,
			RowID: x.RowID}
		if x.LastAt > 0 {
			w.At = time.UnixMilli(int64(x.LastAt * 1000))
		}
		out.Files = append(out.Files, w)
	}
	return out, nil
}

// absPath is p, as the agent named it, as an absolute path on the
// computer: "~/" against home, a relative path against dir. "" when it
// cannot tell.
func absPath(p, dir, home string) string {
	switch {
	case strings.HasPrefix(p, "/"):
		return path.Clean(p)
	case p == "~" || strings.HasPrefix(p, "~/"):
		if !path.IsAbs(home) {
			return ""
		}
		return path.Clean(home + p[1:])
	case strings.HasPrefix(p, "~"):
		return ""
	case path.IsAbs(dir):
		return path.Join(dir, p)
	}
	return ""
}
