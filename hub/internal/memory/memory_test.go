package memory_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/USA-RedDragon/talaria/hub/internal/config"
	"github.com/USA-RedDragon/talaria/hub/internal/device"
	"github.com/USA-RedDragon/talaria/hub/internal/hub"
	"github.com/USA-RedDragon/talaria/hub/internal/memory"
	"github.com/USA-RedDragon/talaria/hub/internal/memory/memorytest"
)

const (
	alice  = "alice"
	bob    = "bob"
	key    = "jwt-secret-value"
	pcID   = "pc"
	homeID = "home"

	zstdAlways = "Use zstd always."
)

// access lets alice use pc and bob use home.
type access struct{}

func (access) Allowed(_ context.Context, user, backendID string) error {
	if (user == alice && backendID == pcID) || (user == bob && backendID == homeID) {
		return nil
	}
	if backendID != pcID && backendID != homeID {
		return hub.ErrUnknownBackend
	}
	return hub.ErrForbidden
}

func (access) Backends(_ context.Context, user string) ([]hub.Backend, error) {
	switch user {
	case alice:
		return []hub.Backend{{ID: pcID, Name: "PC"}}, nil
	case bob:
		return []hub.Backend{{ID: homeID, Name: "Home"}}, nil
	}
	return nil, nil
}

func sampleNotes() hub.Notes {
	return hub.Notes{Targets: map[string]hub.NoteStore{
		hub.TargetMemory: {File: "/data/memories/MEMORY.md", Limit: 2200, Used: 9, Enabled: true, Entries: []string{"Use zstd."}},
		hub.TargetUser:   {File: "/data/memories/USER.md", Limit: 1375, Enabled: true},
	}, Pending: []hub.PendingNote{{ID: "ab12", Target: hub.TargetMemory, Changes: []hub.NoteChange{{Action: "add", Content: "Backups at 02:00."}}}}}
}

type env struct {
	svc      *memory.Service
	pc, home *memorytest.Plugin
	honcho   *memorytest.Honcho
}

func setup(t *testing.T) *env {
	t.Helper()
	pc, home := memorytest.NewPlugin(sampleNotes()), memorytest.NewPlugin(sampleNotes())
	t.Cleanup(pc.Close)
	t.Cleanup(home.Close)
	home.Missing = true
	h := memorytest.NewHoncho("bobs", key)
	t.Cleanup(h.Close)
	backends := []config.Backend{
		{ID: pcID},
		{ID: homeID, Honcho: config.Honcho{URL: h.URL(), Workspace: "bobs", UserPeer: bob, AIPeer: "hermes", APIKey: key}},
	}
	svc, err := memory.New(memory.Options{Access: access{}, Backends: backends,
		Requesters: map[string]device.Requester{pcID: pc, homeID: home}})
	if err != nil {
		t.Fatal(err)
	}
	return &env{svc: svc, pc: pc, home: home, honcho: h}
}

func status(err error) int {
	var me *hub.MemoryError
	if errors.As(err, &me) {
		return me.Status
	}
	return 0
}

