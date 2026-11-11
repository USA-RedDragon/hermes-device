package service

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/USA-RedDragon/hermes-hub/internal/gateway"
	"github.com/USA-RedDragon/hermes-hub/internal/hub"
)

var errAway = fmt.Errorf("%w: Hermes is restarting or cannot be reached, so the attachments were not sent. "+
	"Send them again when it is back", hub.ErrAttachment)

const noticeNoVision = "This chat's model cannot read images, and Hermes has no image reader set up, " +
	"so the agent may not see the images."

// SetFiles makes the service place attached files on the devices that run
// sessions. Without it every file goes to Hermes.
func (s *Service) SetFiles(f hub.SessionFiles) { s.files = f }

// imageExt names a staged image after its type: the gateway takes the
// extension from the file name before the bytes.
func imageExt(mime string) string {
	switch mime {
	case hub.MIMEJPEG:
		return ".jpg"
	case hub.MIMEGIF:
		return ".gif"
	case hub.MIMEWebP:
		return ".webp"
	}
	return ".png"
}

// stage hands a prompt's attachments to the backend. Images are staged
// with image.attach_bytes for the model. Other files go to the device
// that runs the session, else to Hermes with file.attach, and the text
// gets a line that says where each one is. It returns the text, the
// staged images and a notice for the user.
func (b *backend) stage(ctx context.Context, user, sessionID, text string, files []hub.Attachment) (string, []gateway.Attachment, string, error) {
	ls, err := b.session(ctx, sessionID)
	if err != nil {
		return "", nil, "", stageError("the attachments", err)
	}
	defer ls.unpin()
	var atts []gateway.Attachment
	var notes []string
	for _, f := range files {
		name := hub.CleanName(f.Name)
		if mime := hub.ImageMIME(f.Data); mime != "" {
			var out gateway.AttachBytesResult
			err := b.call(ctx, ls, gateway.MethodAttachBytes, gateway.AttachBytesParams{SessionID: ls.id,
				ContentBase64: base64.StdEncoding.EncodeToString(f.Data), Filename: "upload" + imageExt(mime)}, &out)
			if err != nil {
				return "", nil, "", stageError(name, err)
			}
			if out.Path == "" {
				return "", nil, "", fmt.Errorf("%w: Hermes did not take %s", hub.ErrAttachment, name)
			}
			atts = append(atts, gateway.Attachment{Path: out.Path, MIME: mime})
			continue
		}
		p, err := b.placeFile(ctx, ls, user, sessionID, name, f.Data)
		if err != nil {
			return "", nil, "", err
		}
		notes = append(notes, hub.FileNote(path.Base(p), p))
	}
	if len(notes) > 0 {
		if text != "" {
			text += "\n\n"
		}
		text += strings.Join(notes, "\n")
	}
	notice := ""
	if v := ls.canSee(); len(atts) > 0 && v != nil && !*v {
		notice = noticeNoVision
	}
	return text, atts, notice, nil
}

// placeFile puts a file where the session's tools can read it and returns
// its path there.
func (b *backend) placeFile(ctx context.Context, ls *live, user, sessionID, name string, data []byte) (string, error) {
	if f := b.svc.files; f != nil {
		p, ok, err := f.PutFile(ctx, user, b.cfg.ID, sessionID, name, data)
		if err != nil {
			return "", err
		}
		if ok {
			return p, nil
		}
	}
	var out gateway.FileAttachResult
	err := b.call(ctx, ls, gateway.MethodFileAttach, gateway.FileAttachParams{SessionID: ls.id, Name: name,
		DataURL: "data:application/octet-stream;base64," + base64.StdEncoding.EncodeToString(data)}, &out)
	if err != nil {
		return "", stageError(name, err)
	}
	if out.Path == "" {
		return "", fmt.Errorf("%w: Hermes did not take %s", hub.ErrAttachment, name)
	}
	return out.Path, nil
}

// stageError says why a backend refused an attachment.
func stageError(name string, err error) error {
	var r *gateway.RPCError
	switch {
	case gateway.Retryable(err):
		return errAway
	case errors.As(err, &r) && r.Message == gateway.ReasonInvalidParams:
		return fmt.Errorf("%w: Hermes refused %s: it may be damaged, too large or of a type it does not take", hub.ErrAttachment, name)
	}
	return refusal(err)
}
