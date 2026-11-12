package web

import (
	"compress/gzip"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// The compressed encodings an event stream can use.
const (
	encZstd = "zstd"
	encGzip = "gzip"
)

// streamWriter writes an event stream, compressed with zstd or gzip when
// the client takes it. A chat's first event holds its whole history, a
// few megabytes of JSON for a long chat, which compress well.
type streamWriter struct {
	w       io.Writer
	enc     interface{ Flush() error }
	close   func() error
	flusher http.Flusher
}

// newStreamWriter picks the encoding from the request's Accept-Encoding,
// sets Content-Encoding on w, and returns the writer for the body.
func newStreamWriter(w http.ResponseWriter, f http.Flusher, accept string) *streamWriter {
	sw := &streamWriter{w: w, flusher: f}
	switch acceptEncoding(accept) {
	case encZstd:
		z, err := zstd.NewWriter(w, zstd.WithEncoderLevel(zstd.SpeedFastest), zstd.WithEncoderConcurrency(1),
			zstd.WithWindowSize(1<<20), zstd.WithLowerEncoderMem(true))
		if err != nil {
			return sw
		}
		w.Header().Set("Content-Encoding", encZstd)
		sw.w, sw.enc, sw.close = z, z, z.Close
	case encGzip:
		g, err := gzip.NewWriterLevel(w, gzip.BestSpeed)
		if err != nil {
			return sw
		}
		w.Header().Set("Content-Encoding", encGzip)
		sw.w, sw.enc, sw.close = g, g, g.Close
	}
	return sw
}

func (sw *streamWriter) Write(p []byte) (int, error) { return sw.w.Write(p) }

// Flush sends what was written so far to the client.
func (sw *streamWriter) Flush() {
	if sw.enc != nil {
		_ = sw.enc.Flush()
	}
	sw.flusher.Flush()
}

// Close ends the compressed stream.
func (sw *streamWriter) Close() {
	if sw.close != nil {
		_ = sw.close()
		sw.flusher.Flush()
	}
}

// acceptEncoding is "zstd" or "gzip" when accept takes it with a nonzero
// quality, zstd first, else "".
func acceptEncoding(accept string) string {
	ok := map[string]bool{}
	for part := range strings.SplitSeq(accept, ",") {
		name, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		q := 1.0
		if v, found := strings.CutPrefix(strings.TrimSpace(params), "q="); found {
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				q = f
			}
		}
		ok[strings.ToLower(strings.TrimSpace(name))] = q > 0
	}
	switch {
	case ok[encZstd]:
		return encZstd
	case ok[encGzip]:
		return encGzip
	}
	return ""
}