func TestBackendsAndAccess(t *testing.T) {
	t.Parallel()
	e := setup(t)
	ctx := context.Background()
	bs, err := e.svc.MemoryBackends(ctx, alice)
	if err != nil || len(bs) != 1 || bs[0].ID != pcID || !bs[0].Notes || bs[0].Learned || bs[0].Pending != 1 {
		t.Fatalf("alice backends = %+v, %v", bs, err)
	}
	bs, err = e.svc.MemoryBackends(ctx, bob)
	if err != nil || len(bs) != 1 || bs[0].ID != homeID || bs[0].Notes || !bs[0].Learned {
		t.Fatalf("bob backends = %+v, %v", bs, err)
	}
	if _, err := e.svc.Notes(ctx, bob, pcID); !errors.Is(err, hub.ErrForbidden) {
		t.Fatalf("bob notes on pc: %v", err)
	}
	if _, err := e.svc.Notes(ctx, bob, homeID); !errors.Is(err, hub.ErrNoMemory) {
		t.Fatalf("notes without routes: %v", err)
	}
	if _, err := e.svc.Learned(ctx, alice, homeID, hub.LearnedQuery{}); !errors.Is(err, hub.ErrForbidden) {
		t.Fatalf("alice learned on home: %v", err)
	}
	if _, err := e.svc.Learned(ctx, alice, pcID, hub.LearnedQuery{}); !errors.Is(err, hub.ErrNoMemory) {
		t.Fatalf("learned without honcho: %v", err)
	}
	if _, err := e.svc.ChangeNote(ctx, bob, pcID, hub.TargetMemory, hub.NoteAdd, "", "x", nil); !errors.Is(err, hub.ErrForbidden) {
		t.Fatalf("bob change on pc: %v", err)
	}
	if n := len(e.pc.Requests()); n != 1 {
		t.Fatalf("pc plugin got %d requests, want only alice's listing", n)
	}
	if n := len(e.honcho.Requests()); n != 0 {
		t.Fatalf("honcho got %d requests", n)
	}
}

func TestNotes(t *testing.T) {
	t.Parallel()
	e := setup(t)
	ctx := context.Background()
	n, err := e.svc.ChangeNote(ctx, alice, pcID, hub.TargetMemory, hub.NoteEdit, "Use zstd.", zstdAlways, nil)
	if err != nil || !slices.Equal(n.Targets[hub.TargetMemory].Entries, []string{zstdAlways}) {
		t.Fatalf("edit = %+v, %v", n, err)
	}
	last := lastPluginRequest(e.pc)
	if last.Method != http.MethodPost || last.Path != "/api/plugins/device/memory/memory/edit" ||
		last.Body["entry"] != "Use zstd." || last.Body["content"] != zstdAlways {
		t.Fatalf("edit request = %+v", last)
	}
	if _, err := e.svc.ChangeNote(ctx, alice, pcID, hub.TargetUser, hub.NoteReplace, "", "a\n§\nb", []string{zstdAlways}); err != nil {
		t.Fatal(err)
	}
	if last := lastPluginRequest(e.pc); last.Body["text"] != "a\n§\nb" || fmt.Sprint(last.Body["base"]) != "["+zstdAlways+"]" {
		t.Fatalf("replace request = %+v", last)
	}
	if _, err := e.svc.ChangeNote(ctx, alice, pcID, "../config", hub.NoteAdd, "", "x", nil); status(err) != http.StatusNotFound {
		t.Fatalf("bad target: %v", err)
	}
	if _, err := e.svc.ChangeNote(ctx, alice, pcID, hub.TargetMemory, "wipe", "", "x", nil); status(err) != http.StatusNotFound {
		t.Fatalf("bad op: %v", err)
	}
	if _, err := e.svc.ResolvePending(ctx, alice, pcID, "../x", true); status(err) != http.StatusNotFound {
		t.Fatalf("bad pending id: %v", err)
	}
	n, err = e.svc.ResolvePending(ctx, alice, pcID, "ab12", true)
	if err != nil || len(n.Pending) != 0 || !slices.Contains(n.Targets[hub.TargetMemory].Entries, "Backups at 02:00.") {
		t.Fatalf("approve = %+v, %v", n, err)
	}
	if _, err := e.svc.ResolvePending(ctx, alice, pcID, "ab12", false); status(err) != http.StatusNotFound {
		t.Fatalf("reject gone: %v", err)
	}
	n, err = e.svc.SetApproval(ctx, alice, pcID, true)
	if err != nil || !n.Approval {
		t.Fatalf("approval = %+v, %v", n, err)
	}
	put := slices.IndexFunc(e.pc.Requests(), func(r memorytest.Request) bool { return r.Method == http.MethodPut })
	if put < 0 || e.pc.Requests()[put].Path != "/api/config" {
		t.Fatalf("no PUT /api/config: %+v", e.pc.Requests())
	}
	e.pc.Refuse = "That would put MEMORY.md over its limit of 2,200 characters."
	_, err = e.svc.ChangeNote(ctx, alice, pcID, hub.TargetMemory, hub.NoteAdd, "", "x", nil)
	if status(err) != http.StatusConflict || !strings.Contains(err.Error(), "2,200") {
		t.Fatalf("refusal = %v", err)
	}
}

