// Package hermesbuild keeps verified copies of the Hermes build that the
// server runs, taken from its container image, and prepares them to run
// from the device's own directory with a Hermes home of their own.
//
// Layout under Store.Dir:
//
//	builds/<digest hex>/     a verified tree: opt/hermes and usr/local/bin of the image
//	staging/<digest hex>-*/  a pull in progress
//	locks/<digest hex>.lock  flock: exclusive while pulling or pruning, shared while a chat runs
//	home/                    HOME and HERMES_HOME of the managed Hermes
package hermesbuild

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/USA-RedDragon/talaria/internal/oci"
)

// DirName is the directory of this tool in the user cache directory.
const DirName = "talaria"

// Directories and files under Store.Dir and in a build.
const (
	buildsDir  = "builds"
	stagingDir = "staging"
	locksDir   = "locks"
	homeDir    = "home"
	metaFile   = ".build.json"
	trashTag   = "trash-"
	// Prefix is where Hermes lives in the image.
	Prefix = "opt/hermes"
)

// Filter is what a build keeps of the image: Hermes with its bundled
// Python and Node, and the /usr/local/bin links into them. The browser and
// ffmpeg that the gateway uses for tools are left out.
var Filter = oci.Filter{ //nolint:gochecknoglobals // fixed table
	Include: []string{Prefix, "usr/local/bin"},
	Exclude: []string{Prefix + "/tools/chromium-*", Prefix + "/tools/ffmpeg-*", Prefix + "/apps"},
}

// lockPoll is how often a waiting lock tries again.
const lockPoll = 250 * time.Millisecond

// Errors.
var (
	ErrNotPulled   = errors.New("the Hermes build is not on this device yet")
	ErrNoLauncher  = errors.New("the Hermes build has no launcher")
	ErrUnsupported = errors.New("managed Hermes builds run on Linux only")
	ErrNoSpace     = errors.New("not enough free disk space for a Hermes build")
)

// MinFree is the free space a pull needs in the store's file system: a
// build takes about 1.4 GB, and the daemon and the auto-update still need
// room after it.
const MinFree = 4 << 30

// freeSpace returns the bytes available to this user in dir's file system.
func freeSpace(dir string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return st.Bavail * uint64(max(st.Bsize, 0)), nil
}

// DefaultDir is hermes in this tool's directory of the user cache
// directory.
func DefaultDir() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, DirName, "hermes"), nil
}

// Store holds the builds.
type Store struct {
	Dir       string
	HTTP      *http.Client
	UserAgent string
	// Progress, when set, reports a pull: compressed bytes done and in
	// total.
	Progress func(done, total int64)
	Log      *slog.Logger
	// Arch selects the image's platform; empty means runtime.GOARCH.
	Arch string
}

func (s *Store) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// Meta describes a build. It is the build's .build.json.
type Meta struct {
	// Reference is the image the server reported.
	Reference string `json:"reference"`
	// Source is the reference that served it: Reference, or its upstream
	// when the mirror failed.
	Source string `json:"source"`
	// Manifest is the digest of the platform's manifest.
	Manifest string `json:"manifest"`
	// Env is the image's environment.
	Env      []string  `json:"env"`
	PulledAt time.Time `json:"pulled_at"`
}

// Build is a verified build on disk.
type Build struct {
	// Dir is the build's root, which stands for / of the image.
	Dir string
	// Digest is the digest of the reported image, the build's key.
	Digest string
	Meta   Meta
	// LastUsed is when the build was pulled or last started.
	LastUsed time.Time
}

func (s *Store) buildDir(hex string) string { return filepath.Join(s.Dir, buildsDir, hex) }

// Key is the digest hex that names image's build.
func Key(image string) (string, error) {
	ref, err := oci.ParseReference(image)
	if err != nil {
		return "", err
	}
	return ref.Hex(), nil
}

// Open returns the build of image when it is on disk.
func (s *Store) Open(image string) (*Build, error) {
	hex, err := Key(image)
	if err != nil {
		return nil, err
	}
	return s.open(hex)
}

func (s *Store) open(hex string) (*Build, error) {
	dir := s.buildDir(hex)
	f := filepath.Join(dir, metaFile)
	data, err := os.ReadFile(f)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotPulled
	}
	if err != nil {
		return nil, err
	}
	b := &Build{Dir: dir, Digest: "sha256:" + hex}
	if err := json.Unmarshal(data, &b.Meta); err != nil {
		return nil, fmt.Errorf("%s: %w", f, err)
	}
	if fi, err := os.Stat(f); err == nil {
		b.LastUsed = fi.ModTime()
	}
	return b, nil
}

// List returns the builds on disk, most recently used first.
func (s *Store) List() ([]*Build, error) {
	ents, err := os.ReadDir(filepath.Join(s.Dir, buildsDir))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []*Build
	for _, e := range ents {
		if b, err := s.open(e.Name()); err == nil {
			out = append(out, b)
		}
	}
	slices.SortFunc(out, func(a, b *Build) int { return b.LastUsed.Compare(a.LastUsed) })
	return out, nil
}

