// Package memory serves the hub's Memory page: Hermes's built-in memory
// (MEMORY.md and USER.md, through the hermes-device plugin's memory routes)
// and what Honcho learned about the user.
package memory

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/USA-RedDragon/hermes-hub/internal/config"
	"github.com/USA-RedDragon/hermes-hub/internal/device"
	"github.com/USA-RedDragon/hermes-hub/internal/hub"
	"golang.org/x/sync/errgroup"
)

// Access says which backends a user may use. *service.Service is one.
type Access interface {
	Allowed(ctx context.Context, user, backendID string) error
	Backends(ctx context.Context, user string) ([]hub.Backend, error)
}

// Options are the dependencies of a Service.
type Options struct {
	Access   Access
	Backends []config.Backend
	// Requesters reach each backend's dashboard with the hub's session.
	Requesters map[string]device.Requester
	// HTTPClient reaches Honcho. Default: a client with a 30s timeout.
	HTTPClient *http.Client
	// ListTimeout bounds each backend's answer to MemoryBackends. Default
	// 5s.
	ListTimeout time.Duration
}

type backend struct {
	notes  *notes
	honcho *honcho
}

// Service implements hub.Memory.
type Service struct {
	access      Access
	backends    map[string]*backend
	listTimeout time.Duration
}

var _ hub.Memory = (*Service)(nil)

var (
	pendingIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	projectIDPattern = regexp.MustCompile(`^[A-Za-z0-9_~-][A-Za-z0-9._~-]{0,254}$`)
)

// New returns a Service. It reads each backend's Honcho API key.
func New(opts Options) (*Service, error) {
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
	if opts.ListTimeout == 0 {
		opts.ListTimeout = 5 * time.Second
	}
	s := &Service{access: opts.Access, backends: map[string]*backend{}, listTimeout: opts.ListTimeout}
	for _, b := range opts.Backends {
		mb := &backend{}
		if r := opts.Requesters[b.ID]; r != nil {
			mb.notes = &notes{r: r}
		}
		if b.Honcho.URL != "" {
			key, err := b.Honcho.Key()
			if err != nil {
				return nil, err
			}
			mb.honcho = newHoncho(b.Honcho, key, opts.HTTPClient)
		}
		s.backends[b.ID] = mb
	}
	return s, nil
}

func (s *Service) backend(ctx context.Context, user, id string) (*backend, error) {
	if err := s.access.Allowed(ctx, user, id); err != nil {
		return nil, err
	}
	b, ok := s.backends[id]
	if !ok {
		return nil, hub.ErrUnknownBackend
	}
	return b, nil
}

func (s *Service) notes(ctx context.Context, user, id string) (*notes, error) {
	b, err := s.backend(ctx, user, id)
	if err != nil {
		return nil, err
	}
	if b.notes == nil {
		return nil, hub.ErrNoMemory
	}
	return b.notes, nil
}

func (s *Service) honcho(ctx context.Context, user, id string) (*honcho, error) {
	b, err := s.backend(ctx, user, id)
	if err != nil {
		return nil, err
	}
	if b.honcho == nil {
		return nil, hub.ErrNoMemory
	}
	return b.honcho, nil
}

// MemoryBackends lists the user's backends that have notes or learned
// facts, with the number of memory writes waiting for approval. A backend
// whose Hermes cannot be asked shows with Error.
func (s *Service) MemoryBackends(ctx context.Context, user string) ([]hub.MemoryBackend, error) {
	bs, err := s.access.Backends(ctx, user)
	if err != nil {
		return nil, err
	}
	out := make([]hub.MemoryBackend, len(bs))
	keep := make([]bool, len(bs))
	var wg sync.WaitGroup
	for i, hb := range bs {
		b := s.backends[hb.ID]
		if b == nil {
			continue
		}
		out[i] = hub.MemoryBackend{ID: hb.ID, Name: hb.Name, Learned: b.honcho != nil}
		keep[i] = b.honcho != nil
		if b.notes == nil {
			continue
		}
		wg.Go(func() {
			cctx, cancel := context.WithTimeout(ctx, s.listTimeout)
			defer cancel()
			n, err := b.notes.get(cctx)
			switch {
			case err == nil:
				out[i].Notes, out[i].Pending, keep[i] = true, len(n.Pending), true
			case errors.Is(err, hub.ErrNoMemory):
			default:
				out[i].Error, keep[i] = err.Error(), true
			}
		})
	}
	wg.Wait()
	list := []hub.MemoryBackend{}
	for i := range out {
		if keep[i] {
			list = append(list, out[i])
		}
	}
	return list, nil
}

