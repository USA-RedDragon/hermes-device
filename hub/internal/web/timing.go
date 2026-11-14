package web

import (
	"bufio"
	"net"
	"net/http"
	"strconv"
	"time"
)

// serverTiming is the Server-Timing header name. Every answer carries
// "hub;dur=<ms>", the time the hub took before its first byte, so a
// browser's network panel tells the hub's part of a request from the
// network's and its own. A handler that sets the header itself (the event
// stream) keeps its own.
const serverTiming = "Server-Timing"

// timingWriter adds the hub's time to an answer's headers.
type timingWriter struct {
	http.ResponseWriter
	began time.Time
	wrote bool
}

func timed(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(&timingWriter{ResponseWriter: w, began: time.Now()}, r)
	})
}

// msDur is d in milliseconds for a Server-Timing dur.
func msDur(d time.Duration) string {
	return strconv.FormatFloat(float64(d.Microseconds())/1000, 'f', 2, 64)
}

func (t *timingWriter) stamp() {
	if t.wrote {
		return
	}
	t.wrote = true
	h := t.ResponseWriter.Header()
	if h.Get(serverTiming) == "" {
		h.Set(serverTiming, "hub;dur="+msDur(time.Since(t.began)))
	}
}

func (t *timingWriter) WriteHeader(code int) {
	t.stamp()
	t.ResponseWriter.WriteHeader(code)
}

func (t *timingWriter) Write(b []byte) (int, error) {
	t.stamp()
	return t.ResponseWriter.Write(b)
}

// Flush, Hijack and Unwrap keep what the wrapped writer can do.
func (t *timingWriter) Flush() {
	t.stamp()
	if f, ok := t.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (t *timingWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return http.NewResponseController(t.ResponseWriter).Hijack()
}

func (t *timingWriter) Unwrap() http.ResponseWriter { return t.ResponseWriter }
