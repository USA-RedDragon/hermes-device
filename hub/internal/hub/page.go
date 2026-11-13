package hub

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"html"
	"path"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

// Pages reads the pages of a session: interactive HTML that the agent
// writes in a reply as a page block. A file page (an HTML file named with a
// MEDIA directive) is read through Media.
type Pages interface {
	// Page returns the HTML of the page block with that PageID in a reply
	// of the session. It fails with ErrNoPage when no reply has one.
	Page(ctx context.Context, user, backendID, sessionID, id string) (string, error)
}

// ErrNoPage is a page that no reply of the chat has.
var ErrNoPage = errors.New("no such page in this chat")

// MaxPageBytes bounds a page file the hub serves.
const MaxPageBytes = 16 << 20

var (
	pageOpen   = regexp.MustCompile("^ {0,3}(`{3,}|~{3,})[ \t]*(?:[Pp][Aa][Gg][Ee]|[Hh][Tt][Mm][Ll][ \t]+[Pp][Aa][Gg][Ee]|[Hh][Ee][Rr][Mm][Ee][Ss]-[Pp][Aa][Gg][Ee])[ \t]*$")
	fenceStart = regexp.MustCompile("^ {0,3}(`{3,}|~{3,})")
	pageIDRe   = regexp.MustCompile(`^[0-9a-f]{32}$`)
	titleRe    = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
)

// PageBlocks finds the page blocks of an assistant reply: fenced blocks
// whose info string is "page" (or "html page", "hermes-page"), outside other
// fenced blocks and closed by a fence of the same character and at least
// the same length. It returns their HTML in order without repeats, and text
// without them. A block with only white space, or one that is not closed,
// stays in the text. The web page follows the same rules.
func PageBlocks(text string) (pages []string, cleaned string) {
	if !strings.Contains(text, "```") && !strings.Contains(text, "~~~") {
		return nil, text
	}
	lines := strings.Split(text, "\n")
	var keep []string
	found := false
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSuffix(lines[i], "\r")
		m := fenceStart.FindStringSubmatch(line)
		if m == nil {
			keep = append(keep, lines[i])
			continue
		}
		end := fenceEnd(lines, i+1, m[1])
		if !pageOpen.MatchString(line) || end < 0 {
			if end < 0 {
				end = len(lines) - 1
			}
			keep = append(keep, lines[i:end+1]...)
			i = end
			continue
		}
		body := strings.Join(lines[i+1:end], "\n")
		if strings.TrimSpace(body) == "" {
			keep = append(keep, lines[i:end+1]...)
			i = end
			continue
		}
		found = true
		if !slices.Contains(pages, body) {
			pages = append(pages, body)
		}
		i = end
	}
	if !found {
		return nil, text
	}
	return pages, strings.TrimSpace(manyBlank.ReplaceAllString(strings.Join(keep, "\n"), "\n\n"))
}

// FenceOpen says whether text ends inside a fenced block that is not
// closed: a reply that arrives in pieces was cut inside a code or page
// block, so the next piece of the same turn goes on from it. The web page
// has the same rule (HubPageBlocks.open).
func FenceOpen(text string) bool {
	if !strings.Contains(text, "```") && !strings.Contains(text, "~~~") {
		return false
	}
	lines := strings.Split(text, "\n")
	for i := 0; i < len(lines); i++ {
		m := fenceStart.FindStringSubmatch(strings.TrimSuffix(lines[i], "\r"))
		if m == nil {
			continue
		}
		end := fenceEnd(lines, i+1, m[1])
		if end < 0 {
			return true
		}
		i = end
	}
	return false
}

// fenceEnd is the index of the line that closes a fence opened with
// marker, from line from on, or -1.
func fenceEnd(lines []string, from int, marker string) int {
	for j := from; j < len(lines); j++ {
		l := strings.TrimRight(strings.TrimSuffix(lines[j], "\r"), " \t")
		t := strings.TrimLeft(l, " ")
		if len(l)-len(t) > 3 || len(t) < len(marker) {
			continue
		}
		if strings.Trim(t, marker[:1]) == "" {
			return j
		}
	}
	return -1
}

// PageID names a page block: the first 32 hex digits of the SHA-256 of its
// HTML.
func PageID(page string) string {
	sum := sha256.Sum256([]byte(page))
	return hex.EncodeToString(sum[:16])
}

// PageIDOK reports whether id has the form of a PageID.
func PageIDOK(id string) bool { return pageIDRe.MatchString(id) }

// PagePath reports whether p is a MediaPath of an HTML file, which the hub
// shows as a page.
func PagePath(p string) bool {
	switch strings.ToLower(path.Ext(p)) {
	case ".html", ".htm":
		return MediaPath(p)
	}
	return false
}

// PageTitle is the text of the first <title> in the first 64 KiB of a
// page, with white space collapsed and at most 120 characters, or "".
func PageTitle(page string) string {
	if len(page) > 64<<10 {
		page = page[:64<<10]
	}
	m := titleRe.FindStringSubmatch(page)
	if m == nil {
		return ""
	}
	t := strings.Join(strings.Fields(html.UnescapeString(m[1])), " ")
	t = strings.ToValidUTF8(t, "")
	if utf8.RuneCountInString(t) > 120 {
		r := []rune(t)
		t = string(r[:119]) + "…"
	}
	return t
}
