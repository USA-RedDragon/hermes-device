package hub

import (
	"context"
	"errors"
	"io"
	"path"
	"regexp"
	"slices"
	"strings"
)

// MediaFile is a file the agent named with a MEDIA directive, read from the
// backend's server. The caller closes Body.
type MediaFile struct {
	// Name is the file's base name, cleaned with CleanName.
	Name string
	// MIME is the type of an image the hub shows inline, read from the
	// file's first bytes, or "" for any other file.
	MIME string
	// Size is the file's size in bytes, or -1 when the backend did not say.
	Size int64
	// NoStore says browsers must not cache the file: it comes from a
	// device, where it may change or go away.
	NoStore bool
	Body    io.ReadCloser
}

// Media reads the files the agent named in a session's replies.
type Media interface {
	// Media opens a file that a MEDIA directive in the session names. It
	// fails with ErrNoMedia when no reply or browser_screenshot result of
	// the session names path, when path is outside the backend's media
	// roots, or when the backend has no such file.
	Media(ctx context.Context, user, backendID, sessionID, path string) (*MediaFile, error)
}

// DeviceMedia reads the files the agent names from the device that runs a
// session: the user's computer or the sandbox.
type DeviceMedia interface {
	// DeviceMedia opens path on the device that runs the session. ok is
	// false when no device runs it (a backend without devices, or a session
	// that runs on the Hermes server). It fails with ErrNoMedia when the
	// device has no such file or refuses it, ErrMediaTooLarge for a file
	// over the plugin's limit and ErrMediaOffline when the device is not
	// connected. It does not check that the session names path.
	DeviceMedia(ctx context.Context, user, backendID, sessionID, path string) (f *MediaFile, ok bool, err error)
}

// ErrNoMedia is a file the hub does not show: the session never named it,
// it is outside the media roots, or it is gone.
var ErrNoMedia = errors.New("no such file in this chat")

// ErrMediaOffline is a file on a device that is not connected.
var ErrMediaOffline = errors.New("the computer that has this file is offline")

// ErrMediaTooLarge is a file over MaxMediaBytes.
var ErrMediaTooLarge = errors.New("file too large")

// MaxMediaBytes bounds a file the hub passes on.
const MaxMediaBytes = 64 << 20

// ScreenshotTool is the hermes-device tool that saves a browser screenshot
// for the user and returns its path.
const ScreenshotTool = "browser_screenshot"

// mediaExts are the extensions of files Hermes delivers with a MEDIA
// directive (gateway/platforms/base.py MEDIA_DELIVERY_EXTS).
const mediaExts = `.png .jpg .jpeg .gif .webp .bmp .tiff .svg .mp4 .mov .avi .mkv .webm .3gp .mp3 .m2a .wav .ogg
	.opus .m4a .flac .pdf .docx .doc .odt .rtf .txt .md .epub .xlsx .xls .ods .csv .tsv .json .xml .yaml .yml .kmz .kml
	.geojson .gpx .pptx .ppt .odp .key .zip .tar .gz .tgz .bz2 .xz .7z .rar .apk .ipa .html .htm`

// MediaPath reports whether p is a path the hub may ask a backend for: an
// absolute, clean path of at most 4096 bytes, without control characters,
// whose extension Hermes delivers.
func MediaPath(p string) bool {
	if len(p) < 2 || len(p) > 4096 || p[0] != '/' || path.Clean(p) != p {
		return false
	}
	if strings.ContainsFunc(p, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return false
	}
	ext := strings.ToLower(path.Ext(p))
	return ext != "" && slices.Contains(strings.Fields(mediaExts), ext)
}

var (
	mediaTag   = regexp.MustCompile("(?i)[`\"'*_]{0,3}MEDIA:[ \t]*(`[^`\n]+`|\"[^\"\n]+\"|'[^'\n]+'|/[^\\s`\"'<>]+)")
	mediaGlued = regexp.MustCompile("(?i)MEDIA:")
	inlineCode = regexp.MustCompile("`[^`\n]+`")
	fence      = regexp.MustCompile("^[ \t]{0,3}(```|~~~)")
	mediaAfter = regexp.MustCompile("(?i)MEDIA:[ \t]*$")
	manyBlank  = regexp.MustCompile(`\n{3,}`)
)

// mediaTrail is what may follow a bare path without being part of it.
const mediaTrail = ".,;:!?)]}*_"