const (
	demoID       = "github.com-alice-demo"
	demoIdentity = "github.com/alice/demo"
)

func demoProject() hub.Project {
	return hub.Project{ID: demoID, Identity: demoIdentity, Kind: hub.ProjectRemote, Name: "demo",
		Aliases: []string{"git.example.com/alice/demo"}, Folders: []hub.SeenFolder{{DeviceID: "d1", Device: "laptop", Dir: "/home/alice/src/demo", SeenAt: 1731300000.5}}}
}

func TestProjects(t *testing.T) {
	t.Parallel()
	e := setup(t)
	ctx := context.Background()
	e.pc.AddProject(demoProject(), "Run make check.")
	e.pc.AddProject(hub.Project{ID: "root-abc", Identity: "root:abc", Kind: hub.ProjectRoot, Name: "scratch"})
	ps, err := e.svc.Projects(ctx, alice, pcID)
	if err != nil || len(ps) != 2 || ps[0].Identity != demoIdentity || ps[0].Aliases[0] != "git.example.com/alice/demo" ||
		ps[0].Folders[0].Device != "laptop" || ps[1].Aliases == nil || ps[1].Folders == nil {
		t.Fatalf("projects = %+v, %v", ps, err)
	}
	if last := e.pc.Requests()[len(e.pc.Requests())-1]; last.Method != http.MethodGet || last.Path != "/api/plugins/project-memory/projects" {
		t.Fatalf("list request = %+v", last)
	}
	pn, err := e.svc.ProjectNotes(ctx, alice, pcID, demoID)
	if err != nil || pn.Project.Name != "demo" || !slices.Equal(pn.Targets[hub.TargetMemory].Entries, []string{"Run make check."}) ||
		pn.Targets[hub.TargetUser].Entries == nil || pn.Pending == nil {
		t.Fatalf("project notes = %+v, %v", pn, err)
	}
	before := len(e.pc.Requests())
	for _, id := range []string{"..", "a/b", "", "%2e%2e"} {
		if _, err := e.svc.ProjectNotes(ctx, alice, pcID, id); status(err) != http.StatusNotFound {
			t.Fatalf("bad id %q: %v", id, err)
		}
	}
	if after := len(e.pc.Requests()); after != before {
		t.Fatalf("bad ids reached Hermes: %d requests", after-before)
	}
	_, err = e.svc.ProjectNotes(ctx, alice, pcID, "github.com-bob-other")
	if status(err) != http.StatusNotFound || err.Error() != "no such project" {
		t.Fatalf("unknown project = %v", err)
	}
	if _, err := e.svc.Projects(ctx, bob, pcID); !errors.Is(err, hub.ErrForbidden) {
		t.Fatalf("bob projects on pc: %v", err)
	}
	if _, err := e.svc.ChangeProjectNote(ctx, bob, pcID, demoID, hub.NoteAdd, "", "x", nil); !errors.Is(err, hub.ErrForbidden) {
		t.Fatalf("bob change on pc: %v", err)
	}
	e.pc.NoProjects = true
	if _, err := e.svc.Projects(ctx, alice, pcID); !errors.Is(err, hub.ErrNoMemory) {
		t.Fatalf("projects without the plugin: %v", err)
	}
	if _, err := e.svc.ProjectNotes(ctx, alice, pcID, demoID); !errors.Is(err, hub.ErrNoMemory) {
		t.Fatalf("project notes without the plugin: %v", err)
	}
}

