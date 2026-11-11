package pdf_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/USA-RedDragon/hermes-hub/internal/pdf"
	"github.com/USA-RedDragon/hermes-hub/internal/pdf/pdftest"
)

const aPDF = "a.pdf"

func reader(t *testing.T, timeout time.Duration) (*pdf.Reader, string) {
	t.Helper()
	dir := t.TempDir()
	r := pdf.New(dir, timeout)
	if !r.Available() {
		t.Skip("poppler-utils is not installed")
	}
	return r, dir
}

func empty(t *testing.T, dir string) {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil || len(ents) != 0 {
		t.Fatalf("work directory has %d entries, %v", len(ents), err)
	}
}

func pages(n int) []string {
	p := make([]string, n)
	for i := range p {
		p[i] = fmt.Sprintf("Hello from page %d", i+1)
	}
	return p
}

func TestIsPDF(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, data string
		want       bool
	}{
		{"x", "%PDF-1.7\n", true}, {"a.PDF", "junk\n%PDF-1.4", true}, {"notes.txt", "see %PDF-1.4", false},
		{aPDF, strings.Repeat("x", 1100) + "%PDF-", false}, {aPDF, "PDF", false}, {aPDF, "", false},
	} {
		if got := pdf.IsPDF(c.name, []byte(c.data)); got != c.want {
			t.Errorf("IsPDF(%q, %.20q) = %v", c.name, c.data, got)
		}
	}
}

func TestReadTextAndPages(t *testing.T) {
	t.Parallel()
	r, dir := reader(t, 0)
	d, err := r.Read(context.Background(), pdf.Request{Data: pdftest.Make(pages(3)...), Images: 2, MaxText: 10000})
	if err != nil {
		t.Fatal(err)
	}
	if d.Pages != 3 || d.From != 1 || d.To != 3 || d.Truncated || len(d.Images) != 2 {
		t.Fatalf("Read = %+v", d)
	}
	for i := 1; i <= 3; i++ {
		if !strings.Contains(d.Text, fmt.Sprintf("[page %d]\nHello from page %d", i, i)) {
			t.Fatalf("text = %q", d.Text)
		}
	}
	for _, img := range d.Images {
		if !bytes.HasPrefix(img, []byte("\x89PNG\r\n\x1a\n")) {
			t.Fatal("page is not a PNG")
		}
	}
	empty(t, dir)
}

func TestReadRange(t *testing.T) {
	t.Parallel()
	r, _ := reader(t, 0)
	d, err := r.Read(context.Background(), pdf.Request{Data: pdftest.Make(pages(5)...), From: 2, To: 3, Images: 10, MaxText: 10000})
	if err != nil {
		t.Fatal(err)
	}
	if d.Pages != 5 || d.From != 2 || d.To != 3 || len(d.Images) != 2 ||
		!strings.HasPrefix(d.Text, "[page 2]\nHello from page 2") || strings.Contains(d.Text, "page 4") {
		t.Fatalf("Read = %+v %q", d, d.Text)
	}
	_, err = r.Read(context.Background(), pdf.Request{Data: pdftest.Make(pages(2)...), From: 4})
	if !errors.Is(err, pdf.ErrNoPage) {
		t.Fatalf("err = %v", err)
	}
}

func TestReadCutsText(t *testing.T) {
	t.Parallel()
	r, _ := reader(t, 0)
	long := strings.Repeat("All work and no play makes a dull report\n", 40)
	d, err := r.Read(context.Background(), pdf.Request{Data: pdftest.Make(long, long, long), MaxText: 500})
	if err != nil {
		t.Fatal(err)
	}
	if !d.Truncated || len([]rune(d.Text)) > 500 || len(d.Images) != 0 || strings.HasSuffix(d.Text, "dul") {
		t.Fatalf("Read = %v %d %q", d.Truncated, len(d.Text), d.Text)
	}
}

func TestReadDamaged(t *testing.T) {
	t.Parallel()
	r, dir := reader(t, 0)
	_, err := r.Read(context.Background(), pdf.Request{Data: []byte("%PDF-1.4\nnot really\n"), MaxText: 100})
	if !errors.Is(err, pdf.ErrDamaged) {
		t.Fatalf("err = %v", err)
	}
	empty(t, dir)
}