// Ensure returns the build of image, pulling and verifying it first when
// it is not on disk. image must be pinned by digest. Concurrent calls for
// the same image, also from other processes, pull it once.
func (s *Store) Ensure(ctx context.Context, image string) (*Build, error) {
	ref, err := oci.ParseReference(image)
	if err != nil {
		return nil, err
	}
	hex := ref.Hex()
	if b, err := s.open(hex); !errors.Is(err, ErrNotPulled) {
		return b, err
	}
	lock, err := s.waitLock(ctx, hex, syscall.LOCK_EX)
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	if b, err := s.open(hex); !errors.Is(err, ErrNotPulled) {
		return b, err
	}
	if err := s.pull(ctx, image, ref); err != nil {
		return nil, err
	}
	return s.open(hex)
}

// Refresh pulls and verifies image's build again and then replaces the one
// on disk, which stays in place when the pull fails. It refuses with
// ErrInUse while a chat uses the build.
func (s *Store) Refresh(ctx context.Context, image string) (*Build, error) {
	ref, err := oci.ParseReference(image)
	if err != nil {
		return nil, err
	}
	hex := ref.Hex()
	unlock, err := s.tryLock(hex)
	if errors.Is(err, errBusy) {
		return nil, ErrInUse
	}
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err := s.pull(ctx, image, ref); err != nil {
		return nil, err
	}
	return s.open(hex)
}

// pull fetches image into staging, prepares it and moves it into place,
// replacing a build that is there.
func (s *Store) pull(ctx context.Context, image string, ref oci.Reference) error {
	hex := ref.Hex()
	if err := os.MkdirAll(filepath.Join(s.Dir, stagingDir), 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(s.Dir, buildsDir), 0o700); err != nil {
		return err
	}
	staging, err := os.MkdirTemp(filepath.Join(s.Dir, stagingDir), hex+"-")
	if err != nil {
		return err
	}
	done := false
	defer func() {
		if !done {
			_ = os.RemoveAll(staging)
		}
	}()
	if free, err := freeSpace(staging); err == nil && free < MinFree {
		return fmt.Errorf("%w: %d MiB free in %s, %d MiB needed", ErrNoSpace, free>>20, s.Dir, MinFree>>20)
	}
	s.log().Info("Fetching the server's Hermes build", "image", image)
	start := time.Now()
	img, err := oci.Pull(ctx, ref, staging, oci.Options{HTTP: s.HTTP, UserAgent: s.UserAgent, Arch: s.Arch,
		Filter: Filter, Progress: s.Progress, Log: s.log()})
	if err != nil {
		return fmt.Errorf("fetch Hermes %s: %w", image, err)
	}
	final := s.buildDir(hex)
	if err := relocate(staging, final); err != nil {
		return fmt.Errorf("prepare Hermes %s: %w", image, err)
	}
	meta, err := json.MarshalIndent(Meta{Reference: image, Source: img.Source.String(), Manifest: img.Manifest,
		Env: img.Config.Config.Env, PulledAt: time.Now().UTC()}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(staging, metaFile), meta, 0o600); err != nil {
		return err
	}
	// The tree must be on disk before it shows up under builds/: after a
	// power loss, a build there whose files were lost would run unchecked.
	syscall.Sync()
	if err := s.publish(staging, final, hex); err != nil {
		return err
	}
	done = true
	s.log().Info("Hermes build ready", "image", image, "source", img.Source.String(), "dir", final,
		"took", time.Since(start).Round(time.Second))
	return nil
}

// publish moves staging to final, replacing a build there. When the move
// fails, the old build is put back.
func (s *Store) publish(staging, final, hex string) error {
	if _, err := os.Lstat(final); err != nil {
		return os.Rename(staging, final)
	}
	trash := filepath.Join(s.Dir, stagingDir, trashTag+hex)
	_ = os.RemoveAll(trash)
	if err := os.Rename(final, trash); err != nil {
		return err
	}
	if err := os.Rename(staging, final); err != nil {
		if rerr := os.Rename(trash, final); rerr != nil {
			return errors.Join(err, rerr)
		}
		return err
	}
	return os.RemoveAll(trash)
}

// errBusy is a lock that another process holds.
var errBusy = errors.New("in use")

func (s *Store) openLock(hex string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Join(s.Dir, locksDir), 0o700); err != nil {
		return nil, err
	}
	return os.OpenFile(filepath.Join(s.Dir, locksDir, hex+".lock"), os.O_RDWR|os.O_CREATE, 0o600)
}

func flock(f *os.File, how int) error {
	err := syscall.Flock(int(f.Fd()), how|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return errBusy
	}
	return err
}