// lastPluginRequest is the last request that was not the project-memory
// plugin's pending list, which the notes routes read after each answer.
func lastPluginRequest(p *memorytest.Plugin) memorytest.Request {
	reqs := p.Requests()
	for i := len(reqs) - 1; i > 0; i-- {
		if reqs[i].Path != "/api/plugins/project-memory/pending" {
			return reqs[i]
		}
	}
	return reqs[0]
}

func TestPendingProjectWrites(t *testing.T) {
	t.Parallel()
	e := setup(t)
	ctx := context.Background()
	n, err := e.svc.Notes(ctx, alice, pcID)
	if err != nil || len(n.Pending) != 1 || n.Pending[0].Project != "" {
		t.Fatalf("untagged notes = %+v, %v", n.Pending, err)
	}
	e.pc.ProjectPending = map[string]string{"ab12": demoIdentity, "zz99": "github.com/alice/other"}
	n, err = e.svc.Notes(ctx, alice, pcID)
	if err != nil || len(n.Pending) != 1 || n.Pending[0].Project != demoIdentity {
		t.Fatalf("tagged notes = %+v, %v", n.Pending, err)
	}
	e.pc.NoProjects = true
	n, err = e.svc.Notes(ctx, alice, pcID)
	if err != nil || len(n.Pending) != 1 || n.Pending[0].Project != "" {
		t.Fatalf("notes without the plugin = %+v, %v", n.Pending, err)
	}
}

func TestProjectChanges(t *testing.T) {
	t.Parallel()
	e := setup(t)
	ctx := context.Background()
	e.pc.AddProject(demoProject(), "Run make check.")
	pn, err := e.svc.ChangeProjectNote(ctx, alice, pcID, demoID, hub.NoteEdit, "Run make check.", "Run make test.", nil)
	if err != nil || !slices.Equal(pn.Targets[hub.TargetMemory].Entries, []string{"Run make test."}) {
		t.Fatalf("edit = %+v, %v", pn, err)
	}
	last := e.pc.Requests()[len(e.pc.Requests())-1]
	if last.Method != http.MethodPost || last.Path != "/api/plugins/project-memory/projects/"+demoID+"/memory/edit" ||
		last.Body["entry"] != "Run make check." || last.Body["content"] != "Run make test." {
		t.Fatalf("edit request = %+v", last)
	}
	if _, err := e.svc.ChangeProjectNote(ctx, alice, pcID, demoID, hub.NoteReplace, "", "a\n§\nb", nil); err != nil {
		t.Fatal(err)
	}
	if last := e.pc.Requests()[len(e.pc.Requests())-1]; last.Body["text"] != "a\n§\nb" || fmt.Sprint(last.Body["base"]) != "[]" {
		t.Fatalf("replace request = %+v", last)
	}
	pn, err = e.svc.ChangeProjectNote(ctx, alice, pcID, demoID, hub.NoteRemove, "a", "", nil)
	if err != nil || !slices.Equal(pn.Targets[hub.TargetMemory].Entries, []string{"b"}) {
		t.Fatalf("remove = %+v, %v", pn, err)
	}
	if _, err := e.svc.ChangeProjectNote(ctx, alice, pcID, demoID, "wipe", "", "x", nil); status(err) != http.StatusNotFound {
		t.Fatalf("bad op: %v", err)
	}
	e.pc.Refuse = "That would put MEMORY.md over its limit of 3,500 characters."
	if _, err := e.svc.ChangeProjectNote(ctx, alice, pcID, demoID, hub.NoteAdd, "", "x", nil); status(err) != http.StatusConflict || !strings.Contains(err.Error(), "3,500") {
		t.Fatalf("refusal = %v", err)
	}
}

