package cmd

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/USA-RedDragon/hermes-device/internal/config"
	"github.com/USA-RedDragon/hermes-device/internal/daemonstate"
	"github.com/USA-RedDragon/hermes-device/internal/hermesbuild"
	"github.com/USA-RedDragon/hermes-device/internal/oci"
	"github.com/USA-RedDragon/hermes-device/internal/oci/ocitest"
	"github.com/USA-RedDragon/hermes-device/protocol"
)

const testVersion = "v0.23.1"

const testImage = "registry.example.com/hermes:v0.23.1@sha256:0000000000000000000000000000000000000000000000000000000000000000"

func TestRunRecordsTheServersHermes(t *testing.T) {
	t.Parallel()
	s := setupRun(t, &runHub{image: testImage}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := s.start(ctx)
	waitUntil(t, "the image in the daemon state", func() bool {
		st, err := daemonstate.Read(s.stateDir)
		return err == nil && st.HermesImage == testImage
	})
	cancel()
	if err := waitErr(t, done); err != nil {
		t.Errorf("runDaemon = %v", err)
	}
}

func pushHermes(t *testing.T, r *ocitest.Registry, version string) string {
	t.Helper()
	img := r.Push(t, oci.MediaOCILayerGzip, nil, ocitest.Layer(t,
		ocitest.Entry{Name: "opt/hermes/libexec/hermes", Body: "#!/bin/sh\necho " + version + "\n", Mode: 0o755},
		ocitest.Entry{Name: "opt/hermes/.venv/bin/hermes", Type: tar.TypeSymlink, Link: "../../libexec/hermes"},
	))
	return r.Reference(img.Index)
}

func testBuilds(t *testing.T, r *ocitest.Registry) *hermesbuild.Store {
	t.Helper()
	return &hermesbuild.Store{Dir: filepath.Join(t.TempDir(), "hermes"), HTTP: r.Client(), Arch: "amd64",
		Log: slog.New(slog.DiscardHandler)}
}

func TestFetchLoopFetchesEachImageAndPrunes(t *testing.T) {
	t.Parallel()
	r := ocitest.New(t, "nousresearch/hermes-agent")
	builds := testBuilds(t, r)
	images := []string{pushHermes(t, r, "1"), pushHermes(t, r, "2"), pushHermes(t, r, "3")}
	h := newServerHermes()
	if err := h.start(t.TempDir(), daemonstate.State{PID: 1}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.fetchLoop(ctx, builds)
	}()
	defer func() {
		cancel()
		<-done
	}()
	present := func(image string) bool {
		_, err := builds.Open(image)
		return err == nil
	}
	for i, image := range images {
		h.welcome(protocol.Welcome{HermesVersion: "v1", HermesImage: image})
		waitUntil(t, "build "+image, func() bool { return present(image) })
		if i == 2 {
			waitUntil(t, "the prune of the oldest build", func() bool { return !present(images[0]) })
		}
		// Builds pulled within the same clock tick would tie on last use.
		time.Sleep(20 * time.Millisecond)
	}
	if !present(images[1]) {
		t.Error("the previous build was pruned")
	}
	h.welcome(protocol.Welcome{HermesImage: "registry.example.com/hermes:v1"})
	h.welcome(protocol.Welcome{})
}

// TestStatusAndRefresh is not parallel: a child that a parallel test
// forks holds a copy of every open lock file until it execs, and the build
// would then show as in use.
func TestStatusAndRefresh(t *testing.T) { //nolint:paralleltest // see above
	r := ocitest.New(t, "nousresearch/hermes-agent")
	builds := testBuilds(t, r)
	image := pushHermes(t, r, "1")
	st := &daemonstate.State{HermesVersion: testVersion, HermesImage: image}
	cfg := &config.Config{Chat: config.Chat{Managed: true, Hermes: "hermes"}}
	var out bytes.Buffer
	if err := printStatus(&out, cfg, builds, st); err != nil {
		t.Fatal(err)
	}
	if s := out.String(); !strings.Contains(s, "Server image:  "+image) || !strings.Contains(s, "  none") ||
		!strings.Contains(s, "not on this device yet") {
		t.Errorf("status before the pull:\n%s", s)
	}
	out.Reset()
	if err := refreshBuild(context.Background(), &out, builds, st); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := printStatus(&out, cfg, builds, st); err != nil {
		t.Fatal(err)
	}
	if s := out.String(); !strings.Contains(s, "(current)") || strings.Contains(s, "not on this device yet") {
		t.Errorf("status after the pull:\n%s", s)
	}
	if err := refreshBuild(context.Background(), &out, builds, &daemonstate.State{}); !errors.Is(err, errNoImage) {
		t.Errorf("refresh without an image: %v", err)
	}
	if err := refreshBuild(context.Background(), &out, nil, st); !errors.Is(err, hermesbuild.ErrUnsupported) {
		t.Errorf("refresh with managed builds off: %v", err)
	}
	out.Reset()
	cfg.Chat.Managed = false
	if err := printStatus(&out, cfg, nil, st); err != nil || !strings.Contains(out.String(), "chat.managed is false") {
		t.Errorf("status with managed builds off: %v\n%s", err, out.String())
	}
}

func TestServerHermesIgnoresATooLongImage(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	h := newServerHermes()
	if err := h.start(dir, daemonstate.State{PID: 1}); err != nil {
		t.Fatal(err)
	}
	h.welcome(protocol.Welcome{HermesVersion: testVersion, HermesImage: "registry.example.com/" + strings.Repeat("a", protocol.MaxHermesImage)})
	st, err := daemonstate.Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	if st.HermesImage != "" || st.HermesVersion != testVersion {
		t.Errorf("state = %+v", st)
	}
}
