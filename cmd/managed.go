package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"runtime"
	"strings"
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

// errNoImage is chat --refresh without an image from the server.
var errNoImage = errors.New("the server reports no Hermes image; chat uses chat.hermes")

// progress prints the progress of a pull to w.
func progress(w io.Writer) func(done, total int64) {
	return func(done, total int64) {
		if total <= 0 {
			return
		}
		end := ""
		if done >= total {
			end = "\n"
		}
		_, _ = fmt.Fprintf(w, "\rFetching the server's Hermes build: %3d%% of %.1f GB%s", done*100/total, float64(total)/1e9, end)
	}
}

// printStatus shows the server's Hermes and the managed builds.
func printStatus(w io.Writer, cfg *config.Config, builds *hermesbuild.Store, st *daemonstate.State) error {
	image := st.HermesImage
	if image == "" {
		image = "not reported; chat runs chat.hermes"
	}
	version := st.HermesVersion
	if version == "" {
		version = "unknown until run connects"
	}
	_, _ = fmt.Fprintf(w, "Server Hermes: %s\nServer image:  %s\n", version, image)
	switch {
	case !cfg.Chat.Managed:
		_, _ = fmt.Fprintf(w, "Managed builds: off (chat.managed is false); chat runs %s\n", cfg.Chat.Hermes)
		return nil
	case builds == nil:
		_, _ = fmt.Fprintf(w, "Managed builds: not on %s; chat runs %s\n", runtime.GOOS, cfg.Chat.Hermes)
		return nil
	}
	list, err := builds.List()
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(w, "Managed builds in %s:\n", builds.Dir)
	if len(list) == 0 {
		_, _ = fmt.Fprintln(w, "  none")
	}
	current := ""
	if st.HermesImage != "" {
		current, _ = hermesbuild.Key(st.HermesImage)
	}
	const stamp = "2006-01-02 15:04 MST"
	for _, b := range list {
		var notes []string
		if strings.TrimPrefix(b.Digest, "sha256:") == current {
			notes = append(notes, "current")
		}
		if builds.InUse(b) {
			notes = append(notes, "in use")
		}
		_, _ = fmt.Fprintf(w, "  %s\n    from %s, pulled %s, last used %s", b.Meta.Reference, b.Meta.Source,
			b.Meta.PulledAt.Local().Format(stamp), b.LastUsed.Local().Format(stamp))
		if len(notes) > 0 {
			_, _ = fmt.Fprintf(w, " (%s)", strings.Join(notes, ", "))
		}
		_, _ = fmt.Fprintln(w)
	}
	if current != "" {
		if _, err := builds.Open(st.HermesImage); errors.Is(err, hermesbuild.ErrNotPulled) {
			_, _ = fmt.Fprintln(w, "The current build is not on this device yet: run fetches it, or chat does when it starts.")
		}
	}
	return nil
}

// refreshBuild fetches the server's build again and prunes old ones.
func refreshBuild(ctx context.Context, w io.Writer, builds *hermesbuild.Store, st *daemonstate.State) error {
	if builds == nil {
		return fmt.Errorf("chat --refresh: %w, or chat.managed is false", hermesbuild.ErrUnsupported)
	}
	if st.HermesImage == "" {
		return errNoImage
	}
	b, err := builds.Refresh(ctx, st.HermesImage)
	if errors.Is(err, hermesbuild.ErrInUse) {
		return fmt.Errorf("%w; end that chat first", err)
	}
	if err != nil {
		return err
	}
	if err := builds.Prune(st.HermesImage, keepPrevious); err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "Hermes build ready: %s\n  in %s\n", b.Meta.Reference, b.Dir)
	return err
}