func TestLearned(t *testing.T) {
	t.Parallel()
	e := setup(t)
	ctx := context.Background()
	e.honcho.Add("hermes", bob, "Prefers restic over borg.", "local-abc")
	e.honcho.Add(bob, bob, "I keep backups on B2.", "")
	e.honcho.Add("hermes", "hermes", "Hermes runs on a 5090.", "")
	e.honcho.Add("uid-1000", "uid-1000", "stale", "")
	other := e.honcho.Add("carol", bob, "Carol thinks Bob snores.", "")
	p, err := e.svc.Learned(ctx, bob, homeID, hub.LearnedQuery{All: true})
	if err != nil || p.Total != 2 || len(p.Items) != 2 || p.Items[0].View != hub.ViewSelf || p.Items[1].SessionID != "local-abc" ||
		p.Items[0].Level != hub.LevelExplicit {
		t.Fatalf("all = %+v, %v", p, err)
	}
	if _, err := e.svc.DeleteLearned(ctx, bob, homeID, []string{other}); status(err) != http.StatusNotFound {
		t.Fatalf("delete another observer's fact: %v", err)
	}
	p, err = e.svc.Learned(ctx, bob, homeID, hub.LearnedQuery{View: hub.ViewHermes, All: true})
	if err != nil || len(p.Items) != 1 || p.Items[0].Content != "Prefers restic over borg." {
		t.Fatalf("hermes = %+v, %v", p, err)
	}
	p, err = e.svc.Learned(ctx, bob, homeID, hub.LearnedQuery{View: hub.ViewSelf, Oldest: true, All: true})
	if err != nil || len(p.Items) != 1 || p.Items[0].View != hub.ViewSelf {
		t.Fatalf("self = %+v, %v", p, err)
	}
	if _, err := e.svc.Learned(ctx, bob, homeID, hub.LearnedQuery{View: "everyone"}); status(err) != http.StatusBadRequest {
		t.Fatalf("bad view: %v", err)
	}
	e.honcho.Cards["hermes>"+bob] = []string{"Name: Bob", "Uses Flux"}
	info, err := e.svc.LearnedInfo(ctx, bob, homeID)
	if err != nil || info.Workspace != "bobs" || info.Total != 2 || info.Copies != 2 || info.Hermes != 1 || info.Self != 1 ||
		info.Both != 0 || info.Raw != 2 || info.Condensed != 0 || len(info.Card) != 2 {
		t.Fatalf("info = %+v, %v", info, err)
	}
}

func TestLearnedSearch(t *testing.T) {
	t.Parallel()
	e := setup(t)
	ctx := context.Background()
	e.honcho.Add("hermes", bob, "Prefers restic over borg.", "local-abc")
	e.honcho.Add(bob, bob, "I keep backups on B2.", "")
	e.honcho.Add("carol", bob, "Carol keeps backups on tape.", "")
	for _, q := range []string{"restic", "b2"} {
		p, err := e.svc.Learned(ctx, bob, homeID, hub.LearnedQuery{Query: q, All: true})
		if err != nil || len(p.Items) != 1 {
			t.Fatalf("search %q = %+v, %v", q, p, err)
		}
	}
	if p, err := e.svc.Learned(ctx, bob, homeID, hub.LearnedQuery{Query: "backups", All: true}); err != nil || p.Total != 1 {
		t.Fatalf("search must skip other observers: %+v, %v", p, err)
	}
	for _, r := range e.honcho.Requests() {
		if r.Auth != "Bearer "+key {
			t.Fatalf("request without the key: %+v", r)
		}
		f, ok := r.Body["filters"].(map[string]any)
		if ok && (f["observed_id"] != bob || f["observer_id"] == nil) {
			t.Fatalf("request not scoped to the user: %+v", r)
		}
	}
}

