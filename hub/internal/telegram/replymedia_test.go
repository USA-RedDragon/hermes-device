package telegram_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/USA-RedDragon/hermes-hub/internal/hub"
)

const (
	shotPath = "/opt/data/hermes/cache/screenshots/hermes-device/s/shot.jpg"
	jpegData = "\xff\xd8\xff\xe0jpeg"
	// statusComplete is the status of a turn that ended normally.
	statusComplete = "complete"
)

func TestReplyMediaIsSent(t *testing.T) {
	t.Parallel()
	h := startBot(t, nil)
	h.hub.mu.Lock()
	h.hub.media = map[string]fakeFile{shotPath: {mime: hub.MIMEJPEG, data: jpegData}, "/opt/data/out/report.pdf": {data: "%PDF-1.7"}}
	h.hub.mu.Unlock()
	sid := h.startTurn(t)
	h.hub.emit(t, sid, hub.KindMessageComplete, map[string]any{kText: "Here is the page.\nMEDIA:" + shotPath +
		"\nAnd the report: MEDIA:/opt/data/out/report.pdf\nMEDIA:/opt/data/out/gone.png", kStatus: statusComplete})
	text := h.tg.wait(mSend, "Here is the page")
	if strings.Contains(text.text(), "MEDIA:") || !strings.Contains(text.text(), "And the report:") {
		t.Fatalf("text = %q", text.text())
	}
	photo := h.tg.wait(mPhoto, "")
	if photo.Params["field"] != "photo" || photo.Params["name"] != "shot.jpg" || photo.Params["data"] != jpegData {
		t.Fatalf("photo = %+v", photo.Params)
	}
	doc := h.tg.wait(mDocument, "")
	if doc.Params["field"] != "document" || doc.Params["name"] != "report.pdf" || doc.Params["data"] != "%PDF-1.7" {
		t.Fatalf("document = %+v", doc.Params)
	}
	h.tg.wait(mSend, "gone.png: it is no longer on the Hermes server")
	h.hub.mu.Lock()
	calls := h.hub.mediaCalls
	h.hub.mu.Unlock()
	if len(calls) != 3 || calls[0] != userAlice+" "+beCluster+" "+sid+" "+shotPath {
		t.Fatalf("media calls = %q", calls)
	}
}

func TestRefusedPhotoIsSentAsDocument(t *testing.T) {
	t.Parallel()
	h := startBot(t, nil)
	h.tg.mu.Lock()
	h.tg.rejectPhoto = true
	h.tg.mu.Unlock()
	h.hub.mu.Lock()
	h.hub.media = map[string]fakeFile{shotPath: {mime: hub.MIMEJPEG, data: jpegData}}
	h.hub.mu.Unlock()
	sid := h.startTurn(t)
	h.hub.emit(t, sid, hub.KindMessageComplete, map[string]any{kText: "MEDIA:" + shotPath, kStatus: statusComplete})
	doc := h.tg.wait(mDocument, "")
	if doc.Params["name"] != "shot.jpg" || doc.Params["data"] != jpegData {
		t.Fatalf("document = %+v", doc.Params)
	}
	if n := h.tg.find(mSend, "Could not"); len(n) != 0 {
		t.Fatalf("notices = %+v", n)
	}
}

func TestTooLargeReplyMedia(t *testing.T) {
	t.Parallel()
	h := startBot(t, nil)
	h.hub.mu.Lock()
	h.hub.media = map[string]fakeFile{"/opt/data/out/big.zip": {data: strings.Repeat("x", 50<<20+1)}}
	h.hub.mu.Unlock()
	sid := h.startTurn(t)
	h.hub.emit(t, sid, hub.KindMessageComplete, map[string]any{kText: "MEDIA:/opt/data/out/big.zip", kStatus: statusComplete})
	h.tg.wait(mSend, "big.zip: Telegram takes files up to 50 MB")
	if len(h.tg.find(mDocument, "")) != 0 {
		t.Fatal("a file over the limit was sent")
	}
}

func TestReplyMediaIsCapped(t *testing.T) {
	t.Parallel()
	h := startBot(t, nil)
	h.hub.mu.Lock()
	h.hub.media = map[string]fakeFile{}
	h.hub.mu.Unlock()
	sid := h.startTurn(t)
	var text strings.Builder
	for i := range 15 {
		fmt.Fprintf(&text, "MEDIA:/opt/data/out/f%d.png\n", i)
	}
	h.hub.emit(t, sid, hub.KindMessageComplete, map[string]any{kText: text.String(), kStatus: statusComplete})
	h.tg.wait(mSend, "The reply names 15 files; this chat gets the first 10")
	h.tg.wait(mSend, "f9.png: it is no longer on the Hermes server")
	h.hub.mu.Lock()
	n := len(h.hub.mediaCalls)
	h.hub.mu.Unlock()
	if n != 10 {
		t.Fatalf("media calls = %d", n)
	}
}
