// Package pdf reads the text and renders the pages of a PDF with the
// poppler-utils programs pdfinfo, pdftotext and pdftoppm, each in its own
// temporary directory, with a timeout and resource limits.
package pdf

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

// Limits on the work for one PDF.
const (
	// MaxPages is the most pages the hub reads text from. Later pages
	// stay in the file.
	MaxPages = 2000
	// ImageSide is the long side of a rendered page in pixels: about 120
	// dpi for a Letter or A4 page, enough to read body text.
	ImageSide = 1280
	// maxImageBytes bounds one rendered page.
	maxImageBytes = 8 << 20
	// memLimit bounds each program's address space. poppler renders
	// large images in stripes, so even a 30000 by 30000 pixel image fits,
	// but it allocates a JBIG2 page whole.
	memLimit = 192 << 20
	// cpuSeconds bounds each program's CPU time.
	cpuSeconds = 60
	// fileLimit bounds each file a program writes.
	fileLimit = 64 << 20
	// maxStderr bounds the error output kept from a program.
	maxStderr = 16 << 10
	// slots is how many PDFs the hub reads at once. One keeps the
	// programs' memory well inside a 512 MiB pod.
	slots = 1
	// staleAfter is how old a work directory left by an earlier process
	// must be before New removes it.
	staleAfter = 10 * time.Minute
)

// Errors for PDFs the hub cannot read.
var (
	// ErrUnavailable means poppler-utils is not installed.
	ErrUnavailable = errors.New("the PDF tools are not installed")
	// ErrEncrypted means the PDF needs a password or forbids copying text.
	ErrEncrypted = errors.New("the PDF is encrypted")
	// ErrDamaged means poppler could not open the PDF.
	ErrDamaged = errors.New("the PDF is damaged or not a PDF")
	// ErrTooHeavy means a program ran out of time or memory.
	ErrTooHeavy = errors.New("the PDF took too long or too much memory to read")
	// ErrBusy means the hub ran out of time while it read other PDFs.
	ErrBusy = errors.New("the hub was busy reading other PDFs")
	// ErrNoPage means the requested pages are not in the PDF.
	ErrNoPage = errors.New("the PDF has no such page")
)

// Request is what to read from one PDF.
type Request struct {
	Data []byte
	// From and To are the pages to read, 1-based and inclusive. 0 means
	// the first and the last page.
	From, To int
	// Images is the most pages to render, from From.
	Images int
	// MaxText is the most characters of text to return.
	MaxText int
}

// Doc is what the hub read from a PDF.
type Doc struct {
	// Pages is the PDF's page count.
	Pages int
	// From and To are the pages read.
	From, To int
	// Images are PNGs of pages From to From+len(Images)-1.
	Images [][]byte
	// Text is the text of pages From to To, with a "[page N]" line before
	// each page.
	Text string
	// Truncated is true when Text stops before page To ends.
	Truncated bool
	// RenderStopped is true when rendering ran out of time or memory
	// before it rendered every page asked for.
	RenderStopped bool
}

// Reader reads PDFs. The zero value is not usable; use New.
type Reader struct {
	dir     string
	timeout time.Duration
	slots   chan struct{}
	tools   map[string]string
}

// New returns a Reader that keeps its work files under dir (os.TempDir
// when empty) and gives up on a PDF after timeout (60s when zero).
func New(dir string, timeout time.Duration) *Reader {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	r := &Reader{dir: dir, timeout: timeout, slots: make(chan struct{}, slots), tools: map[string]string{}}
	r.removeStale()
	for _, t := range []string{"sh", "pdfinfo", "pdftotext", "pdftoppm"} {
		if p, err := exec.LookPath(t); err == nil {
			r.tools[t] = p
		}
	}
	return r
}

// removeStale removes work directories that a process which stopped
// before it cleaned up left behind.
func (r *Reader) removeStale() {
	dir := r.dir
	if dir == "" {
		dir = os.TempDir()
	}
	old, _ := filepath.Glob(filepath.Join(dir, "hub-pdf-*"))
	for _, p := range old {
		if fi, err := os.Lstat(p); err == nil && fi.IsDir() && time.Since(fi.ModTime()) > staleAfter {
			_ = os.RemoveAll(p)
		}
	}
}

// Available says whether poppler-utils is installed.
func (r *Reader) Available() bool { return len(r.tools) == 4 }

// IsPDF says whether a file is a PDF: it starts with "%PDF-", or its
// name ends in .pdf and it has "%PDF-" in its first 1024 bytes, where
// poppler looks for it.
func IsPDF(name string, data []byte) bool {
	head := data[:min(len(data), 1024)]
	if bytes.HasPrefix(head, []byte("%PDF-")) {
		return true
	}
	return strings.EqualFold(filepath.Ext(name), ".pdf") && bytes.Contains(head, []byte("%PDF-"))
}

