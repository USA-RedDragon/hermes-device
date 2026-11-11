package telegram

import (
	"context"
	"errors"
	"fmt"

	"github.com/USA-RedDragon/hermes-hub/internal/hub"
)

const (
	msgTooBig       = "Telegram lets bots download files up to %d MB, and %s is larger. Send a smaller file, or attach it in the hub's web page."
	msgNoDownload   = "Could not download %s from Telegram. Send it again."
	downloadTimeout = 2 * callTimeout
)

// media is one file a message carries.
type media struct {
	id, name string
	size     int64
}

func hasMedia(m *Message) bool { return mediaOf(m).id != "" }

// mediaOf is the file a message carries: the largest size of a photo
// that a bot may download, or a document, video, audio file or voice note.
func mediaOf(m *Message) media {
	if len(m.Photo) > 0 {
		best := m.Photo[len(m.Photo)-1]
		for i := len(m.Photo) - 1; i >= 0; i-- {
			if m.Photo[i].FileSize <= MaxDownload {
				best = m.Photo[i]
				break
			}
		}
		return media{id: best.FileID, name: "photo.jpg", size: best.FileSize}
	}
	for _, f := range []struct {
		f    *File
		name string
	}{{m.Document, "file"}, {m.Video, "video.mp4"}, {m.Audio, "audio"}, {m.Voice, "voice.ogg"}} {
		if f.f != nil && f.f.FileID != "" {
			name := f.f.FileName
			switch {
			case name != "":
			case f.f.MIMEType == "application/pdf":
				name = "document.pdf"
			default:
				name = f.name
			}
			return media{id: f.f.FileID, name: hub.CleanName(name), size: f.f.FileSize}
		}
	}
	return media{}
}

// promptMedia downloads the files of a message or an album and sends them
// with the caption as one prompt.
func (b *Bot) promptMedia(ctx context.Context, to dest, user string, ms []*Message) {
	text := ""
	var files []hub.Attachment
	for _, m := range ms {
		if text == "" {
			text = m.Caption
		}
		f := mediaOf(m)
		if f.id == "" {
			continue
		}
		if f.size > MaxDownload {
			b.sendText(ctx, to, fmt.Sprintf(msgTooBig, MaxDownload>>20, f.name))
			return
		}
		cctx, cancel := context.WithTimeout(ctx, downloadTimeout)
		data, err := b.api.Download(cctx, f.id, MaxDownload)
		cancel()
		if errors.Is(err, ErrTooLarge) {
			b.sendText(ctx, to, fmt.Sprintf(msgTooBig, MaxDownload>>20, f.name))
			return
		}
		if err != nil {
			b.logger.Warn("Failed to download a Telegram file", "chat", to.chat, "error", err)
			b.sendText(ctx, to, fmt.Sprintf(msgNoDownload, f.name))
			return
		}
		files = append(files, hub.Attachment{Name: f.name, Data: data})
	}
	b.prompt(ctx, to, user, text, files...)
}
