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
	"strings"

	"github.com/USA-RedDragon/hermes-hub/internal/gateway"
	"github.com/USA-RedDragon/hermes-hub/internal/hub"
)

var _ hub.Media = (*Service)(nil)

// fileReader opens files of a backend's server. *gateway.Dialer is one.
type fileReader interface {
	File(ctx context.Context, path string) (*http.Response, error)
}

// screenshotFile is where hermes-device's browser_screenshot saves files:
// <HERMES_HOME>/cache/screenshots/hermes-device/<session>/<file>.
var screenshotFile = regexp.MustCompile(`^/.+/cache/screenshots/hermes-device/[^/]+/[^/]+$`)

// inMediaRoots reports whether p is under one of roots, or, with no roots,
// in a hermes-device screenshot folder.
func inMediaRoots(roots []string, p string) bool {
	if len(roots) == 0 {
		return screenshotFile.MatchString(p)
	}
	for _, r := range roots {
		if strings.HasPrefix(p, r+"/") {
			return true
		}
	}
	return false
}

// Media opens a file that the session's replies or browser_screenshot
// results name. See hub.Media.
func (s *Service) Media(ctx context.Context, user, backendID, sessionID, p string) (*hub.MediaFile, error) {
	b, err := s.backend(ctx, user, backendID)
	if err != nil {
		return nil, err
	}
	fr, ok := b.dialer.(fileReader)
	if !ok || !hub.MediaPath(p) || !inMediaRoots(b.cfg.MediaRoots, p) {
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
	resp, err := fr.File(ctx, p)
	if errors.Is(err, gateway.ErrNoFile) {
		return nil, hub.ErrNoMedia
	}
	if err != nil {
		return nil, err
	}
	if resp.ContentLength > hub.MaxMediaBytes {
		_ = resp.Body.Close()
		return nil, hub.ErrMediaTooLarge
	}
	head := make([]byte, 16)
	n, err := io.ReadFull(resp.Body, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		_ = resp.Body.Close()
		return nil, err
	}
	head = head[:n]
	mime := ""
	switch strings.ToLower(path.Ext(p)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp":
		mime = hub.ImageMIME(head)
	}
	body := io.MultiReader(bytes.NewReader(head), resp.Body)
	return &hub.MediaFile{Name: hub.CleanName(path.Base(p)), MIME: mime, Size: resp.ContentLength,
		Body: readCloser{&capReader{r: body, left: hub.MaxMediaBytes}, resp.Body}}, nil
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

// names reports whether a reply or a browser_screenshot result of the
// session names p: the snapshot's transcript or the events since.
func (ls *live) names(p string) bool {
	ls.mu.Lock()
	snap := ls.snapshot.Data
	ring := ls.ring[:len(ls.ring):len(ls.ring)]
	ls.mu.Unlock()
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
			Name string `json:"name"`
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
		case "assistant":
			for _, c := range r.ToolCalls {
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
			Result json.RawMessage `json:"result"`
		}
		_ = json.Unmarshal(e.Data, &d)
		text := ""
		if d.Text != nil {
			text = *d.Text
		}
		switch e.Kind {
		case hub.KindMessageStart:
			reply.Reset()
		case hub.KindMessageDelta:
			reply.WriteString(text)
		case "message.interim":
			if replyNames(text, p) {
				return true
			}
		case hub.KindMessageComplete:
			if text == "" {
				text = reply.String()
			}
			reply.Reset()
			if replyNames(text, p) {
				return true
			}
		case hub.KindToolComplete:
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
	return false
}

func replyNames(text, p string) bool {
	if !strings.Contains(text, p) {
		return false
	}
	paths, _ := hub.MediaDirectives(text)
	for _, x := range paths {
		if x == p {
			return true
		}
	}
	return false
}

func screenshotNames(result, p string) bool {
	var m map[string]any
	return json.Unmarshal([]byte(result), &m) == nil && hub.ScreenshotPath(m) == p
}