func TestLearnedPairs(t *testing.T) {
	t.Parallel()
	e := setup(t)
	ctx := context.Background()
	a := e.honcho.Add("hermes", bob, "Bob uses zstd.", "s1")
	b := e.honcho.Add(bob, bob, "  bob USES   zstd. ", "")
	e.honcho.Add(bob, bob, "Bob uses zstd!", "")
	e.honcho.Add("hermes", bob, "Bob has a cat.", "")
	e.honcho.Add("carol", bob, "Bob uses zstd.", "")
	p, err := e.svc.Learned(ctx, bob, homeID, hub.LearnedQuery{All: true, Oldest: true})
	if err != nil || p.Total != 3 || p.Copies != 4 || len(p.Items) != 3 {
		t.Fatalf("all = %+v, %v", p, err)
	}
	pair := p.Items[0]
	if pair.ID != a || !slices.Equal(pair.IDs, []string{a, b}) || pair.View != hub.ViewBoth || pair.SessionID != "s1" || pair.Content != "Bob uses zstd." {
		t.Fatalf("pair = %+v", pair)
	}
	if p.Items[1].View != hub.ViewSelf || len(p.Items[1].IDs) != 1 {
		t.Fatalf("punctuation must not pair: %+v", p.Items[1])
	}
	for _, v := range []string{hub.ViewHermes, hub.ViewSelf} {
		p, err = e.svc.Learned(ctx, bob, homeID, hub.LearnedQuery{View: v, All: true, Oldest: true})
		if err != nil || p.Total != 2 || p.Items[0].ID != a || len(p.Items[0].IDs) != 2 {
			t.Fatalf("%s = %+v, %v", v, p, err)
		}
	}
}

func TestLearnedPairsSearchDelete(t *testing.T) {
	t.Parallel()
	e := setup(t)
	ctx := context.Background()
	e.honcho.Add("hermes", bob, "Bob uses zstd.", "s1")
	e.honcho.Add(bob, bob, "  bob USES   zstd. ", "")
	e.honcho.Add(bob, bob, "Bob uses zstd!", "")
	e.honcho.Add("hermes", bob, "Bob has a cat.", "")
	p, err := e.svc.Learned(ctx, bob, homeID, hub.LearnedQuery{Query: "zstd.", All: true})
	if err != nil || p.Total != 1 || p.Copies != 2 || p.Items[0].View != hub.ViewBoth {
		t.Fatalf("search = %+v, %v", p, err)
	}
	p, err = e.svc.Learned(ctx, bob, homeID, hub.LearnedQuery{Query: "zstd.", View: hub.ViewSelf, All: true})
	if err != nil || p.Total != 1 || len(p.Items[0].IDs) != 2 {
		t.Fatalf("self search = %+v, %v", p, err)
	}
	info, err := e.svc.LearnedInfo(ctx, bob, homeID)
	if err != nil || info.Total != 3 || info.Copies != 4 || info.Both != 1 || info.Hermes != 2 || info.Self != 2 {
		t.Fatalf("info = %+v, %v", info, err)
	}
	n, err := e.svc.DeleteLearned(ctx, bob, homeID, p.Items[0].IDs)
	if err != nil || n != 2 {
		t.Fatalf("delete pair = %d, %v", n, err)
	}
	if p, err = e.svc.Learned(ctx, bob, homeID, hub.LearnedQuery{All: true}); err != nil || p.Total != 2 || p.Copies != 2 {
		t.Fatalf("after delete = %+v, %v", p, err)
	}
}

