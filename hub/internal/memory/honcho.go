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
	"golang.org/x/sync/errgroup"
)

const (
	maxHonchoBody = 8 << 20
	// pageSize is the page size of the Learned tab and of exports, the
	// most Honcho allows.
	pageSize = 100
	// maxExportPages bounds an export at 100,000 conclusions.
	maxExportPages = 1000
	// searchSize is how many results a search returns for each observer,
	// the most Honcho allows.
	searchSize = 100
	// maxReadPages bounds the pages read to pair copies and count facts,
	// at 10,000 stored conclusions.
	maxReadPages = 100
	// readWorkers is how many pages are read at once.
	readWorkers = 8
	// maxFact bounds the text of a fact a user adds.
	maxFact = 4000
)

const (
	fieldObserver = "observer_id"
	fieldObserved = "observed_id"
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
	Level      string    `json:"level"`
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
	out := hub.Conclusion{ID: c.ID, Content: c.Content, View: h.view(c), Level: level(c.Level), CreatedAt: c.CreatedAt}
	if c.SessionID != nil {
		out.SessionID = *c.SessionID
	}
	return out
}

// filters selects the conclusions Hermes or the user drew about the user.
// Conclusions of other peers in the workspace never show.
func (h *honcho) filters() map[string]any {
	return map[string]any{fieldObserved: h.user, fieldObserver: map[string][]string{"in": {h.ai, h.user}}}
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

// list reads one page of the conclusions about the user, oldest first.
func (h *honcho) list(ctx context.Context, page int) (pageJSON, error) {
	q := url.Values{"page": {strconv.Itoa(page)}, "size": {strconv.Itoa(pageSize)}, "reverse": {"true"}}
	var out pageJSON
	err := h.do(ctx, http.MethodPost, h.path("conclusions", "list"), msgNoWorkspace, q, map[string]any{"filters": h.filters()}, &out)
	return out, err
}

// query searches by meaning the conclusions one observer drew about the
// user. Honcho's search takes one observer only. Without all, it finds
// only conclusions Honcho condensed in its dreams.
func (h *honcho) query(ctx context.Context, observer, text string, all bool) ([]conclusionJSON, error) {
	f := map[string]any{fieldObserved: h.user, fieldObserver: observer}
	if !all {
		f["level"] = map[string][]string{"in": condensedLevels()}
	}
	var out []conclusionJSON
	err := h.do(ctx, http.MethodPost, h.path("conclusions", "query"), msgNoWorkspace, nil,
		map[string]any{"query": text, "top_k": searchSize, "filters": f}, &out)
	return out, err
}

// observers are the observers of a view.
func (h *honcho) observers(view string) []string {
	switch view {
	case hub.ViewHermes:
		return []string{h.ai}
	case hub.ViewSelf:
		return []string{h.user}
	}
	return []string{h.ai, h.user}
}

// readAll reads the stored conclusions about the user, oldest first. With
// more than maxReadPages pages it reads only the newest pages and reports
// partial.
func (h *honcho) readAll(ctx context.Context) ([]conclusionJSON, bool, error) {
	first, err := h.list(ctx, 1)
	if err != nil || first.Pages <= 1 {
		return first.Items, false, err
	}
	start := max(first.Pages-maxReadPages+1, 1)
	pages := make([][]conclusionJSON, first.Pages-start+1)
	if start == 1 {
		pages[0] = first.Items
	}
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(readWorkers)
	for p := max(start, 2); p <= first.Pages; p++ {
		g.Go(func() error {
			r, err := h.list(gctx, p)
			pages[p-start] = r.Items
			return err
		})
	}
	if err := g.Wait(); err != nil {
		return nil, false, err
	}
	var out []conclusionJSON
	for _, p := range pages {
		out = append(out, p...)
	}
	return out, start > 1, nil
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
	body := map[string]any{"conclusions": []map[string]string{{fieldContent: content, fieldObserver: h.ai, fieldObserved: h.user}}}
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

// queueJSON is Honcho's GET /queue/status answer.
type queueJSON struct {
	Total      int64 `json:"total_work_units"`
	Completed  int64 `json:"completed_work_units"`
	InProgress int64 `json:"in_progress_work_units"`
	Pending    int64 `json:"pending_work_units"`
}

// queue reads the workspace's queue of work: the messages Honcho has not
// yet turned into memory.
func (h *honcho) queue(ctx context.Context) (hub.Backlog, error) {
	var out queueJSON
	err := h.do(ctx, http.MethodGet, h.path("queue", "status"), msgNoWorkspace, nil, nil, &out)
	return hub.Backlog{Total: out.Total, Completed: out.Completed, InProgress: out.InProgress, Pending: out.Pending}, err
}
