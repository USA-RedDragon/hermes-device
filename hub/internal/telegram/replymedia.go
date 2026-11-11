package telegram

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"

	"github.com/USA-RedDragon/hermes-hub/internal/hub"
)

const (
	msgMediaGone     = "Could not send %s: it is no longer on the Hermes server."
	msgMediaTooLarge = "Could not send %s: Telegram takes files up to %d MB from bots. Open it in the hub's web page."
	msgMediaFailed   = "Could not send %s. Open it in the hub's web page."
)

// sendMedia sends the files a reply names, after its text: an image as a
// photo when Telegram takes it as one, anything else as a document.
func (r *renderer) sendMedia(ctx context.Context, paths []string) {
	for _, p := range paths {
		name := hub.CleanName(path.Base(p))
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
		case err != nil:
			r.logger.Warn("Failed to read a file a reply names", "error", err)
			r.notice(ctx, msgMediaFailed, name)
			continue
		}
		if mime != "" && len(data) <= MaxPhotoUpload {
			err = r.api.SendPhoto(ctx, r.chatID, name, data)
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.Code != http.StatusBadRequest {
				r.sent(ctx, name, err)
				continue
			}
			r.logger.Info("Telegram refused a photo, sending it as a file", "error", err)
		}
		r.sent(ctx, name, r.api.SendDocument(ctx, r.chatID, name, data))
	}
}

func (r *renderer) readMedia(ctx context.Context, p string) ([]byte, string, error) {
	cctx, cancel := context.WithTimeout(ctx, downloadTimeout)
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
