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
	h.hub.media = map[string]fakeFile{shotPath: {mime: hub.MIMEJPEG, data: jpegData}, "/opt/data/out/report.pdf": {data: pdfData},
		"/home/alice/src/plot.png": {err: hub.ErrMediaOffline}}
	h.hub.mu.Unlock()
	sid := h.startTurn(t)
	h.hub.emit(t, sid, hub.KindMessageComplete, map[string]any{kText: "Here is the page.\nMEDIA:" + shotPath +
		"\nAnd the report: MEDIA:/opt/data/out/report.pdf\nMEDIA:/opt/data/out/gone.png\nMEDIA:/home/alice/src/plot.png", kStatus: statusComplete})
	text := h.tg.wait(mSend, "Here is the page")
	if strings.Contains(text.text(), "MEDIA:") || !strings.Contains(text.text(), "And the report:") {
		t.Fatalf("text = %q", text.text())
	}
	photo := h.tg.wait(mPhoto, "")
	if photo.Params["field"] != "photo" || photo.Params["name"] != shotName || photo.Params["data"] != jpegData {
		t.Fatalf("photo = %+v", photo.Params)
	}
	doc := h.tg.wait(mDocument, "")
	if doc.Params["field"] != "document" || doc.Params["name"] != "report.pdf" || doc.Params["data"] != pdfData {
		t.Fatalf("document = %+v", doc.Params)
	}
	h.tg.wait(mSend, "gone.png: it is no longer there")
	h.tg.wait(mSend, "plot.png: the computer that has it is offline")
	h.hub.mu.Lock()
	calls := h.hub.mediaCalls
	h.hub.mu.Unlock()
	if len(calls) != 4 || calls[0] != userAlice+" "+beCluster+" "+sid+" "+shotPath {
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
	if doc.Params["name"] != shotName || doc.Params["data"] != jpegData {
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
	h.tg.wait(mSend, "f9.png: it is no longer there")
	h.hub.mu.Lock()
	n := len(h.hub.mediaCalls)
	h.hub.mu.Unlock()
	if n != 10 {
		t.Fatalf("media calls = %d", n)
	}
}

func TestReplyPagesAreLinks(t *testing.T) {
	t.Parallel()
	const page = "<!doctype html><title>Pick &lt;entries&gt;</title><p>hi</p>"
	file := "/opt/data/out/pages/plan.html"
	h := startBot(t, nil)
	h.hub.mu.Lock()
	h.hub.media = map[string]fakeFile{file: {data: "<title>The plan</title>"}}
	h.hub.mu.Unlock()
	sid := h.startTurn(t)
	h.hub.emit(t, sid, hub.KindMessageComplete, map[string]any{kText: "Pick some.\n```page\n" + page + "\n```\nMEDIA:" + file +
		"\nMEDIA:/opt/data/out/gone.html", kStatus: statusComplete})
	text := h.tg.wait(mSend, "Pick some.")
	if strings.Contains(text.text(), "doctype") || strings.Contains(text.text(), "MEDIA:") {
		t.Fatalf("text = %q", text.text())
	}
	link := h.tg.wait(mSend, "Pick &lt;entries&gt;")
	want := "https://hub.example/#/page/" + beCluster + "/" + sid + "/" + hub.PageID(page)
	if !strings.Contains(link.text(), `<a href="`+want+`">`) {
		t.Fatalf("page link = %q, want %q", link.text(), want)
	}
	file1 := h.tg.wait(mSend, "The plan")
	if !strings.Contains(file1.text(), "/"+sid+"/%2Fopt%2Fdata%2Fout%2Fpages%2Fplan.html\"") {
		t.Fatalf("file page link = %q", file1.text())
	}
	h.tg.wait(mSend, "gone.html: it is no longer there")
	if len(h.tg.find(mDocument, "")) != 0 {
		t.Fatal("a page went as a file")
	}
}

func TestMarkdownReplyMediaIsSent(t *testing.T) {
	t.Parallel()
	h := startBot(t, nil)
	h.hub.mu.Lock()
	h.hub.media = map[string]fakeFile{shotPath: {mime: hub.MIMEJPEG, data: jpegData}, "/opt/data/out/my report.pdf": {data: pdfData}}
	h.hub.mu.Unlock()
	sid := h.startTurn(t)
	h.hub.emit(t, sid, hub.KindMessageComplete, map[string]any{kText: "A plot: ![the plot](" + shotPath +
		")\nThe [report](</opt/data/out/my report.pdf>) and [docs](https://example.com/a.pdf) and ![rel](a.png)", kStatus: statusComplete})
	text := h.tg.wait(mSend, "A plot")
	if strings.Contains(text.text(), shotPath) || !strings.Contains(text.text(), "![rel](a.png)") || !strings.Contains(text.text(), "the plot") || !strings.Contains(text.text(), "report") ||
		!strings.Contains(text.text(), "example.com/a.pdf") {
		t.Fatalf("text = %q", text.text())
	}
	photo := h.tg.wait(mPhoto, "")
	if photo.Params["name"] != "shot.jpg" || photo.Params["data"] != jpegData {
		t.Fatalf("photo = %+v", photo.Params)
	}
	doc := h.tg.wait(mDocument, "")
	if doc.Params["name"] != "my report.pdf" || doc.Params["data"] != pdfData {
		t.Fatalf("document = %+v", doc.Params)
	}
}
