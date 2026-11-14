package web_test

import (
	"log/slog"
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
	"github.com/USA-RedDragon/talaria/hub/internal/models"
	"github.com/USA-RedDragon/talaria/hub/internal/models/modelstest"
	"github.com/USA-RedDragon/talaria/hub/internal/store"
	"github.com/USA-RedDragon/talaria/hub/internal/web"
)

// Settings > Agent > Long chats is a slider from 30% to 90% in 5% steps, not
// four pills: it saves once on release, moves 5% per arrow key, shows the
// model's window floor, and is disabled when the install sets the point.
// testdata/slider/run.js drives the real page in headless Chrome at 412 and
// 1280 px. SLIDER_SHOT=dir also writes slider-build-*.png. It skips where
// node or Chrome is missing.
const (
	modeOpen   = "open"
	modeLocked = "locked"
)

func TestSummarizeSlider(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{modeOpen, modeLocked} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			node, err := exec.LookPath("node")
			if err != nil {
				t.Skip("node is not installed")
			}
			chrome := chromePath(t)
			var locked []string
			if mode == modeLocked {
				locked = []string{agentcfg.KeyThreshold}
			}
			pc, home := agentcfgtest.New(), agentcfgtest.New()
			// A saved value between two steps: shown at the nearest step, never rewritten.
			pc.Edit(func(a *agentcfgtest.Agent) {
				a.Config["compression"] = map[string]any{keyEnabled: true, "threshold": 0.33}
			})
			mp, mh := modelstest.New(), modelstest.New()
			// A ready default model with a 32K window, so the hub knows what it will use.
			mp.Keys["OPENROUTER_API_KEY"], mp.Model, mp.Provider = "k", "openai/gpt-5", "openrouter"
			var e *hubEnv
			var mu sync.RWMutex
			mu.Lock()
			e = setupWith(t, func(cfg *config.Config, opts *web.Options, _ *store.Store) {
				cfg.Agent.LockedKeys = locked
				opts.Agent = agentcfg.New(agentcfg.Options{Access: lockedAccess{&mu, lateAccess{&e}}, Locked: locked,
					Requesters: map[string]device.Requester{"pc": pc, backendHome: home}})
				opts.Models = models.New(models.Options{Access: lockedAccess{&mu, lateAccess{&e}}, Backends: cfg.Backends,
					Requesters: map[string]device.Requester{"pc": mp, backendHome: mh}})
				opts.Logger = slog.New(slog.NewTextHandler(&syncBuf{}, nil))
			})
			mu.Unlock()
			prof, err := os.MkdirTemp("", "slider-profile-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(prof) })
			out, err := exec.CommandContext(t.Context(), node, filepath.Join("testdata", "slider", "run.js"), chrome, e.url, prof, mode).CombinedOutput()
			if err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			var puts int
			pc.Edit(func(a *agentcfgtest.Agent) {
				for _, b := range a.Bodies {
					if strings.Contains(b, "threshold") {
						puts++
					}
				}
			})
			switch mode {
			case modeLocked:
				if puts != 0 {
					t.Fatalf("locked slider sent %d saves", puts)
				}
			default:
				if puts != 36 { // 4 page loads x 9 saves
					t.Fatalf("threshold saves = %d, want 36", puts)
				}
			}
		})
	}
}
