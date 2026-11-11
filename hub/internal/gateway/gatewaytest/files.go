package gatewaytest

import (
	"encoding/json"
	"net/http"
	"sync"
)

// fileStore is the fake's server files, served as hermes_cli/web_routers/
// files.py does: POST /api/files/ticket mints a ticket bound to one path,
// and GET /api/fs/download with that ticket streams the file.
type fileStore struct {
	mu      sync.Mutex
	files   map[string][]byte
	tickets map[string]string
	reads   []string
}

// SetFile puts a file on the fake's server.
func (g *Gateway) SetFile(path string, data []byte) {
	g.files.mu.Lock()
	defer g.files.mu.Unlock()
	if g.files.files == nil {
		g.files.files = map[string][]byte{}
	}
	g.files.files[path] = data
}

// FileReads lists the paths downloaded, in order.
func (g *Gateway) FileReads() []string {
	g.files.mu.Lock()
	defer g.files.mu.Unlock()
	return append([]string(nil), g.files.reads...)
}

func (g *Gateway) fileTicket(w http.ResponseWriter, r *http.Request) {
	var p struct {
		Route string `json:"route"`
		Path  string `json:"path"`
	}
	if json.NewDecoder(r.Body).Decode(&p) != nil || p.Route != "download" || p.Path == "" {
		w.WriteHeader(http.StatusUnprocessableEntity)
		return
	}
	g.files.mu.Lock()
	_, ok := g.files.files[p.Path]
	t := randHex()
	if ok {
		if g.files.tickets == nil {
			g.files.tickets = map[string]string{}
		}
		g.files.tickets[t] = p.Path
	}
	g.files.mu.Unlock()
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"ticket":"` + t + `"}`))
}

func (g *Gateway) fileDownload(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	g.files.mu.Lock()
	want, ok := g.files.tickets[q.Get("ticket")]
	delete(g.files.tickets, q.Get("ticket"))
	data, exists := g.files.files[want]
	if ok && exists && len(q) == 2 && q.Get("path") == want {
		g.files.reads = append(g.files.reads, want)
	} else {
		ok = false
	}
	g.files.mu.Unlock()
	if !ok {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", "attachment")
	_, _ = w.Write(data)
}
