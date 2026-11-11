package telegram

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/USA-RedDragon/hermes-hub/internal/hub"
	"github.com/google/uuid"
)

// echoLen is how much of a prompt sent elsewhere a chat shows.
const echoLen = 1000

// promptPrefix starts the submission IDs of the prompts the bot sends.
const promptPrefix = "tg:"

// promptID is the submission ID of a prompt sent in to: base, which the
// bot derives from the Telegram update, with the chat and topic, so a
// stream can tell its own prompts from those sent elsewhere.
func promptID(to dest, base string) string {
	if base == "" {
		base = uuid.NewString()
	}
	return promptPrefix + strconv.FormatInt(to.chat, 10) + ":" + strconv.FormatInt(to.thread, 10) + ":" + base
}

// promptDest is the chat and topic a promptID names.
func promptDest(id string) (dest, bool) {
	rest, ok := strings.CutPrefix(id, promptPrefix)
	if !ok {
		return dest{}, false
	}
	parts := strings.SplitN(rest, ":", 3)
	if len(parts) != 3 {
		return dest{}, false
	}
	chat, err1 := strconv.ParseInt(parts[0], 10, 64)
	thread, err2 := strconv.ParseInt(parts[1], 10, 64)
	if err1 != nil || err2 != nil {
		return dest{}, false
	}
	return dest{chat: chat, thread: thread}, true
}

// The lines that Hermes and the hub put in a prompt for its attachments,
// as the web UI reads them (splitAttachments in app.js).
var (
	imageHintRE = regexp.MustCompile(`^\[Image attached(?: at)?: (.+)\]$`)
	imageRefRE  = regexp.MustCompile("^@image:(?:`([^`]+)`|\"([^\"]+)\"|'([^']+)'|(\\S+))$")
	imageDataRE = regexp.MustCompile(`^data:image/(?:png|jpeg|gif|webp);base64,[A-Za-z0-9+/=]+$`)
	placeholdRE = regexp.MustCompile(`^\[screenshot(?: detail=\w+)?\]$`)
	fileNoteRE  = regexp.MustCompile(`^\[The user attached a file: (.+)\. It is saved at (/.*)\. Read it with your tools when the request needs its contents\.\]$`)
	pdfNoteRE   = regexp.MustCompile(`^\[The PDF (.+) has (\d+) pages?\. .*\]$`)
	pdfFailedRE = regexp.MustCompile(`^\[The hub could not read the PDF .+\. Its pages are not shown\.\]$`)
	pdfStartRE  = regexp.MustCompile(`^<untrusted-pdf-text id="([0-9a-f]{16})">$`)
)

// imageCaption is the text Hermes gives an image sent with no text.
const imageCaption = "What do you see in this image?"

// splitPrompt takes the attachment lines out of a prompt and returns the
// rest and the names of the attachments.
func splitPrompt(text string) (string, []string) {
	var keep, names []string
	files, images := false, false
	skipTo := ""
	for line := range strings.SplitSeq(text, "\n") {
		if skipTo != "" {
			if line == skipTo {
				skipTo = ""
			}
			continue
		}
		var m []string
		switch {
		case files && pdfStartRE.MatchString(line):
			skipTo = `</untrusted-pdf-text id="` + pdfStartRE.FindStringSubmatch(line)[1] + `">`
		case files && (pdfNoteRE.MatchString(line) || pdfFailedRE.MatchString(line)):
		case imageHintRE.MatchString(line):
			names, images = append(names, path.Base(imageHintRE.FindStringSubmatch(line)[1])), true
		case imageRefRE.MatchString(line):
			m = imageRefRE.FindStringSubmatch(line)
			names, images = append(names, path.Base(m[1]+m[2]+m[3]+m[4])), true
		case imageDataRE.MatchString(line):
			names, images = append(names, "an image"), true
		case fileNoteRE.MatchString(line):
			names, files = append(names, fileNoteRE.FindStringSubmatch(line)[1]), true
		case placeholdRE.MatchString(line):
		default:
			keep = append(keep, line)
		}
	}
	if len(names) == 0 {
		return text, nil
	}
	rest := strings.TrimSpace(strings.Join(keep, "\n"))
	if rest == imageCaption && images {
		rest = ""
	}
	return rest, names
}

// echoHTML is a prompt sent elsewhere as a chat shows it: where it came
// from, its text cut at echoLen, and its attachments.
func echoHTML(where, text string, images int) string {
	text, names := splitPrompt(text)
	text = strings.TrimSpace(text)
	switch {
	case images == 1:
		names = append(names, "an image")
	case images > 1:
		names = append(names, fmt.Sprintf("%d images", images))
	}
	if text == "" && len(names) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("<i>You, " + where + ":</i>")
	if text != "" {
		if r := []rune(text); len(r) > echoLen {
			text = strings.TrimSpace(string(r[:echoLen])) + " (…, full text in the hub)"
		}
		b.WriteString("\n" + escapeHTML(text))
	}
	if len(names) > 0 {
		b.WriteString("\n<i>Attached: " + escapeHTML(strings.Join(names, ", ")) + "</i>")
	}
	return b.String()
}

// origin says where a prompt was sent, as "in the hub", and false for a
// prompt sent in this chat, which the chat shows already.
func (r *renderer) origin(id string) (string, bool) {
	if to, ok := promptDest(id); ok {
		return "in Telegram", to != r.to
	}
	if hub.FromHub(id) {
		return "in the hub", true
	}
	src := ""
	if r.source != nil {
		src = r.source()
	}
	if name := sourceName(src); name != "" {
		return "in " + name, true
	}
	return "in another app", true
}

// startPayload is the part of a message.start payload the bot reads.
type startPayload struct {
	Text         string `json:"text"`
	Note         string `json:"note"`
	SubmissionID string `json:"submission_id"`
	StartedAt    int64  `json:"started_at"`
}

// turnStart handles message.start: a cron result or an agent message it
// shows as such, and the prompt of a turn started elsewhere.
func (r *renderer) turnStart(ctx context.Context, ev hub.Event) {
	var p startPayload
	if !r.decode(ev, &p) {
		return
	}
	open := r.open
	r.open = nil
	if p.StartedAt > 0 {
		r.startedAt = time.UnixMilli(p.StartedAt)
	}
	switch {
	case p.Note == "cron":
		r.cronResult(ctx, p.Text)
		return
	case p.Note != "":
		return
	case hub.AgentHeaderRE.MatchString(p.Text):
		r.agentMessage(ctx, p.Text)
		r.echoed = true
		return
	}
	text, id, images := p.Text, p.SubmissionID, 0
	if strings.TrimSpace(text) == "" && open != nil {
		text, images = open.Text, open.Images
		if id == "" {
			id = open.SubmissionID
		}
	}
	r.echo(ctx, text, id, images)
}

// echo shows the prompt of a turn that started elsewhere, once per turn.
func (r *renderer) echo(ctx context.Context, text, id string, images int) {
	if r.echoed {
		return
	}
	r.echoed = true
	where, show := r.origin(id)
	if !show {
		return
	}
	if html := echoHTML(where, text, images); html != "" {
		r.send(ctx, html, nil)
	}
}

// echoOpen shows the prompt of the turn a snapshot found running, when no
// message.start of the turn came before its first step.
func (r *renderer) echoOpen(ctx context.Context) {
	if open := r.open; open != nil {
		r.open = nil
		r.echo(ctx, open.Text, open.SubmissionID, open.Images)
	}
}
