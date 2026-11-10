package acpbridge

import (
	"encoding/json"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unicode/utf8"
)

// MaxInline is the largest file a file link is inlined from.
const MaxInline = 1 << 20

// inlinePrompt replaces each resource_link block of a session/prompt whose
// file:// URI names a regular file of at most MaxInline bytes of valid
// UTF-8 with a resource block holding the file's text.
func (b *Bridge) inlinePrompt(m object) {
	p := params(m)
	if p == nil {
		return
	}
	var blocks []json.RawMessage
	if json.Unmarshal(p["prompt"], &blocks) != nil {
		return
	}
	changed := false
	for i, raw := range blocks {
		var blk object
		if json.Unmarshal(raw, &blk) != nil || str(blk["type"]) != "resource_link" {
			continue
		}
		uri := str(blk["uri"])
		text, ok := readLinkedFile(uri)
		if !ok {
			b.log.Debug("Left a file link as is", "uri", uri)
			continue
		}
		blocks[i] = mustJSON(map[string]any{
			"type":     "resource",
			"resource": map[string]string{"uri": uri, "mimeType": "text/plain", "text": text},
		})
		changed = true
	}
	if changed {
		p["prompt"] = mustJSON(blocks)
		m["params"] = mustJSON(p)
	}
}

func readLinkedFile(uri string) (string, bool) {
	u, err := url.Parse(uri)
	if err != nil || !strings.EqualFold(u.Scheme, "file") || (u.Host != "" && u.Host != "localhost") {
		return "", false
	}
	if !filepath.IsAbs(u.Path) {
		return "", false
	}
	f, err := os.OpenFile(u.Path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", false
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > MaxInline {
		return "", false
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxInline+1))
	if err != nil || len(data) > MaxInline || !utf8.Valid(data) {
		return "", false
	}
	return string(data), true
}