// Notes reads the backend's MEMORY.md, USER.md and pending writes.
func (s *Service) Notes(ctx context.Context, user, backendID string) (hub.Notes, error) {
	n, err := s.notes(ctx, user, backendID)
	if err != nil {
		return hub.Notes{}, err
	}
	return n.get(ctx)
}

// ChangeNote applies one change to MEMORY.md or USER.md.
func (s *Service) ChangeNote(ctx context.Context, user, backendID, target, op, entry, content string, base []string) (hub.Notes, error) {
	if target != hub.TargetMemory && target != hub.TargetUser {
		return hub.Notes{}, &hub.MemoryError{Status: http.StatusNotFound, Message: "unknown memory file"}
	}
	n, err := s.notes(ctx, user, backendID)
	if err != nil {
		return hub.Notes{}, err
	}
	return n.change(ctx, target, op, entry, content, base)
}

// ResolvePending approves or rejects a memory write Hermes staged.
func (s *Service) ResolvePending(ctx context.Context, user, backendID, pendingID string, approve bool) (hub.Notes, error) {
	n, err := s.notes(ctx, user, backendID)
	if err != nil {
		return hub.Notes{}, err
	}
	if !pendingIDPattern.MatchString(pendingID) {
		return hub.Notes{}, &hub.MemoryError{Status: http.StatusNotFound, Message: "That change is no longer waiting"}
	}
	return n.resolve(ctx, pendingID, approve)
}

// SetApproval turns on or off whether Hermes asks before it saves to
// memory.
func (s *Service) SetApproval(ctx context.Context, user, backendID string, on bool) (hub.Notes, error) {
	n, err := s.notes(ctx, user, backendID)
	if err != nil {
		return hub.Notes{}, err
	}
	if err := n.setApproval(ctx, on); err != nil {
		return hub.Notes{}, err
	}
	return n.get(ctx)
}

// Projects lists the projects of the backend's project-memory plugin.
func (s *Service) Projects(ctx context.Context, user, backendID string) ([]hub.Project, error) {
	n, err := s.notes(ctx, user, backendID)
	if err != nil {
		return nil, err
	}
	return n.projects(ctx)
}

func (s *Service) project(ctx context.Context, user, backendID, projectID string) (*notes, error) {
	n, err := s.notes(ctx, user, backendID)
	if err != nil {
		return nil, err
	}
	if !projectIDPattern.MatchString(projectID) {
		return nil, &hub.MemoryError{Status: http.StatusNotFound, Message: "Hermes has no such project"}
	}
	return n, nil
}

// ProjectNotes reads a project's MEMORY.md.
func (s *Service) ProjectNotes(ctx context.Context, user, backendID, projectID string) (hub.ProjectNotes, error) {
	n, err := s.project(ctx, user, backendID, projectID)
	if err != nil {
		return hub.ProjectNotes{}, err
	}
	return n.projectNotes(ctx, projectID)
}

// ChangeProjectNote applies one change to a project's MEMORY.md.
func (s *Service) ChangeProjectNote(ctx context.Context, user, backendID, projectID, op, entry, content string, base []string) (hub.ProjectNotes, error) {
	n, err := s.project(ctx, user, backendID, projectID)
	if err != nil {
		return hub.ProjectNotes{}, err
	}
	return n.changeProject(ctx, projectID, op, entry, content, base)
}

