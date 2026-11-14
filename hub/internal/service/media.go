package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/USA-RedDragon/talaria/hub/internal/gateway"
	"github.com/USA-RedDragon/talaria/hub/internal/hub"
)

var _ hub.Media = (*Service)(nil)

// fileReader opens files of a backend's server. *gateway.Dialer is one.
type fileReader interface {
	File(ctx context.Context, path string) (*http.Response, error)
}

// defaultMedia are the files the hub shows on a backend without media
// roots, when they are images: talaria's browser_screenshot files,
// <HERMES_HOME>/cache/screenshots/hermes-device/<session>/<file>, and the
// images users attach, which the gateway stages in <HERMES_HOME>/cache/images
// (<HERMES_HOME>/image_cache on older installs).
var defaultMedia = regexp.MustCompile(`^/.+/(?:cache/screenshots/hermes-device/[^/]+|cache/images|image_cache)/[^/]+$`)

// inMediaRoots reports whether p is under one of roots, or, with no roots,
// one of the default media files.
func inMediaRoots(roots []string, p string) bool {
	if len(roots) == 0 {
		return hub.ImagePath(p) && defaultMedia.MatchString(p)
	}
	for _, r := range roots {
		if strings.HasPrefix(p, r+"/") {
			return true
		}
	}
	return false
}

// Media opens a file that the session's replies, browser_screenshot results,
// user image attachments or vision_analyze and browser_vision calls name. A path under the media roots comes from
// the backend's server; any other path, and one the server does not have,
// comes from the device that runs the session, when one does. See
// hub.Media.
func (s *Service) Media(ctx context.Context, user, backendID, sessionID, p string) (*hub.MediaFile, error) {
	b, err := s.backend(ctx, user, backendID)
	if err != nil {
		return nil, err
	}
	if !hub.MediaPath(p) {
		return nil, hub.ErrNoMedia
	}
	fr, server := b.dialer.(fileReader)
	server = server && inMediaRoots(b.cfg.MediaRoots, p)
	dm, device := s.files.(hub.DeviceMedia)
	if !server && !device {
		return nil, hub.ErrNoMedia
	}
	ls, err := b.session(ctx, sessionID)
	if err != nil {
		return nil, refusal(err)
	}
	named := ls.names(p)
	ls.unpin()
	if !named {
		return nil, hub.ErrNoMedia
	}
	if server {
		f, err := serverMedia(ctx, fr, p)
		if !device || !errors.Is(err, hub.ErrNoMedia) {
			return f, err
		}
	}
	f, ok, err := dm.DeviceMedia(ctx, user, backendID, sessionID, p)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, hub.ErrNoMedia
	}
	m, err := sniffMedia(p, f.Size, f.Body)
	if m != nil {
		m.NoStore = f.NoStore
	}
	return m, err
}

// serverMedia opens a file of the backend's server.
func serverMedia(ctx context.Context, fr fileReader, p string) (*hub.MediaFile, error) {
	resp, err := fr.File(ctx, p)
	if errors.Is(err, gateway.ErrNoFile) {
		return nil, hub.ErrNoMedia
	}
	if err != nil {
		return nil, err
	}
	return sniffMedia(p, resp.ContentLength, resp.Body)
}

// sniffMedia reads the first bytes of a file to type an image, and caps
// the body at hub.MaxMediaBytes. It closes body on failure.
func sniffMedia(p string, size int64, body io.ReadCloser) (*hub.MediaFile, error) {
	if size > hub.MaxMediaBytes {
		_ = body.Close()
		return nil, hub.ErrMediaTooLarge
	}
	head := make([]byte, 16)
	n, err := io.ReadFull(body, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		_ = body.Close()
		return nil, err
	}
	head = head[:n]
	mime := ""
	if hub.ImagePath(p) {
		mime = hub.ImageMIME(head)
	}
	r := io.MultiReader(bytes.NewReader(head), body)
	return &hub.MediaFile{Name: hub.CleanName(path.Base(p)), MIME: mime, Size: size,
		Body: readCloser{&capReader{r: r, left: hub.MaxMediaBytes}, body}}, nil
}

// capReader fails with hub.ErrMediaTooLarge past left bytes.
type capReader struct {
	r    io.Reader
	left int64
}

func (c *capReader) Read(b []byte) (int, error) {
	if c.left <= 0 {
		var one [1]byte
		n, err := c.r.Read(one[:])
		if n > 0 {
			return 0, hub.ErrMediaTooLarge
		}
		return 0, err
	}
	if int64(len(b)) > c.left {
		b = b[:c.left]
	}
	n, err := c.r.Read(b)
	c.left -= int64(n)
	return n, err
}

type readCloser struct {
	io.Reader
	io.Closer
}