func TestLearnedLevels(t *testing.T) {
	t.Parallel()
	e := setup(t)
	ctx := context.Background()
	raw := e.honcho.Add("hermes", bob, "Bob said he moved to Austin.", "s1")
	e.honcho.AddLevel("hermes", bob, "Bob lives in Austin.", "", hub.LevelDeductive)
	e.honcho.AddLevel(bob, bob, "Bob lives in Austin.", "", hub.LevelExplicit)
	e.honcho.AddLevel("hermes", bob, "Bob tends to work late.", "", hub.LevelInductive)
	p, err := e.svc.Learned(ctx, bob, homeID, hub.LearnedQuery{Oldest: true})
	if err != nil || p.Total != 2 || p.Hidden != 1 || p.Copies != 3 {
		t.Fatalf("condensed = %+v, %v", p, err)
	}
	if p.Items[0].Level != hub.LevelDeductive || p.Items[0].View != hub.ViewBoth || p.Items[1].Level != hub.LevelInductive {
		t.Fatalf("levels = %+v", p.Items)
	}
	p, err = e.svc.Learned(ctx, bob, homeID, hub.LearnedQuery{All: true, Oldest: true})
	if err != nil || p.Total != 3 || p.Hidden != 0 || p.Items[0].ID != raw || p.Items[0].Level != hub.LevelExplicit {
		t.Fatalf("all = %+v, %v", p, err)
	}
	p, err = e.svc.Learned(ctx, bob, homeID, hub.LearnedQuery{Query: "bob"})
	if err != nil || p.Total != 2 {
		t.Fatalf("condensed search = %+v, %v", p, err)
	}
	if p, err = e.svc.Learned(ctx, bob, homeID, hub.LearnedQuery{Query: "moved"}); err != nil || p.Total != 0 {
		t.Fatalf("search for a raw fact = %+v, %v", p, err)
	}
	p, err = e.svc.Learned(ctx, bob, homeID, hub.LearnedQuery{Query: "moved", All: true})
	if err != nil || p.Total != 1 {
		t.Fatalf("search all = %+v, %v", p, err)
	}
	info, err := e.svc.LearnedInfo(ctx, bob, homeID)
	if err != nil || info.Condensed != 2 || info.Raw != 1 || info.Total != 3 || info.Copies != 4 {
		t.Fatalf("info = %+v, %v", info, err)
	}
}

func TestLearnedPages(t *testing.T) {
	t.Parallel()
	e := setup(t)
	ctx := context.Background()
	for i := range 130 {
		text := fmt.Sprintf("fact %d", i)
		e.honcho.Add("hermes", bob, text, "")
		e.honcho.Add(bob, bob, text, "")
	}
	p, err := e.svc.Learned(ctx, bob, homeID, hub.LearnedQuery{All: true})
	if err != nil || p.Total != 130 || p.Copies != 260 || p.Pages != 2 || len(p.Items) != 100 || p.Items[0].Content != "fact 129" {
		t.Fatalf("page 1 = %d items, %+v, %v", len(p.Items), p.Total, err)
	}
	p, err = e.svc.Learned(ctx, bob, homeID, hub.LearnedQuery{All: true, Page: 2})
	if err != nil || len(p.Items) != 30 || p.Items[29].Content != "fact 0" || len(p.Items[29].IDs) != 2 {
		t.Fatalf("page 2 = %+v, %v", p, err)
	}
}

func TestAddLearned(t *testing.T) {
	t.Parallel()
	e := setup(t)
	ctx := context.Background()
	c, err := e.svc.AddLearned(ctx, bob, homeID, "  Has a garage.  ")
	if err != nil || c.Content != "Has a garage." || c.View != hub.ViewHermes {
		t.Fatalf("add = %+v, %v", c, err)
	}
	if _, err := e.svc.AddLearned(ctx, bob, homeID, strings.Repeat("x", 4001)); status(err) != http.StatusBadRequest {
		t.Fatalf("long fact: %v", err)
	}
}

func TestLearnedCard(t *testing.T) {
	t.Parallel()
	e := setup(t)
	ctx := context.Background()
	e.honcho.Cards[bob] = []string{"Own card"}
	info, err := e.svc.LearnedInfo(ctx, bob, homeID)
	if err != nil || len(info.Card) != 1 || info.Card[0] != "Own card" {
		t.Fatalf("own card = %+v, %v", info, err)
	}
	e.honcho.MissingPeers = true
	if info, err = e.svc.LearnedInfo(ctx, bob, homeID); err != nil || len(info.Card) != 0 {
		t.Fatalf("card of unknown peers = %+v, %v", info, err)
	}
}

