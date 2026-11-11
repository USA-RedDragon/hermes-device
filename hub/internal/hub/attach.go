package hub

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Attachment is a file a user sends with a prompt.
type Attachment struct {
	Name string
	Data []byte
}

// Attachment limits. MaxImageBytes and MaxImages are the gateway's
// (tui_gateway/prompt_attachments.py _ATTACH_BYTES_MAX_BYTES,
// gateway/session_ingress_media.py _ATTACHMENT_LIMIT); MaxFileBytes is the
// device plugin's upload limit (hermes-device docs/protocol.md section 9).
const (
	MaxImageBytes = 25 << 20
	MaxImages     = 10
	MaxFileBytes  = 32 << 20
	// MaxAttachments and MaxAttachmentBytes bound one prompt.
	MaxAttachments     = 10
	MaxAttachmentBytes = 64 << 20
)

// ErrAttachment wraps a message that says why the hub did not send a
// prompt with its attachments.
var ErrAttachment = errors.New("attachment refused")

// SessionFiles places attached files on the device that runs a session.
type SessionFiles interface {
	// PutFile writes a file into the folder of the device that runs the
	// session and returns its path there. ok is false when no device runs
	// the session (a backend without devices, or a session that runs on
	// the Hermes server): Hermes then keeps the file itself.
	PutFile(ctx context.Context, user, backendID, sessionID, name string, data []byte) (path string, ok bool, err error)
}

// Image types the gateway takes with a prompt.
const (
	MIMEPNG  = "image/png"
	MIMEJPEG = "image/jpeg"
	MIMEGIF  = "image/gif"
	MIMEWebP = "image/webp"
)

// ImageMIMEs are the image types the gateway takes with a prompt.
func ImageMIMEs() []string { return []string{MIMEPNG, MIMEJPEG, MIMEGIF, MIMEWebP} }

// ImageMIME is the type of an image the gateway takes, read from its
// first bytes as Hermes does (gateway/session_ingress_media.py
// sniff_image_mime), or "" for any other file.
func ImageMIME(data []byte) string {
	switch {
	case bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")):
		return MIMEPNG
	case bytes.HasPrefix(data, []byte("\xff\xd8\xff")):
		return MIMEJPEG
	case bytes.HasPrefix(data, []byte("GIF87a")), bytes.HasPrefix(data, []byte("GIF89a")):
		return MIMEGIF
	case len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		return MIMEWebP
	}
	return ""
}

// CheckAttachments refuses attachments over the limits, with a message
// for the user.
func CheckAttachments(files []Attachment) error {
	if len(files) > MaxAttachments {
		return fmt.Errorf("%w: Attach at most %d files to one message", ErrAttachment, MaxAttachments)
	}
	total, images := 0, 0
	for _, f := range files {
		total += len(f.Data)
		name := CleanName(f.Name)
		switch {
		case len(f.Data) == 0:
			return fmt.Errorf("%w: The file %s is empty", ErrAttachment, name)
		case ImageMIME(f.Data) != "":
			images++
			if len(f.Data) > MaxImageBytes {
				return fmt.Errorf("%w: The image %s is larger than %d MB, the most Hermes takes for an image", ErrAttachment, name, MaxImageBytes>>20)
			}
		case len(f.Data) > MaxFileBytes:
			return fmt.Errorf("%w: The file %s is larger than %d MB, the most the hub takes for a file", ErrAttachment, name, MaxFileBytes>>20)
		}
	}
	if images > MaxImages {
		return fmt.Errorf("%w: Attach at most %d images to one message", ErrAttachment, MaxImages)
	}
	if total > MaxAttachmentBytes {
		return fmt.Errorf("%w: The attachments add up to more than %d MB", ErrAttachment, MaxAttachmentBytes>>20)
	}
	return nil
}

// maxNameBytes bounds a cleaned file name.
const maxNameBytes = 120

var unsafeName = regexp.MustCompile(`[\x00-\x1f\x7f/\\]+`)

// CleanName is a file name fit to show and to send: the last path part,
// without control characters or slashes, at most 120 bytes, else
// "attachment".
func CleanName(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	name = cutUTF8(name, 4*maxNameBytes)
	name = strings.TrimSpace(unsafeName.ReplaceAllString(name, "_"))
	name = strings.TrimSpace(cutUTF8(strings.TrimLeft(name, "."), maxNameBytes))
	if name == "" {
		return "attachment"
	}
	return name
}

// cutUTF8 cuts s to at most n bytes, at a rune boundary.
func cutUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// FileNote is the line a prompt carries for an attached file, so the
// agent knows where it is. Web pages parse it back to show the file.
func FileNote(name, path string) string {
	return "[The user attached a file: " + name + ". It is saved at " + path +
		". Read it with your tools when the request needs its contents.]"
}

// AttachmentReason is the message for the user that an ErrAttachment
// wraps.
func AttachmentReason(err error) string {
	msg := err.Error()
	if _, after, ok := strings.Cut(msg, ErrAttachment.Error()+": "); ok {
		msg = after
	}
	if msg == "" {
		return "Hermes did not take the attachments"
	}
	return msg
}