// MediaDirectives finds the MEDIA:<path> directives in an assistant reply,
// as Hermes does (gateway/platforms/base.py extract_media): MEDIA: then an
// absolute path, bare or in backticks or quotes, with a file extension
// Hermes delivers, maybe wrapped in quotes or Markdown emphasis. Directives
// in code blocks, in inline code and in quote lines are examples and stay.
// It returns the paths in order without repeats, and text without the
// directives. Paths that are not MediaPath stay in the text.
func MediaDirectives(text string) (paths []string, cleaned string) {
	if !strings.Contains(strings.ToUpper(text), "MEDIA:") {
		return nil, text
	}
	scan := maskMedia(text)
	type span struct{ start, end int }
	var spans []span
	seen := map[string]bool{}
	for pos := 0; pos < len(scan); {
		m := mediaTag.FindStringSubmatchIndex(scan[pos:])
		if m == nil {
			break
		}
		start, pStart, pEnd := pos+m[0], pos+m[2], pos+m[3]
		pos += m[1]
		raw := text[pStart:pEnd]
		var p string
		switch raw[0] {
		case '`', '"', '\'':
			p = strings.TrimSpace(raw[1 : len(raw)-1])
		default:
			if loc := mediaGlued.FindStringIndex(raw); loc != nil {
				raw = raw[:loc[0]]
				pos = pStart + loc[0]
			}
			raw = strings.TrimRight(raw, mediaTrail)
			p = raw
		}
		if !MediaPath(p) {
			continue
		}
		end := pStart + len(raw)
		for n := 0; n < 3 && end < len(text) && strings.IndexByte("`\"'*_", text[end]) >= 0; n++ {
			end++
		}
		if end < len(text) && text[end] == '.' && (end+1 == len(text) || text[end+1] == ' ' || text[end+1] == '\n') {
			end++
		}
		spans = append(spans, span{start, end})
		if !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}
	if len(spans) == 0 {
		return nil, text
	}
	var b strings.Builder
	last := 0
	for _, s := range spans {
		b.WriteString(text[last:s.start])
		last = s.end
	}
	b.WriteString(text[last:])
	return paths, strings.TrimSpace(manyBlank.ReplaceAllString(b.String(), "\n\n"))
}

// MarkdownMedia finds the local files a reply names with standard Markdown:
// an image ![alt](/abs/path.png) or a link [label](/abs/path/file.pdf),
// with the path bare or in angle brackets (<...>, for a path with spaces).
// Only MediaPath paths count; remote URLs and relative paths are ordinary
// Markdown. Fenced code and inline code are examples and stay. It returns
// the paths in order without repeats.
func MarkdownMedia(text string) []string {
	var paths []string
	for _, r := range markdownRefs(text) {
		if !slices.Contains(paths, r.path) {
			paths = append(paths, r.path)
		}
	}
	return paths
}

// MarkdownMediaText is text with each local file that MarkdownMedia finds
// replaced by its alt text or label, for a client that shows the files
// apart from the text.
func MarkdownMediaText(text string) string {
	refs := markdownRefs(text)
	if len(refs) == 0 {
		return text
	}
	var b strings.Builder
	last := 0
	for _, r := range refs {
		b.WriteString(text[last:r.start])
		b.WriteString(r.label)
		last = r.end
	}
	b.WriteString(text[last:])
	return b.String()
}

type mdRef struct {
	start, end int
	label      string
	path       string
}

// markdownRefs finds the images and links of text whose destination is a
// MediaPath, outside code.
func markdownRefs(text string) []mdRef {
	if !strings.Contains(text, "](") {
		return nil
	}
	scan := maskMarkdown(text)
	var refs []mdRef
	for pos := 0; ; {
		i := strings.Index(scan[pos:], "](")
		if i < 0 {
			break
		}
		closer := pos + i
		pos = closer + 2
		p, end, ok := MarkdownDest(scan, pos)
		if !ok || !MediaPath(p) {
			continue
		}
		open, depth := -1, 0
		for k := closer - 1; k >= 0 && scan[k] != '\n'; k-- {
			if scan[k] == ']' {
				depth++
			} else if scan[k] == '[' {
				if depth == 0 {
					open = k
					break
				}
				depth--
			}
		}
		if open < 0 {
			continue
		}
		start := open
		if open > 0 && scan[open-1] == '!' {
			start--
		}
		if len(refs) > 0 && start < refs[len(refs)-1].end {
			continue
		}
		pos = end
		refs = append(refs, mdRef{start, end, text[open+1 : closer], p})
	}
	return refs
}

// MarkdownDest reads the destination of the Markdown link or image whose
// "(" ends just before s[i]: a bare destination (no spaces, balanced
// parentheses) or one in angle brackets, then an optional quoted title and
// ")". It returns the destination and the index after ")".
func MarkdownDest(s string, i int) (dest string, end int, ok bool) {
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	if i >= len(s) {
		return "", 0, false
	}
	if s[i] == '<' {
		j := strings.IndexAny(s[i+1:], ">\n")
		if j < 0 || s[i+1+j] != '>' {
			return "", 0, false
		}
		dest = s[i+1 : i+1+j]
		i += j + 2
	} else {
		depth, j := 0, i
	scan:
		for ; j < len(s); j++ {
			switch s[j] {
			case ' ', '\t', '\n':
				break scan
			case '(':
				depth++
			case ')':
				if depth == 0 {
					break scan
				}
				depth--
			}
		}
		dest = s[i:j]
		i = j
	}
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	if i < len(s) && (s[i] == '"' || s[i] == '\'') {
		j := strings.IndexByte(s[i+1:], s[i])
		if j < 0 {
			return "", 0, false
		}
		i += j + 2
		for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
			i++
		}
	}
	if i >= len(s) || s[i] != ')' {
		return "", 0, false
	}
	return dest, i + 1, true
}

