package service_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/USA-RedDragon/hermes-hub/internal/gateway"
	"github.com/USA-RedDragon/hermes-hub/internal/gateway/gatewaytest"
	"github.com/USA-RedDragon/hermes-hub/internal/hub"
	"github.com/USA-RedDragon/hermes-hub/internal/service"
)

const (
	pngHead  = "\x89PNG\r\n\x1a\n..."
	webpHead = "RIFF\x00\x00\x00\x00WEBPVP8 "
	aPNG     = "a.png"
)

type fakeFiles struct {
	path string
	ok   bool
	err  error
	got  []string
}

func (f *fakeFiles) PutFile(_ context.Context, user, backendID, sessionID, name string, data []byte) (string, bool, error) {
	f.got = append(f.got, strings.Join([]string{user, backendID, sessionID, name, string(data)}, "|"))
	return f.path, f.ok, f.err
}

// lastSubmit is the text and attachments of the latest prompt.submit to
// sLive.
func lastSubmit(t *testing.T, gw *gatewaytest.Gateway) (string, []gateway.Attachment) {
	t.Helper()
	s, ok := gw.Session(sLive)
	if !ok || len(s.Attachments) == 0 {
		t.Fatal("no submit")
	}
	text := ""
	for _, m := range s.Messages {
		if m["role"] == roleUser {
			text, _ = m[kText].(string)
		}
	}
	return text, s.Attachments[len(s.Attachments)-1]
}

func TestPromptStagesImages(t *testing.T) {
	t.Parallel()
	e := setup(t, service.Options{})
	ctx := context.Background()
	res, err := e.svc.Prompt(ctx, userAlice, backendPC, sLive, "what is this?",
		hub.Attachment{Name: "a.jpg", Data: []byte(pngHead)}, hub.Attachment{Name: "b", Data: []byte(webpHead)})
	if err != nil || res.Held || res.Notice != "" {
		t.Fatalf("Prompt = %+v, %v", res, err)
	}
	text, atts := lastSubmit(t, e.gw)
	if text != "what is this?" || len(atts) != 2 || atts[0].MIME != "image/png" || atts[1].MIME != "image/webp" ||
		!strings.HasPrefix(atts[0].Path, gatewaytest.StagedImages) || !strings.HasSuffix(atts[0].Path, ".png") {
		t.Fatalf("submit = %q %+v", text, atts)
	}
}

func TestPromptPlacesFiles(t *testing.T) {
	t.Parallel()
	e := setup(t, service.Options{})
	ctx := context.Background()
	doc := hub.Attachment{Name: "dir/notes.txt", Data: []byte("hello")}
	if _, err := e.svc.Prompt(ctx, userAlice, backendPC, sLive, "read it", doc); err != nil {
		t.Fatal(err)
	}
	text, atts := lastSubmit(t, e.gw)
	want := "read it\n\n" + hub.FileNote("notes.txt", gatewaytest.StagedFiles+"notes.txt")
	if text != want || len(atts) != 0 {
		t.Fatalf("submit = %q %+v, want %q", text, atts, want)
	}

	files := &fakeFiles{path: "/home/alice/src/.hermes/attachments/notes-2.txt", ok: true}
	e.svc.SetFiles(files)
	if _, err := e.svc.Prompt(ctx, userAlice, backendPC, sLive, "", doc); err != nil {
		t.Fatal(err)
	}
	text, _ = lastSubmit(t, e.gw)
	if text != hub.FileNote("notes-2.txt", files.path) || len(files.got) != 1 ||
		files.got[0] != "alice|desktop|s-live|notes.txt|hello" {
		t.Fatalf("submit = %q, PutFile calls %v", text, files.got)
	}

	if _, err := e.svc.Prompt(ctx, userAlice, backendPC, sLive, "", hub.Attachment{Name: aPNG, Data: []byte(pngHead)}); err != nil {
		t.Fatal(err)
	}
	if text, atts := lastSubmit(t, e.gw); len(atts) != 1 || text != hub.FileNote("notes-2.txt", files.path) {
		t.Fatalf("an image on a computer = %q %+v", text, atts)
	}

	files.ok, files.path = false, ""
	if _, err := e.svc.Prompt(ctx, userAlice, backendPC, sLive, "", doc); err != nil {
		t.Fatal(err)
	}
	if text, _ = lastSubmit(t, e.gw); !strings.Contains(text, gatewaytest.StagedFiles) {
		t.Fatalf("a file no device took did not go to Hermes: %q", text)
	}

	files.err = errors.New("offline")
	if _, err := e.svc.Prompt(ctx, userAlice, backendPC, sLive, "x", doc); err == nil {
		t.Fatal("a failed upload sent the prompt")
	}
}

