package web

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// assetRef is a src or href that names a static file.
var assetRef = regexp.MustCompile(`((?:src|href)=")/static/([^"?#]+)"`)

// assets versions the static files by content: a page names a file as
// /static/app.js?v=<hash of its bytes>, which the browser keeps for a year
// (a new release has a new hash and so a new URL), and the same file with no
// ?v= (or an old one) is revalidated by its ETag.
type assets struct {
	fsys   fs.FS
	hashes map[string]string
}

func newAssets(fsys fs.FS) *assets {
	a := &assets{fsys: fsys, hashes: map[string]string{}}
	_ = fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if b, err := fs.ReadFile(fsys, p); err == nil {
				sum := sha256.Sum256(b)
				a.hashes[p] = hex.EncodeToString(sum[:8])
			}
		}
		return err
	})
	return a
}

// page is an HTML file of the static folder with its static files named by
// version, and its ETag.
func (a *assets) page(file string) ([]byte, string, error) {
	b, err := fs.ReadFile(a.fsys, file)
	if err != nil {
		return nil, "", err
	}
	b = assetRef.ReplaceAllFunc(b, func(m []byte) []byte {
		sub := assetRef.FindSubmatch(m)
		h, ok := a.hashes[string(sub[2])]
		if !ok {
			return m
		}
		return []byte(string(sub[1]) + "/static/" + string(sub[2]) + "?v=" + h + `"`)
	})
	sum := sha256.Sum256(b)
	return b, `"` + hex.EncodeToString(sum[:8]) + `"`, nil
}

// servePage writes page (see page) with the given Cache-Control; a browser
// that has the same version gets a 304.
func servePage(w http.ResponseWriter, r *http.Request, b []byte, etag, cache string) {
	h := w.Header()
	h.Set("Cache-Control", cache)
	h.Set("ETag", etag)
	h.Set("Content-Type", "text/html; charset=utf-8")
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(b))
}

// handler serves the static files.
func (a *assets) handler() http.Handler {
	files := http.StripPrefix("/static/", http.FileServerFS(a.fsys))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/static/")
		h, ok := a.hashes[name]
		if !ok {
			files.ServeHTTP(w, r)
			return
		}
		etag := `"` + h + `"`
		w.Header().Set("ETag", etag)
		if r.URL.Query().Get("v") == h {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		files.ServeHTTP(w, r)
	})
}
