package telegram

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"time"

	"github.com/USA-RedDragon/hermes-hub/internal/hub"
)

const (
	msgMediaGone     = "Could not send %s: it is no longer there."
	msgMediaOffline  = "Could not send %s: the computer that has it is offline. Open it in the hub's web page when the computer is back."
	msgMediaTooLarge = "Could not send %s: Telegram takes files up to %d MB from bots. Open it in the hub's web page."
	msgMediaFailed   = "Could not send %s. Open it in the hub's web page."
	msgMediaMany     = "The reply names %d files; this chat gets the first %d. Open the chat in the hub's web page for the rest."
)

// maxReplyMedia bounds the files the bot sends for one reply.
const maxReplyMedia = 10

// mediaReadTimeout bounds reading one file a reply names. A file on a
// device can take the plugin's 120 seconds and its transfer (hermes-device
// plugin/device/core.py FILE_READ_TIMEOUT_S).
const mediaReadTimeout = 150 * time.Second

// uploadTimeout bounds one upload to Telegram.
const uploadTimeout = 2 * time.Minute

// sendMedia sends the files a reply names, after its text: an image as a
// photo when Telegram takes it as one, anything else as a document.
func (r *renderer) sendMedia(ctx context.Context, paths []string) {
	if len(paths) > maxReplyMedia {
		r.notice(ctx, msgMediaMany, len(paths), maxReplyMedia)
		paths = paths[:maxReplyMedia]
	}
	for _, p := range paths {
		name := hub.CleanName(path.Base(p))
		if hub.PagePath(p) && r.pageURL != nil {
			r.sendPageFile(ctx, p, name)
			continue
		}
		if r.media == nil {
			r.notice(ctx, msgMediaFailed, name)
			continue
		}
		data, mime, err := r.readMedia(ctx, p)
		switch {
		case errors.Is(err, hub.ErrNoMedia):
			r.notice(ctx, msgMediaGone, name)
			continue
		case errors.Is(err, hub.ErrMediaTooLarge):
			r.notice(ctx, msgMediaTooLarge, name, MaxDocumentUpload>>20)
			continue
		case errors.Is(err, hub.ErrMediaOffline):
			r.notice(ctx, msgMediaOffline, name)
			continue
		case err != nil:
			r.logger.Warn("Failed to read a file a reply names", "error", err)
			r.notice(ctx, msgMediaFailed, name)
			continue
		}
		if mime != "" && len(data) <= MaxPhotoUpload {
			err = r.upload(ctx, r.api.SendPhoto, name, data)
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.Code != http.StatusBadRequest {
				r.sent(ctx, name, err)
				continue
			}
			r.logger.Info("Telegram refused a photo, sending it as a file", "error", err)
		}
		r.sent(ctx, name, r.upload(ctx, r.api.SendDocument, name, data))
	}
}

func (r *renderer) upload(ctx context.Context, send func(context.Context, int64, string, []byte) error, name string, data []byte) error {
	cctx, cancel := context.WithTimeout(ctx, uploadTimeout)
	defer cancel()
	return send(cctx, r.chatID, name, data)
}

func (r *renderer) readMedia(ctx context.Context, p string) ([]byte, string, error) {
	cctx, cancel := context.WithTimeout(ctx, mediaReadTimeout)
	defer cancel()
	f, err := r.media(cctx, p)
	if err != nil {
		return nil, "", err
	}
	defer f.Body.Close()
	if f.Size > MaxDocumentUpload {
		return nil, "", hub.ErrMediaTooLarge
	}
	data, err := io.ReadAll(io.LimitReader(f.Body, MaxDocumentUpload+1))
	if err != nil {
		return nil, "", err
	}
	if len(data) > MaxDocumentUpload {
		return nil, "", hub.ErrMediaTooLarge
	}
	return data, f.MIME, nil
}

func (r *renderer) sent(ctx context.Context, name string, err error) {
	if err != nil {
		r.logger.Warn("Failed to send a file", "error", err)
		r.notice(ctx, msgMediaFailed, name)
	}
}

func (r *renderer) notice(ctx context.Context, format string, args ...any) {
	r.send(ctx, escapeHTML(fmt.Sprintf(format, args...)), nil)
}

// sendPages sends a link to the hub page that opens each page block of a
// reply. Telegram cannot run a page, and the user's browser opens the link.
func (r *renderer) sendPages(ctx context.Context, pages []string) {
	if r.pageURL == nil {
		return
	}
	for _, p := range pages {
		r.pageLink(ctx, hub.PageTitle(p), r.pageURL(hub.PageID(p)))
	}
}

// sendPageFile sends a link that opens an HTML file a reply names as a page.
func (r *renderer) sendPageFile(ctx context.Context, p, name string) {
	title := ""
	if r.media != nil {
		cctx, cancel := context.WithTimeout(ctx, downloadTimeout)
		f, err := r.media(cctx, p)
		if errors.Is(err, hub.ErrNoMedia) {
			cancel()
			r.notice(ctx, msgMediaGone, name)
			return
		}
		if err == nil {
			head, _ := io.ReadAll(io.LimitReader(f.Body, 64<<10))
			_ = f.Body.Close()
			title = hub.PageTitle(string(head))
		}
		cancel()
	}
	if title == "" {
		title = name
	}
	r.pageLink(ctx, title, r.pageURL(p))
}

func (r *renderer) pageLink(ctx context.Context, title, link string) {
	if title == "" {
		title = "Page"
	}
	r.send(ctx, "<b>"+escapeHTML(title)+"</b>\n<a href=\""+escapeHTML(link)+"\">Open the page in the hub</a>", nil)
}