// maskMarkdown blanks fenced code and inline code, keeping offsets.
func maskMarkdown(text string) string {
	b := []byte(text)
	blank := func(i, j int) {
		for k := i; k < j; k++ {
			if b[k] != '\n' {
				b[k] = ' '
			}
		}
	}
	inFence := false
	var marker string
	off := 0
	for line := range strings.SplitAfterSeq(text, "\n") {
		body := strings.TrimRight(line, "\n")
		switch m := fence.FindStringSubmatch(body); {
		case inFence:
			blank(off, off+len(body))
			if m != nil && m[1] == marker {
				inFence = false
			}
		case m != nil:
			inFence, marker = true, m[1]
			blank(off, off+len(body))
		}
		off += len(line)
	}
	masked := string(b)
	for _, m := range inlineCode.FindAllStringIndex(masked, -1) {
		blank(m[0], m[1])
	}
	return string(b)
}

// maskMedia blanks fenced code, inline code and quote lines, keeping
// offsets, so directives there are not found. Inline code that is a whole
// directive (`MEDIA:/a.png`) or the quoted path of one stays.
func maskMedia(text string) string {
	b := []byte(text)
	blank := func(i, j int) {
		for k := i; k < j; k++ {
			if b[k] != '\n' {
				b[k] = ' '
			}
		}
	}
	inFence := false
	var marker string
	off := 0
	for line := range strings.SplitAfterSeq(text, "\n") {
		body := strings.TrimRight(line, "\n")
		switch m := fence.FindStringSubmatch(body); {
		case inFence:
			blank(off, off+len(body))
			if m != nil && m[1] == marker {
				inFence = false
			}
		case m != nil:
			inFence, marker = true, m[1]
			blank(off, off+len(body))
		case strings.HasPrefix(strings.TrimLeft(body, " \t"), ">"):
			blank(off, off+len(body))
		}
		off += len(line)
	}
	masked := string(b)
	for _, m := range inlineCode.FindAllStringIndex(masked, -1) {
		inner := strings.TrimSpace(masked[m[0]+1 : m[1]-1])
		if mediaAfter.MatchString(masked[max(0, m[0]-20):m[0]]) || strings.HasPrefix(strings.ToUpper(inner), "MEDIA:") {
			continue
		}
		blank(m[0], m[1])
	}
	return string(b)
}

var (
	imageHint = regexp.MustCompile(`^\[Image attached at: (.+)\]$`)
	imageRef  = regexp.MustCompile("^@image:(?:`([^`]+)`|\"([^\"]+)\"|'([^']+)'|(\\S+))$")
	fileNote  = regexp.MustCompile(`^\[The user attached a file: .+\. It is saved at (/.*)\. Read it with your tools when the request needs its contents\.\]$`)
)

// AttachedImages are the paths of the images a user message carries, from
// the lines Hermes keeps in its text for them: "[Image attached at: <path>]"
// (agent/image_routing.py) and "@image:<path>" refs
// (tui_gateway/session_history.py), and the hub's FileNote for an image it
// copied to the chat's computer. Only MediaPath images count.
func AttachedImages(text string) []string {
	if !strings.Contains(text, "[Image attached at: ") && !strings.Contains(text, "@image:") && !strings.Contains(text, "[The user attached a file: ") {
		return nil
	}
	var out []string
	for line := range strings.SplitSeq(text, "\n") {
		line = strings.TrimSuffix(line, "\r")
		var p string
		if m := imageHint.FindStringSubmatch(line); m != nil {
			p = m[1]
		} else if m := imageRef.FindStringSubmatch(line); m != nil {
			p = m[1] + m[2] + m[3] + m[4]
		} else if m := fileNote.FindStringSubmatch(line); m != nil {
			p = m[1]
		}
		if MediaPath(p) && ImagePath(p) && !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out
}

// ImagePath reports whether p names an image the hub shows inline, by its
// extension.
func ImagePath(p string) bool {
	switch strings.ToLower(path.Ext(p)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp":
		return true
	}
	return false
}

// ScreenshotPath is the path a browser_screenshot result names, or "".
func ScreenshotPath(result map[string]any) string {
	p, _ := result["path"].(string)
	if !MediaPath(p) {
		return ""
	}
	return p
}