// Read reads the text and renders pages of a PDF.
func (r *Reader) Read(ctx context.Context, req Request) (*Doc, error) {
	if !r.Available() {
		return nil, ErrUnavailable
	}
	select {
	case r.slots <- struct{}{}:
		defer func() { <-r.slots }()
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, ErrBusy
		}
		return nil, ctx.Err()
	}
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	dir, err := os.MkdirTemp(r.dir, "hub-pdf-")
	if err != nil {
		return nil, fmt.Errorf("failed to make a work directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	in := filepath.Join(dir, "in.pdf")
	if err := os.WriteFile(in, req.Data, 0o600); err != nil {
		return nil, fmt.Errorf("failed to write the PDF: %w", err)
	}
	pages, err := r.pageCount(ctx, dir)
	if err != nil {
		return nil, err
	}
	d := &Doc{Pages: pages, From: max(req.From, 1), To: req.To}
	if d.To == 0 || d.To > pages {
		d.To = pages
	}
	if d.From > pages || d.From > d.To {
		return nil, fmt.Errorf("%w: it has %d pages", ErrNoPage, pages)
	}
	d.To = min(d.To, d.From+MaxPages-1)
	d.Text, d.Truncated, err = r.text(ctx, dir, d.From, d.To, req.MaxText)
	if err != nil {
		return nil, err
	}
	if n := min(req.Images, d.To-d.From+1); n > 0 {
		d.Images, err = r.render(ctx, dir, d.From, d.From+n-1)
		switch {
		case errors.Is(err, ErrTooHeavy):
			d.RenderStopped = true
		case err != nil:
			return nil, err
		}
	}
	return d, nil
}

var pagesLine = regexp.MustCompile(`(?m)^Pages:\s+(\d+)\s*$`)

// pageCount reads the page count from pdfinfo. It takes the last "Pages:"
// line, since the free-text fields before it (Title, Subject...) may hold
// a line that looks like one, and keeps only the end of the output.
func (r *Reader) pageCount(ctx context.Context, dir string) (int, error) {
	out := &tail{n: 16 << 10}
	if err := r.run(ctx, dir, out, "pdfinfo", "in.pdf"); err != nil {
		return 0, err
	}
	ms := pagesLine.FindAllSubmatch(out.b, -1)
	if ms == nil {
		return 0, ErrDamaged
	}
	n, err := strconv.Atoi(string(ms[len(ms)-1][1]))
	if err != nil || n < 1 {
		return 0, ErrDamaged
	}
	return n, nil
}

// text runs pdftotext and keeps at most maxChars characters. It stops
// the program once it has enough.
func (r *Reader) text(ctx context.Context, dir string, from, to, maxChars int) (string, bool, error) {
	if maxChars <= 0 {
		return "", false, nil
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var out bytes.Buffer
	enough := &limited{w: &out, n: 4*maxChars + 4096, full: cancel}
	err := r.run(ctx, dir, enough, "pdftotext", "-layout", "-enc", "UTF-8", "-eol", "unix",
		"-f", strconv.Itoa(from), "-l", strconv.Itoa(to), "in.pdf", "-")
	if err != nil && !enough.isFull() {
		return "", false, err
	}
	text, cut := tidy(out.Bytes(), from, maxChars)
	return text, cut || enough.isFull(), nil
}

var (
	blankRun = regexp.MustCompile(`\n{3,}`)
	// marker and hubLine find text in a PDF that could pass for the
	// hub's markers and notes around it.
	marker  = regexp.MustCompile(`(?i)untrusted-pdf-text`)
	hubLine = regexp.MustCompile(`(?i)^(\s*)\[(the user attached|the pdf |the hub |page \d|image attached)`)
)

// tidy makes pdftotext's output valid UTF-8 without control characters,
// defuses text that looks like the hub's markers and notes, puts a
// "[page N]" line before each page and cuts it to maxChars characters at
// a line end. It returns "" when the PDF has no text.
func tidy(raw []byte, from, maxChars int) (string, bool) {
	s := strings.ToValidUTF8(string(raw), "�")
	if strings.TrimSpace(strings.ReplaceAll(s, "\f", "")) == "" {
		return "", false
	}
	s = marker.ReplaceAllString(s, "untrusted pdf text")
	pages := strings.Split(s, "\f")
	if len(pages) > 1 && strings.TrimSpace(pages[len(pages)-1]) == "" {
		pages = pages[:len(pages)-1]
	}
	var sb strings.Builder
	for i, p := range pages {
		fmt.Fprintf(&sb, "[page %d]\n", from+i)
		for line := range strings.SplitSeq(p, "\n") {
			line = hubLine.ReplaceAllString(strings.Map(dropControl, line), "$1($2")
			sb.WriteString(strings.TrimRightFunc(line, isSpace))
			sb.WriteByte('\n')
		}
	}
	text := strings.TrimSpace(blankRun.ReplaceAllString(sb.String(), "\n\n"))
	if utf8.RuneCountInString(text) <= maxChars {
		return text, false
	}
	cut := text[:runeOffset(text, maxChars)]
	if i := strings.LastIndexByte(cut, '\n'); i > len(cut)/2 {
		cut = cut[:i]
	}
	return cut, true
}

func dropControl(r rune) rune {
	if r == '\t' || (r >= 0x20 && r != 0x7f && (r < 0x80 || r > 0x9f)) {
		return r
	}
	if r == '\r' {
		return -1
	}
	return ' '
}

func isSpace(r rune) bool { return r == ' ' || r == '\t' }

func runeOffset(s string, n int) int {
	for i := range s {
		if n == 0 {
			return i
		}
		n--
	}
	return len(s)
}

// render runs pdftoppm for pages first to last, one page at a time, and
// returns the PNGs. When a page runs out of time or memory it returns the
// pages before it with ErrTooHeavy.
func (r *Reader) render(ctx context.Context, dir string, first, last int) ([][]byte, error) {
	out := filepath.Join(dir, "out")
	if err := os.Mkdir(out, 0o700); err != nil {
		return nil, fmt.Errorf("failed to make an output directory: %w", err)
	}
	imgs := make([][]byte, 0, last-first+1)
	for p := first; p <= last; p++ {
		err := r.run(ctx, dir, io.Discard, "pdftoppm", "-png", "-singlefile", "-scale-to", strconv.Itoa(ImageSide),
			"-f", strconv.Itoa(p), "-l", strconv.Itoa(p), "in.pdf", "out/page")
		if err != nil {
			return imgs, err
		}
		name := filepath.Join(out, "page.png")
		b, err := readLimited(name, maxImageBytes)
		if err != nil {
			return imgs, err
		}
		if err := os.Remove(name); err != nil {
			return imgs, fmt.Errorf("failed to remove a page: %w", err)
		}
		imgs = append(imgs, b)
	}
	return imgs, nil
}

func readLimited(path string, n int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open a page: %w", err)
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, n+1))
	if err != nil {
		return nil, fmt.Errorf("failed to read a page: %w", err)
	}
	if int64(len(b)) > n {
		return nil, fmt.Errorf("%w: a page image is too large", ErrDamaged)
	}
	return b, nil
}

