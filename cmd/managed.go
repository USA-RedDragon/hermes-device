package cmd

import (
	"context"
	"errors"
	"log/slog"
	"runtime"
	"sync"
	"time"

	"github.com/USA-RedDragon/hermes-device/internal/config"
	"github.com/USA-RedDragon/hermes-device/internal/daemonstate"
	"github.com/USA-RedDragon/hermes-device/internal/hermesbuild"
	"github.com/USA-RedDragon/hermes-device/internal/oci"
	"github.com/USA-RedDragon/hermes-device/protocol"
)

// Retry delays of a failed background fetch of the server's Hermes build.
const (
	fetchRetryMin = time.Minute
	fetchRetryMax = time.Hour
	// keepPrevious is how many builds besides the current one a prune
	// keeps, so a chat started just before an upgrade can start again.
	keepPrevious = 1
)

// hermesBuilds is the store of managed Hermes builds, or nil when
// chat.managed is off or this OS cannot run them.
func hermesBuilds(cfg *config.Config, userAgent string) *hermesbuild.Store {
	if !cfg.Chat.Managed || runtime.GOOS != "linux" {
		return nil
	}
	dir := cfg.Chat.ManagedDir
	if dir == "" {
		var err error
		if dir, err = hermesbuild.DefaultDir(); err != nil {
			slog.Warn("No directory for managed Hermes builds; chat uses chat.hermes", "error", err)
			return nil
		}
	}
	return &hermesbuild.Store{Dir: dir, UserAgent: userAgent}
}

// serverHermes records the server's Hermes from each welcome in the
// daemon state file and hands its image to fetchLoop.
type serverHermes struct {
	mu    sync.Mutex
	dir   string
	state daemonstate.State
	ready bool
	// fetch holds the latest image to fetch.
	fetch chan string
}

func newServerHermes() *serverHermes { return &serverHermes{fetch: make(chan string, 1)} }

// start writes st into dir and from then on keeps the server's Hermes in
// it.
func (h *serverHermes) start(dir string, st daemonstate.State) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	st.HermesVersion, st.HermesImage = h.state.HermesVersion, h.state.HermesImage
	h.dir, h.state, h.ready = dir, st, true
	return daemonstate.Write(dir, st)
}

func (h *serverHermes) welcome(w protocol.Welcome) {
	image := w.HermesImage
	if len(image) > protocol.MaxHermesImage {
		slog.Warn("The plugin reported a Hermes image reference that is too long; ignoring it", "length", len(image))
		image = ""
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	changed := h.state.HermesVersion != w.HermesVersion || h.state.HermesImage != image
	h.state.HermesVersion, h.state.HermesImage = w.HermesVersion, image
	if h.ready && changed {
		if err := daemonstate.Write(h.dir, h.state); err != nil {
			slog.Warn("Could not record the server's Hermes in the daemon state", "error", err)
		}
	}
	select {
	case <-h.fetch:
	default:
	}
	h.fetch <- image
}

// image is the server's image from the latest welcome.
func (h *serverHermes) image() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.state.HermesImage
}

// fetchLoop fetches the build of each image the welcomes name, retrying
// with backoff until it is on disk, and prunes older builds. It runs
// until ctx ends.
func (h *serverHermes) fetchLoop(ctx context.Context, builds *hermesbuild.Store) {
	var image string
	for {
		select {
		case <-ctx.Done():
			return
		case image = <-h.fetch:
		}
		delay := fetchRetryMin
		for image != "" && h.fetchOnce(ctx, builds, image) {
			slog.Warn("Trying the Hermes build again later", "image", image, "in", delay)
			t := time.NewTimer(delay)
		wait:
			for {
				select {
				case <-ctx.Done():
					t.Stop()
					return
				case next := <-h.fetch:
					if next == image {
						// A reconnect repeats the image; the backoff stays.
						continue
					}
					t.Stop()
					image, delay = next, fetchRetryMin
					break wait
				case <-t.C:
					delay = min(delay*2, fetchRetryMax)
					break wait
				}
			}
		}
	}
}

// fetchOnce fetches image's build and prunes the builds that neither the
// latest welcome nor recent use needs. It reports whether a later try may
// work.
func (h *serverHermes) fetchOnce(ctx context.Context, builds *hermesbuild.Store, image string) (retry bool) {
	if _, err := oci.ParseReference(image); err != nil {
		slog.Warn("The server's Hermes image is not pinned by digest; chat uses chat.hermes", "image", image, "error", err)
		return false
	}
	_, err := builds.Ensure(ctx, image)
	switch {
	case err == nil:
		current := h.image()
		if _, perr := oci.ParseReference(current); perr != nil {
			current = image
		}
		if err := builds.Prune(current, keepPrevious); err != nil {
			slog.Warn("Could not remove old Hermes builds", "error", err)
		}
		return false
	case ctx.Err() != nil:
		return false
	case permanent(err):
		slog.Warn("Cannot run the server's Hermes build here; chat uses chat.hermes", "image", image, "error", err)
		return false
	}
	slog.Warn("Could not fetch the server's Hermes build", "image", image, "error", err)
	return true
}

// permanent reports whether a fetch failed for a reason that a retry of
// the same image cannot fix.
func permanent(err error) bool {
	return errors.Is(err, oci.ErrPlatform) || errors.Is(err, oci.ErrMediaType) || errors.Is(err, oci.ErrBadReference)
}