// names reports whether a reply, a browser_screenshot result, a user's
// image attachment or the image a vision_analyze or browser_vision call
// looked at in the session names p: in the snapshot's transcript or
// the events since.
func (ls *live) names(p string) bool {
	ls.mu.Lock()
	snap := ls.snapshot.Data
	ring := ls.ring[:len(ls.ring):len(ls.ring)]
	// A message that waits for the running turn is in neither the
	// snapshot's transcript nor the ring yet, but its page already shows
	// the images it carries.
	var queued []string
	for id, t := range ls.texts {
		queued = append(queued, t)
		if in, ok := ls.inputs[id]; ok && in.Text != t {
			queued = append(queued, in.Text)
		}
	}
	for id, in := range ls.inputs {
		if _, ok := ls.texts[id]; !ok {
			queued = append(queued, in.Text)
		}
	}
	ls.mu.Unlock()
	for _, t := range queued {
		if slices.Contains(hub.AttachedImages(t), p) {
			return true
		}
	}
	var s struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if len(snap) > 0 && json.Unmarshal(snap, &s) == nil && rowsName(s.Messages, p) {
		return true
	}
	return eventsName(ring, p)
}

type row struct {
	Role       string          `json:"role"`
	Text       *string         `json:"text"`
	Content    json.RawMessage `json:"content"`
	Name       string          `json:"name"`
	ToolName   string          `json:"tool_name"`
	ToolCallID string          `json:"tool_call_id"`
	ToolCalls  []struct {
		ID       string `json:"id"`
		CallID   string `json:"call_id"`
		Function struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls"`
}

func rowsName(rows []json.RawMessage, p string) bool {
	calls := map[string]string{}
	for _, raw := range rows {
		var r row
		if json.Unmarshal(raw, &r) != nil {
			continue
		}
		var content string
		if r.Text != nil {
			content = *r.Text
		} else {
			_ = json.Unmarshal(r.Content, &content)
		}
		switch r.Role {
		case "user":
			if slices.Contains(hub.AttachedImages(content), p) {
				return true
			}
		case "assistant":
			for _, c := range r.ToolCalls {
				if visionNames(c.Function.Name, c.Function.Arguments, p) {
					return true
				}
				for _, id := range []string{c.ID, c.CallID} {
					if id != "" {
						calls[id] = c.Function.Name
					}
				}
			}
			if replyNames(content, p) {
				return true
			}
		case "tool":
			name := r.Name
			if name == "" {
				name = r.ToolName
			}
			if name == "" {
				name = calls[r.ToolCallID]
			}
			if name == hub.ScreenshotTool && screenshotNames(content, p) {
				return true
			}
		}
	}
	return false
}

func eventsName(ring []hub.Event, p string) bool {
	var reply strings.Builder
	for _, e := range ring {
		var d struct {
			Text   *string         `json:"text"`
			Name   string          `json:"name"`
			Args   json.RawMessage `json:"args"`
			Result json.RawMessage `json:"result"`
		}
		_ = json.Unmarshal(e.Data, &d)
		text := ""
		if d.Text != nil {
			text = *d.Text
		}
		switch e.Kind {
		case hub.KindMessageDelta:
			reply.WriteString(text)
		case hub.KindMessageStart, hub.KindMessageComplete, hub.KindMessageInterim:
			if e.Kind == hub.KindMessageStart && slices.Contains(hub.AttachedImages(text), p) ||
				e.Kind != hub.KindMessageStart && replyNames(text, p) || replyNames(reply.String(), p) {
				return true
			}
			if e.Kind != hub.KindMessageInterim {
				reply.Reset()
			}
		case hub.KindToolStart, hub.KindToolComplete:
			if visionNames(d.Name, d.Args, p) {
				return true
			}
			if e.Kind != hub.KindToolComplete {
				continue
			}
			if d.Name != hub.ScreenshotTool {
				continue
			}
			var res string
			if json.Unmarshal(d.Result, &res) != nil {
				res = string(d.Result)
			}
			if screenshotNames(res, p) {
				return true
			}
		}
	}
	return replyNames(reply.String(), p)
}

func replyNames(text, p string) bool {
	if !strings.Contains(text, p) {
		return false
	}
	paths, _ := hub.MediaDirectives(text)
	return slices.Contains(paths, p) || slices.Contains(hub.MarkdownMedia(text), p)
}

func screenshotNames(result, p string) bool {
	var m map[string]any
	return json.Unmarshal([]byte(result), &m) == nil && hub.ScreenshotPath(m) == p
}

// isVisionTool is true for the tools that look at an image named by an image_url
// argument.
func isVisionTool(name string) bool { return name == "vision_analyze" || name == "browser_vision" }

const toolBatch = "tool_call"

// visionNames reports whether a call of a vision tool, or a batch of calls
// (tool_call) that holds one, looks at the image p. The arguments are a JSON
// object or a string that holds one. Only the image_url of the call counts,
// and p is already a MediaPath.
func visionNames(name string, args json.RawMessage, p string) bool {
	if !hub.ImagePath(p) || len(args) == 0 {
		return false
	}
	if name != toolBatch && !isVisionTool(name) {
		return false
	}
	var s string
	if json.Unmarshal(args, &s) == nil {
		args = json.RawMessage(s)
	}
	var a struct {
		ImageURL string `json:"image_url"`
		Calls    []struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"calls"`
	}
	if json.Unmarshal(args, &a) != nil {
		return false
	}
	if name != toolBatch {
		return a.ImageURL == p
	}
	for _, c := range a.Calls {
		if c.Name != toolBatch && visionNames(c.Name, c.Arguments, p) {
			return true
		}
	}
	return false
}
