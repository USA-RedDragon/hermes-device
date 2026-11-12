package web

import (
	"bufio"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func TestAcceptEncoding(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"":                           "",
		"gzip, deflate, br, zstd":    encZstd,
		"gzip, deflate, br":          encGzip,
		"zstd;q=0, gzip;q=0.5":       encGzip,
		"ZSTD":                       encZstd,
		"identity":                   "",
		"gzip;q=0":                   "",
		"br;q=1.0, zstd ; q=0.8":     encZstd,
		"deflate, gzip;q=bad, *;q=0": encGzip,
	} {
		if got := acceptEncoding(in); got != want {
			t.Errorf("acceptEncoding(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestStreamWriterFlushes checks that each flushed event reaches the client
// whole while the stream stays open, for every encoding.
func TestStreamWriterFlushes(t *testing.T) {
	t.Parallel()
	for _, enc := range []string{encZstd, encGzip, ""} {
		next := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			f, _ := w.(http.Flusher)
			sw := newStreamWriter(w, f, r.Header.Get("Accept-Encoding"))
			defer sw.Close()
			for _, ev := range []string{"one", strings.Repeat("abcd", 50000), "three"} {
				_, _ = io.WriteString(sw, "data: "+ev+"\n\n")
				sw.Flush()
				<-next
			}
		}))
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
		if enc != "" {
			req.Header.Set("Accept-Encoding", enc)
		}
		resp, err := (&http.Transport{DisableCompression: true}).RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		if got := resp.Header.Get("Content-Encoding"); got != enc {
			t.Fatalf("%s: Content-Encoding %q", enc, got)
		}
		var body io.Reader = resp.Body
		switch enc {
		case encZstd:
			d, err := zstd.NewReader(resp.Body, zstd.WithDecoderConcurrency(1))
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			body = d
		case encGzip:
			g, err := gzip.NewReader(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			body = g
		}
		br := bufio.NewReader(body)
		for i, want := range []int{len("data: one"), len("data: ") + 200000, len("data: three")} {
			line, err := br.ReadString('\n')
			if err != nil || len(strings.TrimSpace(line)) != want {
				t.Fatalf("%s: event %d: %d bytes, %v", enc, i, len(line), err)
			}
			_, _ = br.ReadString('\n')
			next <- struct{}{}
		}
		_ = resp.Body.Close()
		srv.Close()
	}
}