// waitLock takes an flock of kind how on hex's lock file, waiting until
// ctx ends. Closing the file releases it.
func (s *Store) waitLock(ctx context.Context, hex string, how int) (*os.File, error) {
	f, err := s.openLock(hex)
	if err != nil {
		return nil, err
	}
	for {
		err := flock(f, how)
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, errBusy) {
			_ = f.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, ctx.Err()
		case <-time.After(lockPoll):
		}
	}
}

// tryLock takes the exclusive flock on hex's lock file, or fails with
// errBusy at once.
func (s *Store) tryLock(hex string) (unlock func(), err error) {
	f, err := s.openLock(hex)
	if err != nil {
		return nil, err
	}
	// A child process that this program forks holds a copy of the lock
	// file's descriptor until it execs, which looks like a holder for a
	// moment; only a lock held past tryLockGrace counts as busy.
	deadline := time.Now().Add(tryLockGrace)
	for {
		err := flock(f, syscall.LOCK_EX)
		if err == nil {
			return func() { _ = f.Close() }, nil
		}
		if !errors.Is(err, errBusy) || time.Now().After(deadline) {
			_ = f.Close()
			return nil, err
		}
		time.Sleep(tryLockPoll)
	}
}

// tryLockGrace and tryLockPoll bound tryLock's wait for a lock that a
// forked child holds for a moment.
const (
	tryLockGrace = time.Second
	tryLockPoll  = 20 * time.Millisecond
)

// Use marks b as in use until the returned file is closed: Prune and
// Refresh leave it alone. Pass the file to the TUI process
// (exec.Cmd.ExtraFiles) so the mark lasts as long as the TUI.
func (s *Store) Use(ctx context.Context, b *Build) (*os.File, error) {
	hex := strings.TrimPrefix(b.Digest, "sha256:")
	f, err := s.waitLock(ctx, hex, syscall.LOCK_SH)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(b.Dir, metaFile)); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("%w: %w", ErrNotPulled, err)
	}
	now := time.Now()
	_ = os.Chtimes(filepath.Join(b.Dir, metaFile), now, now)
	return f, nil
}

// InUse reports whether a chat uses b, or a pull or prune of it runs.
func (s *Store) InUse(b *Build) bool {
	unlock, err := s.tryLock(strings.TrimPrefix(b.Digest, "sha256:"))
	if err != nil {
		return true
	}
	unlock()
	return false
}

// ErrInUse is a build that a running chat uses.
var ErrInUse = errors.New("the Hermes build is in use by a running chat or a pull")

// Remove deletes the build of image unless a chat uses it.
func (s *Store) Remove(image string) error {
	hex, err := Key(image)
	if err != nil {
		return err
	}
	if err := s.removeBuild(hex); errors.Is(err, errBusy) {
		return ErrInUse
	} else if err != nil {
		return err
	}
	return nil
}

func (s *Store) removeBuild(hex string) error {
	unlock, err := s.tryLock(hex)
	if err != nil {
		return err
	}
	defer unlock()
	dir := s.buildDir(hex)
	if _, err := os.Lstat(dir); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	trash := filepath.Join(s.Dir, stagingDir, trashTag+hex)
	if err := os.MkdirAll(filepath.Dir(trash), 0o700); err != nil {
		return err
	}
	_ = os.RemoveAll(trash)
	if err := os.Rename(dir, trash); err != nil {
		return err
	}
	return os.RemoveAll(trash)
}

// Prune deletes the builds other than current's and the keep most
// recently used others, and leftovers of pulls that no longer run. It
// skips builds that a chat uses. current may be empty.
func (s *Store) Prune(current string, keep int) error {
	var cur string
	if current != "" {
		hex, err := Key(current)
		if err != nil {
			return err
		}
		cur = hex
	}
	builds, err := s.List()
	if err != nil {
		return err
	}
	var errs []error
	for _, b := range builds {
		hex := strings.TrimPrefix(b.Digest, "sha256:")
		if hex == cur {
			continue
		}
		if keep > 0 {
			keep--
			continue
		}
		switch err := s.removeBuild(hex); {
		case errors.Is(err, errBusy):
			s.log().Info("Keeping a Hermes build that a chat uses", "image", b.Meta.Reference)
		case err != nil:
			errs = append(errs, err)
		default:
			s.log().Info("Removed an old Hermes build", "image", b.Meta.Reference)
		}
	}
	return errors.Join(append(errs, s.pruneStaging())...)
}

// pruneStaging removes pulls whose lock is free, so no pull runs.
func (s *Store) pruneStaging() error {
	ents, err := os.ReadDir(filepath.Join(s.Dir, stagingDir))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range ents {
		name := strings.TrimPrefix(e.Name(), trashTag)
		hex, _, _ := strings.Cut(name, "-")
		unlock, err := s.tryLock(hex)
		if err != nil {
			continue
		}
		if err := os.RemoveAll(filepath.Join(s.Dir, stagingDir, e.Name())); err != nil {
			errs = append(errs, err)
		}
		unlock()
	}
	return errors.Join(errs...)
}