func view(v string) (string, error) {
	switch v {
	case "", hub.ViewAll:
		return hub.ViewAll, nil
	case hub.ViewHermes, hub.ViewSelf:
		return v, nil
	}
	return "", &hub.MemoryError{Status: http.StatusBadRequest, Message: "view must be all, hermes or self"}
}

// Learned pages through or searches the distinct facts about the user.
// It reads every stored copy to pair them, so counts and pages are of
// distinct facts.
func (s *Service) Learned(ctx context.Context, user, backendID string, q hub.LearnedQuery) (hub.LearnedPage, error) {
	h, err := s.honcho(ctx, user, backendID)
	if err != nil {
		return hub.LearnedPage{}, err
	}
	v, err := view(q.View)
	if err != nil {
		return hub.LearnedPage{}, err
	}
	cs, partial, err := h.readAll(ctx)
	if err != nil {
		return hub.LearnedPage{}, err
	}
	all, byKey := h.facts(cs)
	out := hub.LearnedPage{Items: []hub.Conclusion{}, Partial: partial}
	if text := strings.TrimSpace(q.Query); text != "" {
		if r := []rune(text); len(r) > 500 {
			text = string(r[:500])
		}
		hits, err := s.search(ctx, h, v, text, q.All)
		if err != nil {
			return hub.LearnedPage{}, err
		}
		var found []*fact
		for _, c := range hits {
			k := factKey(c.Content)
			f := byKey[k]
			switch {
			case f == nil:
				f = h.newFact(c)
				byKey[k] = f
			case !slices.Contains(f.IDs, c.ID):
				f.IDs = append(f.IDs, c.ID)
				f.add(h, c)
			}
			if !slices.Contains(found, f) {
				found = append(found, f)
			}
		}
		for _, f := range found {
			if f.in(v) && (q.All || condensed(f.Level)) {
				out.Items = append(out.Items, f.Conclusion)
				out.Copies += len(f.IDs)
			}
		}
		out.Total, out.Page = len(out.Items), 1
		return out, nil
	}
	var pick []*fact
	for _, f := range all {
		switch {
		case !f.in(v):
		case q.All || condensed(f.Level):
			pick = append(pick, f)
			out.Copies += len(f.IDs)
		default:
			out.Hidden++
		}
	}
	if !q.Oldest {
		slices.Reverse(pick)
	}
	out.Total, out.Page = len(pick), max(q.Page, 1)
	out.Pages = (len(pick) + pageSize - 1) / pageSize
	lo := min((out.Page-1)*pageSize, len(pick))
	for _, f := range pick[lo:min(lo+pageSize, len(pick))] {
		out.Items = append(out.Items, f.Conclusion)
	}
	return out, nil
}

