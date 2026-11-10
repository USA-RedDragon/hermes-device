package telegram

import (
	"html"
	"regexp"
	"strings"
	"unicode/utf16"
)

// MaxMessageLen is Telegram's message text limit in UTF-16 code units. The
// limit applies to the text after entity parsing; measuring the HTML source
// against it is stricter and always safe.
const MaxMessageLen = 4096

var (
	boldRe = regexp.MustCompile(`\*\*([^*\n]+?)\*\*`)
	linkRe = regexp.MustCompile(`\[([^\]\n]+)\]\((https?://[^\s)]+)\)`)
)

func escapeHTML(s string) string {
	return html.EscapeString(s)
}

func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		n += utf16.RuneLen(r)
	}
	return n
}

// truncate cuts s to at most n runes, adding an ellipsis when it cuts.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:max(n-1, 0)]) + "…"
}

// tail keeps the last n runes of s.
func tail(s string, n int) (string, bool) {
	r := []rune(s)
	if len(r) <= n {
		return s, false
	}
	return string(r[len(r)-n:]), true
}

// RenderMarkdown converts the Markdown subset Hermes writes (fenced code,
// inline code, bold, links, headings) to Telegram HTML and splits it into
// messages of at most limit UTF-16 code units. Every chunk is valid HTML on
// its own.
func RenderMarkdown(md string, limit int) []string {
	return pack(markdownPieces(md, limit), limit)
}

// SplitPlain escapes text and splits it into chunks of at most limit.
func SplitPlain(text string, limit int) []string {
	var pieces []string
	for line := range strings.SplitSeq(text, "\n") {
		pieces = append(pieces, hardSplit(line, limit)...)
	}
	return pack(pieces, limit)
}

func markdownPieces(md string, limit int) []string {
	lines := strings.Split(strings.ReplaceAll(md, "\r\n", "\n"), "\n")
	var pieces []string
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		if fence, ok := strings.CutPrefix(trimmed, "```"); ok {
			var code []string
			for i++; i < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[i]), "```"); i++ {
				code = append(code, lines[i])
			}
			pieces = append(pieces, codePieces(code, strings.TrimSpace(fence), limit)...)
			continue
		}
		pieces = append(pieces, prosePiece(line, limit)...)
	}
	return pieces
}

func prosePiece(line string, limit int) []string {
	text := line
	heading := false
	if t := strings.TrimLeft(line, "#"); t != line && strings.HasPrefix(t, " ") {
		text, heading = strings.TrimSpace(t), true
	}
	out := inline(text)
	if heading {
		out = "<b>" + out + "</b>"
	}
	if utf16Len(out) <= limit {
		return []string{out}
	}
	return hardSplit(text, limit)
}

func inline(s string) string {
	parts := strings.Split(s, "`")
	if len(parts)%2 == 0 {
		last := len(parts) - 1
		parts[last-1] += "`" + parts[last]
		parts = parts[:last]
	}
	var b strings.Builder
	for i, p := range parts {
		if i%2 == 1 {
			b.WriteString("<code>" + escapeHTML(p) + "</code>")
			continue
		}
		e := escapeHTML(p)
		e = boldRe.ReplaceAllString(e, "<b>$1</b>")
		e = linkRe.ReplaceAllString(e, `<a href="$2">$1</a>`)
		b.WriteString(e)
	}
	return b.String()
}

func codePieces(lines []string, lang string, limit int) []string {
	open := "<pre><code>"
	if lang != "" && !strings.ContainsAny(lang, " \t\"<>&") {
		open = `<pre><code class="language-` + lang + `">`
	}
	const closeTag = "</code></pre>"
	room := limit - utf16Len(open) - utf16Len(closeTag)
	var pieces, cur []string
	curLen := 0
	flush := func() {
		if len(cur) > 0 {
			pieces = append(pieces, open+strings.Join(cur, "\n")+closeTag)
			cur, curLen = nil, 0
		}
	}
	for _, line := range lines {
		for _, part := range hardSplit(line, room) {
			n := utf16Len(part)
			if len(cur) > 0 && curLen+1+n > room {
				flush()
			}
			if len(cur) > 0 {
				curLen++
			}
			cur = append(cur, part)
			curLen += n
		}
	}
	if len(cur) == 0 {
		cur = []string{""}
	}
	flush()
	return pieces
}

// hardSplit escapes s and cuts it into pieces whose escaped form fits in
// room.
func hardSplit(s string, room int) []string {
	if e := escapeHTML(s); utf16Len(e) <= room {
		return []string{e}
	}
	var pieces []string
	var b strings.Builder
	n := 0
	for _, r := range s {
		e := escapeHTML(string(r))
		w := utf16Len(e)
		if n+w > room && n > 0 {
			pieces = append(pieces, b.String())
			b.Reset()
			n = 0
		}
		b.WriteString(e)
		n += w
	}
	pieces = append(pieces, b.String())
	return pieces
}

// pack joins pieces with newlines into as few chunks as fit in limit.
func pack(pieces []string, limit int) []string {
	var chunks []string
	cur := make([]string, 0, len(pieces))
	curLen := 0
	flush := func() {
		text := strings.Trim(strings.Join(cur, "\n"), "\n")
		if strings.TrimSpace(text) != "" {
			chunks = append(chunks, text)
		}
		cur, curLen = cur[:0], 0
	}
	for _, p := range pieces {
		n := utf16Len(p)
		if len(cur) > 0 && curLen+1+n > limit {
			flush()
		}
		if len(cur) > 0 {
			curLen++
		}
		cur = append(cur, p)
		curLen += n
	}
	flush()
	return chunks
}