func TestReadEncrypted(t *testing.T) {
	t.Parallel()
	r, dir := reader(t, 0)
	qpdf, err := exec.LookPath("qpdf")
	if err != nil {
		t.Skip("qpdf is not installed")
	}
	work := t.TempDir()
	plain, locked := filepath.Join(work, "plain.pdf"), filepath.Join(work, "locked.pdf")
	if err := os.WriteFile(plain, pdftest.Make("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(qpdf, "--encrypt", "user-pw", "owner-pw", "256", "--", plain, locked).CombinedOutput() //nolint:gosec,noctx // test
	if err != nil {
		t.Fatalf("qpdf: %v %s", err, out)
	}
	data, err := os.ReadFile(locked) //nolint:gosec // test file
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Read(context.Background(), pdf.Request{Data: data, MaxText: 100}); !errors.Is(err, pdf.ErrEncrypted) {
		t.Fatalf("err = %v", err)
	}
	empty(t, dir)
}

func TestReadTimeout(t *testing.T) {
	t.Parallel()
	r, dir := reader(t, time.Nanosecond)
	_, err := r.Read(context.Background(), pdf.Request{Data: pdftest.Make("x"), MaxText: 100})
	if !errors.Is(err, pdf.ErrTooHeavy) {
		t.Fatalf("err = %v", err)
	}
	empty(t, dir)
}

func TestReadCancelled(t *testing.T) {
	t.Parallel()
	r, _ := reader(t, 0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.Read(ctx, pdf.Request{Data: pdftest.Make("x"), MaxText: 100}); err == nil {
		t.Fatal("no error")
	}
}

func TestUnavailable(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	r := pdf.New(t.TempDir(), 0)
	if _, err := r.Read(context.Background(), pdf.Request{Data: pdftest.Make("x")}); !errors.Is(err, pdf.ErrUnavailable) {
		t.Fatalf("err = %v", err)
	}
}

// fakeTools puts scripts named after the poppler programs on PATH.
func fakeTools(t *testing.T, info, text string) {
	t.Helper()
	bin := t.TempDir()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	for name, body := range map[string]string{"pdfinfo": info, "pdftotext": text, "pdftoppm": "exit 0"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!"+sh+"\n"+body+"\n"), 0o700); err != nil { //nolint:gosec // test script
			t.Fatal(err)
		}
	}
	if err := os.Symlink(sh, filepath.Join(bin, "sh")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
}

func TestToolLimits(t *testing.T) { //nolint:paralleltest // sets PATH
	fakeTools(t, `echo "Pages: $(ulimit -v)"`, `printf 'cpu %s files %s env %s\f' "$(ulimit -t)" "$(ulimit -c)" "$(env | sort | tr '\n' ' ')"`)
	r := pdf.New(t.TempDir(), 0)
	d, err := r.Read(context.Background(), pdf.Request{Data: []byte("%PDF-1.4"), MaxText: 10000})
	if err != nil {
		t.Fatal(err)
	}
	if d.Pages != 256<<10 || !strings.Contains(d.Text, "cpu 60 files 0") {
		t.Fatalf("Read = %+v", d)
	}
	if strings.Contains(d.Text, "GOPATH") || strings.Contains(d.Text, "TERM=") {
		t.Fatalf("environment leaked: %q", d.Text)
	}
}

func TestToolKilledOnTimeout(t *testing.T) { //nolint:paralleltest // sets PATH
	fakeTools(t, `echo "Pages: 1"`, `sleep 30 & sleep 30`)
	r := pdf.New(t.TempDir(), 300*time.Millisecond)
	start := time.Now()
	_, err := r.Read(context.Background(), pdf.Request{Data: []byte("%PDF-1.4"), MaxText: 100})
	if !errors.Is(err, pdf.ErrTooHeavy) || time.Since(start) > 10*time.Second {
		t.Fatalf("err = %v after %v", err, time.Since(start))
	}
}

func TestToolStoppedWhenTextIsEnough(t *testing.T) { //nolint:paralleltest // sets PATH
	fakeTools(t, `echo "Pages: 1"`, `yes "a line of text"`)
	r := pdf.New(t.TempDir(), 20*time.Second)
	d, err := r.Read(context.Background(), pdf.Request{Data: []byte("%PDF-1.4"), MaxText: 1000})
	if err != nil || !d.Truncated || len(d.Text) > 1000 {
		t.Fatalf("Read = %+v, %v", d, err)
	}
}