// command is exec.CommandContext for a program found by New, with its
// arguments added after it.
func command(ctx context.Context, program string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, program)
	cmd.Args = append(cmd.Args, args...)
	return cmd
}

// limitScript sets the resource limits, then runs the program. The
// program and its arguments come as positional parameters, so no file
// name is ever parsed by the shell. Each ulimit call sets one limit,
// since dash takes only one option.
const limitScript = `{ ulimit -v %d && ulimit -t %d && ulimit -f %d && ulimit -c 0; } || exit 125; exec "$@"`

// run runs one poppler program in dir with a clean environment and
// limits, until ctx ends. Its standard output goes to stdout.
func (r *Reader) run(ctx context.Context, dir string, stdout io.Writer, tool string, args ...string) error {
	script := fmt.Sprintf(limitScript, memLimit>>10, cpuSeconds, fileLimit>>9)
	cmd := command(ctx, r.tools["sh"], append([]string{"-c", script, "sh", r.tools[tool]}, args...)...)
	cmd.Dir = dir
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + dir, "TMPDIR=" + dir, "XDG_CACHE_HOME=" + dir, "LC_ALL=C.UTF-8"}
	cmd.Stdin = nil
	cmd.Stdout = stdout
	var stderr bytes.Buffer
	cmd.Stderr = &limited{w: &stderr, n: maxStderr}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	err := cmd.Run()
	if err == nil {
		return nil
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return ErrTooHeavy
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return toolError(err, stderr.String())
}

// toolError maps a poppler exit to an error. poppler exits 1 when it
// cannot open the file (with "Incorrect password" for an encrypted one),
// 3 when the file forbids the action, and dies by a signal when a limit
// stops it.
func toolError(err error, stderr string) error {
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return fmt.Errorf("failed to run a PDF tool: %w", err)
	}
	if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return ErrTooHeavy
	}
	switch {
	case strings.Contains(stderr, "password"), ee.ExitCode() == 3:
		return ErrEncrypted
	case ee.ExitCode() == 125:
		return fmt.Errorf("failed to set limits for a PDF tool: %s", strings.TrimSpace(stderr))
	}
	return ErrDamaged
}

// limited writes at most n bytes to w and drops the rest. When it drops
// bytes it calls full once.
type limited struct {
	w       io.Writer
	n       int
	full    func()
	dropped bool
}

func (l *limited) Write(p []byte) (int, error) {
	if len(p) > l.n {
		if !l.dropped && l.full != nil {
			l.full()
		}
		l.dropped = true
		_, err := l.w.Write(p[:l.n])
		l.n = 0
		return len(p), err
	}
	l.n -= len(p)
	_, err := l.w.Write(p)
	return len(p), err
}

func (l *limited) isFull() bool { return l.dropped }

// tail keeps the last n bytes written to it.
type tail struct {
	b []byte
	n int
}

func (t *tail) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > 2*t.n {
		t.b = append(t.b[:0], t.b[len(t.b)-t.n:]...)
	}
	return len(p), nil
}
