package service_test

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/USA-RedDragon/talaria/hub/internal/gateway/gatewaytest"
	"github.com/USA-RedDragon/talaria/hub/internal/hub"
)

// deviceFiles is a device that takes files and serves them back.
type deviceFiles struct{ fakeFiles }

func (d *deviceFiles) DeviceMedia(_ context.Context, _, _, _, p string) (*hub.MediaFile, bool, error) {
	return &hub.MediaFile{Name: p, Size: int64(len(pngHead)), Body: io.NopCloser(strings.NewReader(pngHead))}, true, nil
}

func TestSteerWithFilesSavesThemAndSteersText(t *testing.T) {
	t.Parallel()
	e := setup(t, fastSteer())
	running(t, e)
	files := &fakeFiles{path: "/home/alice/src/.hermes/attachments/shot.png", ok: true}
	e.svc.SetFiles(files)
	res, err := e.svc.Prompt(hub.WithSteer(context.Background()), userAlice, backendPC, sLive, textSteer,
		hub.Attachment{Name: "shot.png", Data: []byte(pngHead)}, hub.Attachment{Name: "rec.mp4", Data: []byte("video")})
	if err != nil || !res.Steered || res.Held {
		t.Fatalf("prompt = %+v %v", res, err)
	}
	if got := e.gw.Steered(); len(got) != 1 {
		t.Fatalf("steered = %v", got)
	}
	s, _ := e.gw.Session(sLive)
	if len(s.Pending) != 1 {
		t.Fatalf("pending = %+v", s.Pending)
	}
	note := hub.FileNote("shot.png", files.path)
	if txt := s.Pending[0].Text; !strings.HasPrefix(txt, textSteer) || strings.Count(txt, note) != 2 {
		t.Fatalf("steered text = %q", txt)
	}
	if n := calls(e, "image.attach_bytes"); n != 0 {
		t.Fatalf("a steer staged %d inline images", n)
	}
}

func TestSteerWithAFileOnHermesNamesItsPath(t *testing.T) {
	t.Parallel()
	e := setup(t, fastSteer())
	running(t, e)
	res, err := e.svc.Prompt(hub.WithSteer(context.Background()), userAlice, backendPC, sLive, "",
		hub.Attachment{Name: "shot.png", Data: []byte(pngHead)})
	if err != nil || !res.Steered {
		t.Fatalf("prompt = %+v %v", res, err)
	}
	s, _ := e.gw.Session(sLive)
	if want := hub.FileNote("shot.png", gatewaytest.StagedFiles+"shot.png"); len(s.Pending) != 1 || s.Pending[0].Text != want {
		t.Fatalf("pending = %+v, want %q", s.Pending, want)
	}
}

func TestQueuedImageIsServedBeforeItsTurnRuns(t *testing.T) {
	t.Parallel()
	e := setup(t, fastSteer())
	running(t, e)
	d := &deviceFiles{fakeFiles{path: "/home/alice/src/.hermes/attachments/shot.png", ok: true}}
	e.svc.SetFiles(d)
	ctx := context.Background()
	// Not a steer: the message queues behind the running turn.
	if _, err := e.svc.Prompt(ctx, userAlice, backendPC, sLive, "look", hub.Attachment{Name: "shot.png", Data: []byte(pngHead)}); err != nil {
		t.Fatal(err)
	}
	s, _ := e.gw.Session(sLive)
	if len(s.Pending) != 1 {
		t.Fatalf("pending = %+v", s.Pending)
	}
	f, err := e.svc.Media(ctx, userAlice, backendPC, sLive, d.path)
	if err != nil {
		t.Fatalf("a queued message's image: %v", err)
	}
	_ = f.Body.Close()
	if _, err := e.svc.Media(ctx, userAlice, backendPC, sLive, "/home/alice/src/.hermes/attachments/other.png"); err == nil {
		t.Fatal("served an image no message names")
	}
}
