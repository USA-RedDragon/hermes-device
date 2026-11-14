package web_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/USA-RedDragon/talaria/hub/internal/config"
	"github.com/USA-RedDragon/talaria/hub/internal/gateway/gatewaytest"
	"github.com/USA-RedDragon/talaria/hub/internal/hub"
	"github.com/USA-RedDragon/talaria/hub/internal/store"
	"github.com/USA-RedDragon/talaria/hub/internal/web"
)

const (
	modeHub     = "hub"
	modeAway    = "away"
	modeHidden  = "hidden"
	modeRelease = "release"
	longOne     = "long1"
	longTwo     = "long2"
	longThree   = "long3"
	longFour    = "long4"
)

// After Hermes or the hub restarts, the page reconnects, and the chat it
// shows, and each chat opened next, stays at its newest message: a
// reconnect's snapshot, the hub's empty copy and the rows a turn replays
// must not leave the view on older messages. testdata/restartjump/run.js
// drives the real page in headless Chrome and Firefox. Each skips where
// node or its browser is missing.
func TestRestartKeepsNewestChrome(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"gw", modeHub, modeRelease, modeAway} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			restartJump(t, chromePath(t), mode, false)
		})
	}
}

// A tab that is hidden when the new release arrives reloads then; the
// reader comes back to the newest message. Chrome only: Firefox headless
// has no second tab to hide the first.
func TestRestartKeepsNewestHiddenChrome(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{modeHidden, "bggw", "bghub"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			restartJump(t, chromePath(t), mode, false)
		})
	}
}

func TestRestartKeepsNewestFirefox(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"gw", modeHub, modeRelease, modeAway} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			restartJump(t, firefoxPath(t), mode, false)
		})
	}
}