func TestPromptRefusesBadAttachments(t *testing.T) {
	t.Parallel()
	e := setup(t, service.Options{})
	ctx := context.Background()
	many := make([]hub.Attachment, hub.MaxAttachments+1)
	for i := range many {
		many[i] = hub.Attachment{Name: "f", Data: []byte("x")}
	}
	big := make([]byte, hub.MaxImageBytes+1)
	copy(big, []byte(pngHead))
	for name, tc := range map[string]struct {
		text  string
		files []hub.Attachment
		want  string
	}{
		"count":   {"x", many, "at most 10 files"},
		"image":   {"x", []hub.Attachment{{Name: "big.png", Data: big}}, "The image big.png is larger than 25 MB"},
		"empty":   {"x", []hub.Attachment{{Name: "e.txt"}}, "The file e.txt is empty"},
		"command": {"/new", []hub.Attachment{{Name: aPNG, Data: []byte(pngHead)}}, "slash command"},
		"forbid":  {"x", []hub.Attachment{{Name: aPNG, Data: []byte(pngHead)}}, ""},
	} {
		user := userAlice
		if name == "forbid" {
			user = userBob
		}
		_, err := e.svc.Prompt(ctx, user, backendPC, sLive, tc.text, tc.files...)
		switch {
		case tc.want == "" && !errors.Is(err, hub.ErrForbidden):
			t.Errorf("%s: err = %v, want forbidden", name, err)
		case tc.want != "" && (!errors.Is(err, hub.ErrAttachment) || !strings.Contains(hub.AttachmentReason(err), tc.want)):
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
	if s, _ := e.gw.Session(sLive); len(s.Attachments) != 0 {
		t.Fatalf("submitted: %+v", s.Attachments)
	}
}

func TestPromptNoticeWithoutVision(t *testing.T) {
	t.Parallel()
	e := setup(t, service.Options{})
	ctx := context.Background()
	e.gw.AddSession(&gatewaytest.Session{ID: "blind", Live: true, Tools: map[string][]string{"file": {"read_file"}}})
	e.gw.AddSession(&gatewaytest.Session{ID: "sees", Live: true, Tools: map[string][]string{"vision": {gateway.VisionTool}}})
	img := hub.Attachment{Name: aPNG, Data: []byte(pngHead)}
	res, err := e.svc.Prompt(ctx, userAlice, backendPC, "blind", "look", img)
	if err != nil || !strings.Contains(res.Notice, "no image reader") {
		t.Fatalf("blind Prompt = %+v, %v", res, err)
	}
	if res, err = e.svc.Prompt(ctx, userAlice, backendPC, "blind", "text only", hub.Attachment{Name: "a.txt", Data: []byte("a")}); err != nil || res.Notice != "" {
		t.Fatalf("file-only Prompt = %+v, %v", res, err)
	}
	if res, err = e.svc.Prompt(ctx, userAlice, backendPC, "sees", "look", img); err != nil || res.Notice != "" {
		t.Fatalf("seeing Prompt = %+v, %v", res, err)
	}
}

func TestPromptWithFilesNeedsTheBackend(t *testing.T) {
	t.Parallel()
	e := setup(t, service.Options{})
	ctx := context.Background()
	e.gw.BeginRestart()
	_, err := e.svc.Prompt(ctx, userAlice, backendPC, sLive, "x", hub.Attachment{Name: aPNG, Data: []byte(pngHead)})
	if !errors.Is(err, hub.ErrAttachment) || !strings.Contains(err.Error(), "restarting") {
		t.Fatalf("err = %v", err)
	}
	if !bytes.Contains([]byte(hub.AttachmentReason(err)), []byte("Send them again")) {
		t.Fatalf("reason = %q", hub.AttachmentReason(err))
	}
}
