package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"time"

	"github.com/USA-RedDragon/hermes-hub/internal/config"
	"github.com/USA-RedDragon/hermes-hub/internal/hub"
)

const (
	maxHonchoBody = 8 << 20
	// pageSize is the page size of the Learned tab and of exports, the
	// most Honcho allows.
	pageSize = 100
	// maxExportPages bounds an export at 100,000 conclusions.
	maxExportPages = 1000
	// searchSize is how many results a search returns, the most Honcho
	// allows.
	searchSize = 100
	// maxFact bounds the text of a fact a user adds.
	maxFact = 4000
)

const (
	msgNoFact      = "Honcho has no such fact. It may be deleted already"
	msgNoWorkspace = "Honcho does not know this workspace or peer. Check backends[].honcho in the hub config"
)

var conclusionIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// honcho is a client of one workspace of the Honcho API, for one user and
// their Hermes.
type honcho struct {
	base, ws, user, ai, key string
	http                    *http.Client
}

func newHoncho(cfg config.Honcho, key string, c *http.Client) *honcho {
	return &honcho{base: cfg.URL, ws: cfg.Workspace, user: cfg.UserPeer, ai: cfg.AIPeer, key: key, http: c}
}

type conclusionJSON struct {
	ID         string    `json:"id"`
	Content    string    `json:"content"`
	ObserverID string    `json:"observer_id"`
	ObservedID string    `json:"observed_id"`
	SessionID  *string   `json:"session_id"`
	CreatedAt  time.Time `json:"created_at"`
}

type pageJSON struct {
	Items []conclusionJSON `json:"items"`
	Total int              `json:"total"`
	Page  int              `json:"page"`
	Pages int              `json:"pages"`
}

func (h *honcho) view(c conclusionJSON) string {
	switch c.ObserverID {
	case h.ai:
		return hub.ViewHermes
	case h.user:
		return hub.ViewSelf
	}
	return hub.ViewAll
}

func (h *honcho) conclusion(c conclusionJSON) hub.Conclusion {
	out := hub.Conclusion{ID: c.ID, Content: c.Content, View: h.view(c), CreatedAt: c.CreatedAt}
	if c.SessionID != nil {
		out.SessionID = *c.SessionID
	}
	return out
}

// filters selects conclusions Hermes or the user drew about the user, in
// one view. Conclusions of other peers in the workspace never show.
func (h *honcho) filters(view string) map[string]any {
	f := map[string]any{"observed_id": h.user}
	switch view {
	case hub.ViewHermes:
		f["observer_id"] = h.ai
	case hub.ViewSelf:
		f["observer_id"] = h.user
	default:
		f["observer_id"] = map[string][]string{"in": {h.ai, h.user}}
	}
	return f
}

// about is true for a conclusion Hermes or the user drew about the user.
func (h *honcho) about(c conclusionJSON) bool {
	return c.ObservedID == h.user && (c.ObserverID == h.ai || c.ObserverID == h.user)
}

func (h *honcho) path(parts ...string) string {
	p := "/v3/workspaces/" + url.PathEscape(h.ws)
	for _, s := range parts {
		p += "/" + url.PathEscape(s)
	}
	return p
}

// do sends one request. notFound is the message for a 404.
func (h *honcho) do(ctx context.Context, method, path, notFound string, q url.Values, body, out any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	u := h.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, r)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+h.key)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := h.http.Do(req)
	if err != nil {
		slog.Warn("Honcho request failed", "workspace", h.ws, "method", method, "path", path, "error", err.Error())
		return &hub.MemoryError{Status: http.StatusBadGateway, Message: "Could not reach Honcho. Try again later"}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxHonchoBody))
	if err != nil {
		return &hub.MemoryError{Status: http.StatusBadGateway, Message: "Honcho's answer was cut off"}
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return &hub.MemoryError{Status: http.StatusNotFound, Message: notFound}
	case resp.StatusCode >= 300:
		slog.Warn("Honcho refused a request", "workspace", h.ws, "method", method, "path", path, "status", resp.StatusCode)
		return &hub.MemoryError{Status: http.StatusBadGateway, Message: "Honcho refused the request (status " + strconv.Itoa(resp.StatusCode) + ")"}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return &hub.MemoryError{Status: http.StatusBadGateway, Message: "Honcho's answer was not understood"}
	}
	return nil
}

func (h *honcho) list(ctx context.Context, view string, page, size int, oldest bool) (pageJSON, error) {
	q := url.Values{"page": {strconv.Itoa(page)}, "size": {strconv.Itoa(size)}}
	if oldest {
		q.Set("reverse", "true")
	}
	var out pageJSON
	err := h.do(ctx, http.MethodPost, h.path("conclusions", "list"), msgNoWorkspace, q, map[string]any{"filters": h.filters(view)}, &out)
	return out, err
}

func (h *honcho) query(ctx context.Context, view, text string) ([]conclusionJSON, error) {
	var out []conclusionJSON
	err := h.do(ctx, http.MethodPost, h.path("conclusions", "query"), msgNoWorkspace, nil,
		map[string]any{"query": text, "top_k": searchSize, "filters": h.filters(view)}, &out)
	return out, err
}

func (h *honcho) get(ctx context.Context, id string) (conclusionJSON, error) {
	var out conclusionJSON
	err := h.do(ctx, http.MethodGet, h.path("conclusions", id), msgNoFact, nil, nil, &out)
	return out, err
}

func (h *honcho) remove(ctx context.Context, id string) error {
	return h.do(ctx, http.MethodDelete, h.path("conclusions", id), msgNoFact, nil, nil, nil)
}

func (h *honcho) create(ctx context.Context, content string) (conclusionJSON, error) {
	var out []conclusionJSON
	body := map[string]any{"conclusions": []map[string]string{{fieldContent: content, "observer_id": h.ai, "observed_id": h.user}}}
	if err := h.do(ctx, http.MethodPost, h.path("conclusions"), msgNoWorkspace, nil, body, &out); err != nil {
		return conclusionJSON{}, err
	}
	if len(out) != 1 {
		return conclusionJSON{}, &hub.MemoryError{Status: http.StatusBadGateway, Message: "Honcho did not save the fact"}
	}
	return out[0], nil
}

// card is Hermes's peer card of the user, else the user's own card. A
// peer Honcho does not know has no card.
func (h *honcho) card(ctx context.Context) ([]string, error) {
	for _, c := range []struct {
		peer string
		q    url.Values
	}{{h.ai, url.Values{"target": {h.user}}}, {h.user, nil}} {
		var out struct {
			PeerCard []string `json:"peer_card"`
		}
		err := h.do(ctx, http.MethodGet, h.path("peers", c.peer, "card"), msgNoWorkspace, c.q, nil, &out)
		var me *hub.MemoryError
		if errors.As(err, &me) && me.Status == http.StatusNotFound {
			continue
		}
		if err != nil {
			return nil, err
		}
		if len(out.PeerCard) > 0 {
			return out.PeerCard, nil
		}
	}
	return []string{}, nil
}
