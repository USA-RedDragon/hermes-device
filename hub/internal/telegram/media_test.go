package telegram_test

import (
	"bytes"
	"testing"

	"github.com/USA-RedDragon/hermes-hub/internal/telegram"
)

// sendMedia sends m from alice in a direct message.
func (f *fakeTelegram) sendMedia(m telegram.Message) {
	m.MessageID, m.From, m.Chat = 1, &telegram.User{ID: aliceTG, FirstName: "u"}, dm(aliceTG)
	f.push(telegram.Update{Message: &m})
}

func (f *fakeTelegram) addFile(id string, data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[id] = data
}

const pngHead = "\x89PNG\r\n\x1a\nrest"

func TestPhotoWithCaption(t *testing.T) {
	t.Parallel()
	h := startBot(t, nil)
	h.tg.addFile("small", []byte("small"))
	h.tg.addFile("big", []byte(pngHead))
	h.tg.sendMedia(telegram.Message{Caption: "what is this?",
		Photo: []telegram.PhotoSize{{FileID: "small", FileSize: 5}, {FileID: "big", FileSize: 12}}})
	p := h.hub.waitPrompt(t, 1)[0]
	if p.Text != "what is this?" || len(p.Files) != 1 || !bytes.Equal(p.Files[0].Data, []byte(pngHead)) || p.Files[0].Name != "photo.jpg" {
		t.Fatalf("prompt = %+v", p)
	}
}

func TestDocumentsAndAlbums(t *testing.T) {
	t.Parallel()
	h := startBot(t, nil)
	h.tg.addFile("doc", []byte("%PDF-1.7"))
	h.tg.sendMedia(telegram.Message{Document: &telegram.File{FileID: "doc", FileName: "../report.pdf"}})
	p := h.hub.waitPrompt(t, 1)[0]
	if p.Text != "" || len(p.Files) != 1 || p.Files[0].Name != "report.pdf" {
		t.Fatalf("document prompt = %+v", p)
	}
	h.tg.addFile("a1", []byte(pngHead))
	h.tg.addFile("a2", []byte("%PDF"))
	h.tg.sendMedia(telegram.Message{MediaGroupID: "g1", Photo: []telegram.PhotoSize{{FileID: "a1"}}})
	h.tg.sendMedia(telegram.Message{MediaGroupID: "g1", Caption: "both",
		Document: &telegram.File{FileID: "a2", FileName: "b.pdf"}})
	p = h.hub.waitPrompt(t, 2)[1]
	if p.Text != "both" || len(p.Files) != 2 || p.Files[1].Name != "b.pdf" {
		t.Fatalf("album prompt = %+v", p)
	}
}

func TestTooLargeFileIsRefused(t *testing.T) {
	t.Parallel()
	h := startBot(t, nil)
	h.tg.sendMedia(telegram.Message{Document: &telegram.File{FileID: "x", FileName: "big.iso",
		FileSize: telegram.MaxDownload + 1}})
	h.tg.wait(mSend, "Telegram lets bots download files up to 20 MB, and big.iso is larger")
	h.tg.addFile("y", []byte("y"))
	h.tg.mu.Lock()
	h.tg.sizes["y"] = telegram.MaxDownload + 1
	h.tg.mu.Unlock()
	h.tg.sendMedia(telegram.Message{Document: &telegram.File{FileID: "y", FileName: "y.bin"}})
	h.tg.wait(mSend, "y.bin is larger")
	h.tg.sendMedia(telegram.Message{Document: &telegram.File{FileID: "gone", FileName: "gone.txt"}})
	h.tg.wait(mSend, "Could not download gone.txt")
	h.tg.addFile("e", []byte{})
	h.tg.sendMedia(telegram.Message{Document: &telegram.File{FileID: "e", FileName: "empty.txt"}})
	h.tg.wait(mSend, "The file empty.txt is empty.")
	if p := h.hub.promptsCopy(); len(p) != 0 {
		t.Fatalf("prompted: %+v", p)
	}
}

func TestPromptNoticeIsSent(t *testing.T) {
	t.Parallel()
	h := startBot(t, nil)
	h.hub.mu.Lock()
	h.hub.notice = "This chat's model cannot read images"
	h.hub.mu.Unlock()
	h.tg.addFile("p", []byte(pngHead))
	h.tg.sendMedia(telegram.Message{Photo: []telegram.PhotoSize{{FileID: "p"}}})
	h.tg.wait(mSend, "cannot read images")
}
