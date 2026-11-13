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

	"github.com/USA-RedDragon/talaria/internal/config"
	"github.com/USA-RedDragon/talaria/internal/daemonstate"
	"github.com/USA-RedDragon/talaria/internal/hermesbuild"
	"github.com/USA-RedDragon/talaria/internal/oci"
	"github.com/USA-RedDragon/talaria/protocol"
)

// Retry delays of a failed background fetch of the server's Hermes build.
const (
	fetchRetryInitial = time.Minute
	fetchRetryMax     = time.Hour
	// keepPrevious is how many builds besides the current one a prune
	// keeps, so a chat started just before an upgrade can start again.
	keepPrevious = 1
)

// hermesBuilds is the store of managed Hermes builds, or nil when
// chat.managed is off or this OS cannot run them.
func hermesBuilds(cfg *config.Config, userAgent string) *hermesbuild.Store {
	if !cfg.Chat.Managed || runtime.GOOS != goosLinux {
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
	// fetch holds the latest images to fetch.
	fetch chan images
}

// images are the server's image and the upstream image it builds on.
type images struct{ image, base string }

func newServerHermes() *serverHermes { return &serverHermes{fetch: make(chan images, 1)} }

// start writes st into dir and from then on keeps the server's Hermes in
// it.
func (h *serverHermes) start(dir string, st daemonstate.State) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	st.HermesVersion, st.HermesImage, st.HermesBaseImage = h.state.HermesVersion, h.state.HermesImage, h.state.HermesBaseImage
	h.dir, h.state, h.ready = dir, st, true
	return daemonstate.Write(dir, st)
}

func (h *serverHermes) welcome(w protocol.Welcome) {
	image, base := w.HermesImage, w.HermesBaseImage
	if len(image) > protocol.MaxHermesImage || len(base) > protocol.MaxHermesImage {
		slog.Warn("The plugin reported a Hermes image reference that is too long; ignoring it", "length", max(len(image), len(base)))
		image, base = "", ""
	}
	if image == "" {
		base = ""
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	changed := h.state.HermesVersion != w.HermesVersion || h.state.HermesImage != image || h.state.HermesBaseImage != base
	h.state.HermesVersion, h.state.HermesImage, h.state.HermesBaseImage = w.HermesVersion, image, base
	if h.ready && changed {
		if err := daemonstate.Write(h.dir, h.state); err != nil {
			slog.Warn("Could not record the server's Hermes in the daemon state", "error", err)
		}
	}
	select {
	case <-h.fetch:
	default:
	}
	h.fetch <- images{image: image, base: base}
}

// current is the server's images from the latest welcome.
func (h *serverHermes) current() images {
	h.mu.Lock()
	defer h.mu.Unlock()
	return images{image: h.state.HermesImage, base: h.state.HermesBaseImage}
}

// fetchLoop fetches the build of each image the welcomes name, retrying
// with backoff until it is on disk, and prunes older builds. It runs
// until ctx ends.
func (h *serverHermes) fetchLoop(ctx context.Context, builds *hermesbuild.Store) {
	var want images
	for {
		select {
		case <-ctx.Done():
			return
		case want = <-h.fetch:
		}
		delay := fetchRetryInitial
		for want.image != "" && h.fetchOnce(ctx, builds, want) {
			slog.Warn("Trying the Hermes build again later", "image", want.image, "in", delay)
			t := time.NewTimer(delay)
		wait:
			for {
				select {
				case <-ctx.Done():
					t.Stop()
					return
				case next := <-h.fetch:
					if next == want {
						// A reconnect repeats the images; the backoff stays.
						continue
					}
					t.Stop()
					want, delay = next, fetchRetryInitial
					break wait
				case <-t.C:
					delay = min(delay*2, fetchRetryMax)
					break wait
				}
			}
		}
	}
}

// fetchOnce fetches the build of want's image, or when that fails, of its
// base image, and prunes the builds that neither the latest welcome nor
// recent use needs. It reports whether a later try may work.
func (h *serverHermes) fetchOnce(ctx context.Context, builds *hermesbuild.Store, want images) (retry bool) {
	image := want.image
	if _, err := oci.ParseReference(image); err != nil {
		slog.Warn("The server's Hermes image is not pinned by digest; chat uses chat.hermes", "image", image, "error", err)
		return false
	}
	_, err := builds.Ensure(ctx, image)
	if err != nil && ctx.Err() == nil && want.base != "" {
		if _, perr := oci.ParseReference(want.base); perr == nil {
			slog.Info("Could not fetch the server's Hermes build; fetching the upstream build it is based on",
				"image", image, "base", want.base, "error", err)
			image = want.base
			_, err = builds.Ensure(ctx, image)
		}
	}
	switch {
	case err == nil:
		keep := image
		if cur := h.current(); cur.image != want.image {
			keep = cur.image
		}
		if _, perr := oci.ParseReference(keep); perr != nil {
			keep = image
		}
		if err := builds.Prune(keep, keepPrevious); err != nil {
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
var errNoImage = errors.New("the server reports no agent image; chat uses chat.hermes")

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
		_, _ = fmt.Fprintf(w, "\rFetching the server's agent build: %3d%% of %.1f GB%s", done*100/total, float64(total)/1e9, end)
	}
}

// printStatus shows the server's agent release and the managed builds.
func printStatus(w io.Writer, cfg *config.Config, builds *hermesbuild.Store, st *daemonstate.State) error {
	image := st.HermesImage
	if image == "" {
		image = "not reported; chat runs chat.hermes"
	}
	version := st.HermesVersion
	if version == "" {
		version = "unknown until run connects"
	}
	_, _ = fmt.Fprintf(w, "Server agent:  %s\nServer image:  %s\n", version, image)
	if st.HermesBaseImage != "" {
		_, _ = fmt.Fprintf(w, "Base image:    %s\n", st.HermesBaseImage)
	}
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
	current, baseKey := "", ""
	if st.HermesImage != "" {
		current, _ = hermesbuild.Key(st.HermesImage)
	}
	if st.HermesBaseImage != "" {
		baseKey, _ = hermesbuild.Key(st.HermesBaseImage)
	}
	const stamp = "2006-01-02 15:04 MST"
	for _, b := range list {
		var notes []string
		switch strings.TrimPrefix(b.Digest, "sha256:") {
		case current:
			notes = append(notes, "current")
		case baseKey:
			notes = append(notes, "upstream base of the current image")
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
		_, err := builds.Open(st.HermesImage)
		_, berr := builds.Open(st.HermesBaseImage)
		if errors.Is(err, hermesbuild.ErrNotPulled) && (baseKey == "" || berr != nil) {
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
	image := st.HermesImage
	b, err := builds.Refresh(ctx, image)
	if err != nil && !errors.Is(err, hermesbuild.ErrInUse) && ctx.Err() == nil && st.HermesBaseImage != "" {
		_, _ = fmt.Fprintf(w, "Could not fetch %s (%v); fetching the upstream build it is based on.\n", image, err)
		image = st.HermesBaseImage
		b, err = builds.Refresh(ctx, image)
	}
	if errors.Is(err, hermesbuild.ErrInUse) {
		return fmt.Errorf("%w; end that chat first", err)
	}
	if err != nil {
		return err
	}
	if err := builds.Prune(image, keepPrevious); err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "Agent build ready: %s\n  in %s\n", b.Meta.Reference, b.Dir)
	return err
}