// search runs one search for each observer of the view, since Honcho's
// search takes one observer, and takes their results in turn.
func (s *Service) search(ctx context.Context, h *honcho, v, text string, all bool) ([]conclusionJSON, error) {
	obs := h.observers(v)
	res := make([][]conclusionJSON, len(obs))
	g, gctx := errgroup.WithContext(ctx)
	for i, o := range obs {
		g.Go(func() error {
			r, err := h.query(gctx, o, text, all)
			res[i] = r
			return err
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	var out []conclusionJSON
	for i := 0; ; i++ {
		more := false
		for _, r := range res {
			if i < len(r) {
				more = true
				if h.about(r[i]) {
					out = append(out, r[i])
				}
			}
		}
		if !more {
			return out, nil
		}
	}
}

// LearnedInfo counts the distinct facts and their stored copies and reads
// the profile card.
func (s *Service) LearnedInfo(ctx context.Context, user, backendID string) (hub.LearnedInfo, error) {
	h, err := s.honcho(ctx, user, backendID)
	if err != nil {
		return hub.LearnedInfo{}, err
	}
	cs, partial, err := h.readAll(ctx)
	if err != nil {
		return hub.LearnedInfo{}, err
	}
	out := hub.LearnedInfo{Workspace: h.ws, Partial: partial}
	facts, _ := h.facts(cs)
	for _, f := range facts {
		out.Total++
		out.Copies += len(f.IDs)
		if f.hermes {
			out.Hermes++
		}
		if f.self {
			out.Self++
		}
		if f.hermes && f.self {
			out.Both++
		}
		if condensed(f.Level) {
			out.Condensed++
		} else {
			out.Raw++
		}
	}
	card, err := h.card(ctx)
	if err != nil {
		return hub.LearnedInfo{}, err
	}
	out.Card = card
	if out.Card == nil {
		out.Card = []string{}
	}
	return out, nil
}

// AddLearned saves a fact about the user as Hermes's view.
func (s *Service) AddLearned(ctx context.Context, user, backendID, content string) (hub.Conclusion, error) {
	h, err := s.honcho(ctx, user, backendID)
	if err != nil {
		return hub.Conclusion{}, err
	}
	content = strings.TrimSpace(content)
	if content == "" || utf8.RuneCountInString(content) > maxFact {
		return hub.Conclusion{}, &hub.MemoryError{Status: http.StatusBadRequest, Message: "A fact must be 1 to 4,000 characters"}
	}
	c, err := h.create(ctx, content)
	if err != nil {
		return hub.Conclusion{}, err
	}
	return h.newFact(c).Conclusion, nil
}

// DeleteLearned deletes stored conclusions by id. The page sends every
// copy of a fact, and each copy counts toward hub.MaxLearnedDelete. It
// reads each one first and deletes only those Hermes or the user drew
// about the user, so an id of any other conclusion is refused. It stops at
// the first failure.
func (s *Service) DeleteLearned(ctx context.Context, user, backendID string, ids []string) (int, error) {
	h, err := s.honcho(ctx, user, backendID)
	if err != nil {
		return 0, err
	}
	if len(ids) == 0 || len(ids) > hub.MaxLearnedDelete {
		return 0, &hub.MemoryError{Status: http.StatusBadRequest, Message: "Pick 1 to 100 facts to delete"}
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if !conclusionIDPattern.MatchString(id) || seen[id] {
			return 0, &hub.MemoryError{Status: http.StatusBadRequest, Message: "bad fact id"}
		}
		seen[id] = true
	}
	n := 0
	for _, id := range ids {
		c, err := h.get(ctx, id)
		if err != nil {
			return n, err
		}
		if c.ID != id || !h.about(c) {
			return n, &hub.MemoryError{Status: http.StatusNotFound, Message: "Honcho has no such fact"}
		}
		if err := h.remove(ctx, id); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// ExportLearned writes every conclusion about the user, oldest first, as
// a JSON array. It pages oldest first, so facts Honcho adds meanwhile go to
// the end instead of shifting the pages, and it skips ids it already wrote.
func (s *Service) ExportLearned(ctx context.Context, user, backendID string, w io.Writer) error {
	h, err := s.honcho(ctx, user, backendID)
	if err != nil {
		return err
	}
	p, err := h.list(ctx, 1)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(w, "["); err != nil {
		return err
	}
	enc := json.NewEncoder(w)
	n := 0
	seen := map[string]bool{}
	for page := 1; ; page++ {
		for _, c := range p.Items {
			if !h.about(c) || seen[c.ID] {
				continue
			}
			seen[c.ID] = true
			if n > 0 {
				if _, err := io.WriteString(w, ","); err != nil {
					return err
				}
			}
			if err := enc.Encode(h.conclusion(c)); err != nil {
				return err
			}
			n++
		}
		if page >= p.Pages || page >= maxExportPages || len(p.Items) == 0 {
			break
		}
		if p, err = h.list(ctx, page+1); err != nil {
			return err
		}
	}
	_, err = io.WriteString(w, "]\n")
	return err
}

// Backlog reads Honcho's queue of work for the backend's workspace. It
// fails with hub.ErrNoMemory when the backend has no Honcho.
func (s *Service) Backlog(ctx context.Context, user, backendID string) (hub.Backlog, error) {
	h, err := s.honcho(ctx, user, backendID)
	if err != nil {
		return hub.Backlog{}, err
	}
	return h.queue(ctx)
}
