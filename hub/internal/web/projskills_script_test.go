package web_test

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/USA-RedDragon/talaria/hub/internal/agentcfg"
	"github.com/USA-RedDragon/talaria/hub/internal/agentcfg/agentcfgtest"
	"github.com/USA-RedDragon/talaria/hub/internal/config"
	"github.com/USA-RedDragon/talaria/hub/internal/device"
	"github.com/USA-RedDragon/talaria/hub/internal/hub"
	"github.com/USA-RedDragon/talaria/hub/internal/memory"
	"github.com/USA-RedDragon/talaria/hub/internal/memory/memorytest"
	"github.com/USA-RedDragon/talaria/hub/internal/store"
	"github.com/USA-RedDragon/talaria/hub/internal/web"
)

// Project skills show in two places: a Skills tab on each project in Memory
// (name, project pill, description, View and Delete on the name's line), and
// Settings > Skills and tools, grouped by where each skill lives. The page
// reads the lists from the project-memory plugin's routes through the hub.
// testdata/projskills/run.js drives the real page in headless Chrome at 412
// and 1280 px, light and dark. PROJSKILLS_SHOT=dir also writes
// project-skills-build-*.png. It skips where node or Chrome is missing.
func TestProjectSkillsPages(t *testing.T) {
	t.Parallel()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	chrome := chromePath(t)
	pcA, homeA := agentcfgtest.New(), agentcfgtest.New()
	notes := hub.Notes{Targets: map[string]hub.NoteStore{
		hub.TargetMemory: {File: "/data/memories/MEMORY.md", Limit: 2200, Enabled: true, Entries: []string{"Use zstd."}},
		hub.TargetUser:   {File: "/data/memories/USER.md", Limit: 1375, Enabled: true},
	}, Pending: []hub.PendingNote{}}
	pcM, homeM := memorytest.NewPlugin(notes), memorytest.NewPlugin(notes)
	t.Cleanup(pcM.Close)
	t.Cleanup(homeM.Close)
	const demo, other = "github.com-alice-demo-app", "github.com-alice-other-tool"
	pcM.AddProject(hub.Project{ID: demo, Identity: "github.com/alice/demo-app", Kind: hub.ProjectRemote, Name: "demo-app", Count: 3, Skills: 3}, "Run make check.", "Use Go 1.26.", "Tabs, not spaces.")
	pcM.AddProject(hub.Project{ID: other, Identity: "github.com/alice/other-tool", Kind: hub.ProjectRemote, Name: "other-tool", Skills: 1})
	skill := func(project, name, desc, text string, on bool) {
		pcM.AddProjectSkill(project, hub.ProjectSkillDoc{ProjectSkill: hub.ProjectSkill{Name: name, Path: name, Description: desc, Enabled: on}, Content: text, Files: []string{"notes.txt"}})
	}
	skill(demo, "demo-app-layout", "Use when changing the build or the module layout of demo-app.", "---\nname: demo-app-layout\n---\n\n# demo-app-layout\n\nStep 1: run make.", true)
	skill(demo, "demo-app-release", "Use when cutting a demo-app release or fixing its CI.", "# release", true)
	skill(demo, "demo-app-people-and-owners-of-every-part-of-the-code-base", "Use when a change needs a reviewer or an owner for a part of demo-app.", "# people", false)
	skill(other, "other-tool-ci", "Use when the CI of other-tool fails.", "# ci", true)

	var e *hubEnv
	var mu sync.RWMutex
	mu.Lock()
	e = setupWith(t, func(cfg *config.Config, opts *web.Options, _ *store.Store) {
		opts.Agent = agentcfg.New(agentcfg.Options{Access: lockedAccess{&mu, lateAccess{&e}},
			Requesters: map[string]device.Requester{"pc": pcA, backendHome: homeA}})
		if err := cfg.Validate(); err != nil {
			t.Fatal(err)
		}
		m, err := memory.New(memory.Options{Access: lateAccess{&e}, Backends: cfg.Backends,
			Requesters: map[string]device.Requester{"pc": pcM, backendHome: homeM}})
		if err != nil {
			t.Fatal(err)
		}
		opts.Memory = m
	})
	mu.Unlock()
	prof, err := os.MkdirTemp("", "projskills-profile-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(prof) })
	out, err := exec.CommandContext(t.Context(), node, filepath.Join("testdata", "projskills", "run.js"), chrome, e.url, prof, demo).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	// The page's Delete reached the plugin once, for the skill the user picked.
	var dels []string
	for _, r := range pcM.Requests() {
		if r.Method == http.MethodDelete {
			dels = append(dels, r.Path)
		}
	}
	if len(dels) != 1 || !strings.HasSuffix(dels[0], "/projects/"+demo+"/skills/demo-app-release") {
		t.Fatalf("plugin deletes = %v", dels)
	}
	// Turning a project skill off goes through the agent's one list of switched-off skills.
	toggles, named := 0, 0
	for _, p := range pcA.Paths {
		if p == "PUT /api/skills/toggle" {
			toggles++
		}
	}
	for _, b := range pcA.Bodies {
		if strings.Contains(b, `"name":"other-tool-ci"`) && strings.Contains(b, `"enabled":false`) {
			named++
		}
	}
	if toggles != 1 || named != 1 {
		t.Fatalf("agent toggles = %d, for other-tool-ci = %d (paths %v)", toggles, named, pcA.Paths)
	}
}
