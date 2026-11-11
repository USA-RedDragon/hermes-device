package device

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
)

// Tree is the plugin's listing of a folder of the device that serves a
// session (docs/protocol.md section 9, listing rules).
type Tree struct {
	Path     string `json:"path"`
	Dir      string `json:"dir"`
	DeviceID string `json:"device_id"`
	Entries  []struct {
		Name string `json:"name"`
		Kind string `json:"kind"`
		Link bool   `json:"link"`
		Size int64  `json:"size"`
	} `json:"entries"`
	Truncated bool `json:"truncated"`
}

// Diff is the plugin's git diff of a session's folder (docs/protocol.md
// section 9, diff rules).
type Diff struct {
	Dir      string `json:"dir"`
	DeviceID string `json:"device_id"`
	Git      bool   `json:"git"`
	Refused  bool   `json:"refused"`
	Files    []struct {
		Path   string `json:"path"`
		Status string `json:"status"`
	} `json:"files"`
	Untracked []string `json:"untracked"`
	Diff      string   `json:"diff"`
	Truncated bool     `json:"truncated"`
	Hidden    int      `json:"hidden"`
}

// Changes are the files the agent wrote in a session (docs/protocol.md
// section 9.2).
type Changes struct {
	SessionID string `json:"session_id"`
	Files     []struct {
		Path   string   `json:"path"`
		Tools  []string `json:"tools"`
		Count  int      `json:"count"`
		LastAt float64  `json:"last_at"`
		RowID  int64    `json:"row_id"`
	} `json:"files"`
	Truncated bool `json:"truncated"`
}

// SearchHit is one matching message of a search (docs/protocol.md
// section 9.2).
type SearchHit struct {
	RowID     int64   `json:"row_id"`
	SessionID string  `json:"session_id"`
	Role      string  `json:"role"`
	Snippet   string  `json:"snippet"`
	Timestamp float64 `json:"timestamp"`
}

// SearchChat is one chat of a search's results.
type SearchChat struct {
	SessionID  string      `json:"session_id"`
	Title      string      `json:"title"`
	Source     string      `json:"source"`
	StartedAt  float64     `json:"started_at"`
	LastActive float64     `json:"last_active"`
	Archived   bool        `json:"archived"`
	Hits       []SearchHit `json:"hits"`
}

// Counters are the token counters of the usage route.
type Counters struct {
	Input      int64   `json:"input"`
	Output     int64   `json:"output"`
	CacheRead  int64   `json:"cache_read"`
	CacheWrite int64   `json:"cache_write"`
	Reasoning  int64   `json:"reasoning"`
	Calls      int64   `json:"calls"`
	CostUSD    float64 `json:"cost_usd"`
}

// Usage is the plugin's token use per session and model (docs/protocol.md
// section 9.2).
type Usage struct {
	Since    float64 `json:"since"`
	Sessions []struct {
		SessionID  string  `json:"session_id"`
		ChatID     string  `json:"chat_id"`
		Title      string  `json:"title"`
		Source     string  `json:"source"`
		Model      string  `json:"model"`
		Provider   string  `json:"provider"`
		LastActive float64 `json:"last_active"`
		Counters
	} `json:"sessions"`
	Models []struct {
		Model    string `json:"model"`
		Provider string `json:"provider"`
		Counters
	} `json:"models"`
	Truncated bool `json:"truncated"`
}

func (p *Plugin) slow() func(context.Context, string, string, []byte) (*http.Response, error) {
	if s, ok := p.r.(slowRequester); ok {
		return s.RequestSlow
	}
	return p.r.Request
}

// Tree lists a folder of the device that serves a session; "" is the
// session's folder. Only ctx bounds it.
func (p *Plugin) Tree(ctx context.Context, sessionID, path string) (Tree, error) {
	var out Tree
	q := ""
	if path != "" {
		q = "?path=" + url.QueryEscape(path)
	}
	return out, p.callWith(ctx, p.slow(), http.MethodGet, "/sessions/"+url.PathEscape(sessionID)+"/tree"+q, nil, &out)
}

// Diff returns what git shows as changed in a session's folder. Only ctx
// bounds it.
func (p *Plugin) Diff(ctx context.Context, sessionID string) (Diff, error) {
	var out Diff
	return out, p.callWith(ctx, p.slow(), http.MethodGet, "/sessions/"+url.PathEscape(sessionID)+"/diff", nil, &out)
}

// Changes lists the files the agent wrote in a session.
func (p *Plugin) Changes(ctx context.Context, sessionID string) (Changes, error) {
	var out Changes
	return out, p.call(ctx, http.MethodGet, "/sessions/"+url.PathEscape(sessionID)+"/changes", nil, &out)
}

// Search searches the messages of every chat of the backend.
func (p *Plugin) Search(ctx context.Context, query string, limit int) ([]SearchChat, error) {
	var out struct {
		Results []SearchChat `json:"results"`
	}
	path := "/search?q=" + url.QueryEscape(query) + "&limit=" + strconv.Itoa(limit)
	return out.Results, p.callWith(ctx, p.slow(), http.MethodGet, path, nil, &out)
}

// Usage returns the token use of the sessions active in the last days
// days.
func (p *Plugin) Usage(ctx context.Context, days int) (Usage, error) {
	var out Usage
	return out, p.callWith(ctx, p.slow(), http.MethodGet, "/usage?days="+strconv.Itoa(days), nil, &out)
}