func TestDeleteLearned(t *testing.T) {
	t.Parallel()
	e := setup(t)
	ctx := context.Background()
	a := e.honcho.Add("hermes", bob, "one", "")
	b := e.honcho.Add("hermes", bob, "two", "")
	other := e.honcho.Add("hermes", "hermes", "about hermes", "")
	n, err := e.svc.DeleteLearned(ctx, bob, homeID, []string{a, other, b})
	if n != 1 || status(err) != http.StatusNotFound {
		t.Fatalf("delete = %d, %v", n, err)
	}
	if len(e.honcho.Items) != 2 {
		t.Fatalf("items left = %+v", e.honcho.Items)
	}
	for _, ids := range [][]string{nil, {"x/../y"}, {b, b}, slices.Repeat([]string{"c"}, hub.MaxLearnedDelete+1)} {
		if _, err := e.svc.DeleteLearned(ctx, bob, homeID, ids); status(err) != http.StatusBadRequest {
			t.Fatalf("delete %v: %v", ids, err)
		}
	}
	if n, err := e.svc.DeleteLearned(ctx, bob, homeID, []string{b}); n != 1 || err != nil {
		t.Fatalf("delete b = %d, %v", n, err)
	}
	if _, err := e.svc.DeleteLearned(ctx, alice, homeID, []string{other}); !errors.Is(err, hub.ErrForbidden) {
		t.Fatalf("alice delete: %v", err)
	}
}

func TestExportLearned(t *testing.T) {
	t.Parallel()
	e := setup(t)
	for range 205 {
		e.honcho.Add("hermes", bob, "fact", "")
	}
	e.honcho.Add("hermes", "hermes", "not bob", "")
	e.honcho.Add("carol", bob, "not Hermes's or Bob's", "")
	var buf bytes.Buffer
	if err := e.svc.ExportLearned(context.Background(), bob, homeID, &buf); err != nil {
		t.Fatal(err)
	}
	var out []hub.Conclusion
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil || len(out) != 205 {
		t.Fatalf("export = %d, %v", len(out), err)
	}
	if strings.Contains(buf.String(), key) {
		t.Fatal("export holds the key")
	}
}

func TestHonchoErrorsHideKey(t *testing.T) {
	t.Parallel()
	e := setup(t)
	e.honcho.HangUp()
	_, err := e.svc.Learned(context.Background(), bob, homeID, hub.LearnedQuery{})
	if status(err) != http.StatusBadGateway || strings.Contains(err.Error(), key) || strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("err = %v", err)
	}
}

func TestNewNeedsKey(t *testing.T) {
	t.Parallel()
	_, err := memory.New(memory.Options{Access: access{}, Backends: []config.Backend{{ID: homeID,
		Honcho: config.Honcho{URL: "http://honcho.example.com", Workspace: "w", UserPeer: bob, AIPeer: "hermes", APIKeyEnv: "UNSET_KEY"}}}})
	if err == nil {
		t.Fatal("New without a key")
	}
}

func TestBacklog(t *testing.T) {
	t.Parallel()
	e := setup(t)
	ctx := context.Background()
	e.honcho.SetQueue(10, 6, 1, 3)
	got, err := e.svc.Backlog(ctx, bob, homeID)
	if err != nil || got != (hub.Backlog{Total: 10, Completed: 6, InProgress: 1, Pending: 3}) {
		t.Fatalf("backlog = %+v, %v", got, err)
	}
	if _, err := e.svc.Backlog(ctx, alice, pcID); !errors.Is(err, hub.ErrNoMemory) {
		t.Fatalf("backlog without Honcho: %v", err)
	}
	if _, err := e.svc.Backlog(ctx, alice, homeID); !errors.Is(err, hub.ErrForbidden) {
		t.Fatalf("backlog of another user's backend: %v", err)
	}
}
