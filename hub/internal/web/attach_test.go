package web_test

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"

	"github.com/USA-RedDragon/talaria/hub/internal/gateway/gatewaytest"
	"github.com/USA-RedDragon/talaria/hub/internal/hub"
)

const (
	fieldPages = "pages"
	aPDF       = "a.pdf"
	fieldFile  = "file"
	aPNG       = "a.png"
)

type part struct {
	field, name string
	data        []byte
}

func postForm(t *testing.T, c *http.Client, u, csrf, origin string, parts ...part) (*http.Response, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for _, p := range parts {
		var fw io.Writer
		var err error
		if p.name == "" {
			fw, err = w.CreateFormField(p.field)
		} else {
			fw, err = w.CreateFormFile(p.field, p.name)
		}
		if err != nil {
			t.Fatal(err)
		}
		_, _ = fw.Write(p.data)
	}
	_ = w.Close()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, u, &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("X-CSRF-Token", csrf)
	req.Header.Set("Origin", origin)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	return resp, string(rb)
}

func TestPromptWithAttachments(t *testing.T) {
	t.Parallel()
	e := setup(t)
	c := client(t)
	m := login(t, e, c)
	u := e.url + "/api/sessions/pc/s1/prompt"
	png := []byte("\x89PNG\r\n\x1a\nxx")
	resp, body := postForm(t, c, u, m.CSRF, e.url, part{field: kText, data: []byte("look")},
		part{field: fieldFile, name: "shot.png", data: png}, part{field: fieldFile, name: "../notes.txt", data: []byte("n")})
	if resp.StatusCode != http.StatusAccepted || !strings.Contains(body, `"status":"sent"`) {
		t.Fatalf("prompt = %d %s", resp.StatusCode, body)
	}
	s, _ := e.gw.Session("s1")
	if len(s.Attachments) != 1 || len(s.Attachments[0]) != 1 || s.Attachments[0][0].MIME != mimePNG {
		t.Fatalf("attachments = %+v", s.Attachments)
	}
	if text, _ := s.Messages[0]["text"].(string); text != "look\n\n"+hub.FileNote("notes.txt", gatewaytest.StagedFiles+"notes.txt") {
		t.Fatalf("text = %q", text)
	}

	resp, body = postForm(t, c, u, m.CSRF, e.url, part{field: fieldFile, name: aPNG, data: png})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("image only = %d %s", resp.StatusCode, body)
	}

	resp, body = postForm(t, c, u, m.CSRF, e.url, part{field: fieldFile, name: aPDF, data: []byte("%PDF-1.4")},
		part{field: fieldPages, data: []byte("2-3")})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("pages = %d %s", resp.StatusCode, body)
	}
	for _, parts := range [][]part{
		{{field: fieldPages, data: []byte("1")}, {field: fieldFile, name: aPDF, data: []byte("%PDF-1.4")}},
		{{field: fieldFile, name: aPDF, data: []byte("%PDF-1.4")}, {field: fieldPages, data: []byte("3-1")}},
	} {
		resp, body = postForm(t, c, u, m.CSRF, e.url, parts...)
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "Pick pages") {
			t.Fatalf("bad pages = %d %s", resp.StatusCode, body)
		}
	}
	for _, tc := range []struct {
		parts  []part
		status int
		want   string
	}{
		{[]part{{field: kText, data: []byte("/new")}, {field: fieldFile, name: aPNG, data: png}}, http.StatusBadRequest, "slash command"},
		{[]part{{field: fieldFile, name: "e.txt"}}, http.StatusBadRequest, "The file e.txt is empty"},
		{[]part{{field: "extra", data: []byte("x")}}, http.StatusBadRequest, "unknown form field"},
		{[]part{{field: fieldFile, name: "big.bin", data: make([]byte, hub.MaxFileBytes+1)}}, http.StatusRequestEntityTooLarge, "larger than 32 MB"},
		{nil, http.StatusBadRequest, "empty prompt"},
	} {
		resp, body := postForm(t, c, u, m.CSRF, e.url, tc.parts...)
		if resp.StatusCode != tc.status || !strings.Contains(body, tc.want) {
			t.Errorf("%v = %d %s, want %d %q", tc.parts, resp.StatusCode, body, tc.status, tc.want)
		}
	}
	if resp, _ := postForm(t, c, u, "", e.url, part{field: fieldFile, name: aPNG, data: png}); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("form without a CSRF token = %d", resp.StatusCode)
	}
}

func TestCSPAllowsBlobImages(t *testing.T) {
	t.Parallel()
	e := setup(t)
	resp, _ := get(t, client(t), e.url+"/")
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "img-src 'self' data: blob:;") {
		t.Fatalf("CSP = %q", csp)
	}
}

func TestFormPromptSteersWithFiles(t *testing.T) {
	t.Parallel()
	e := setup(t)
	c := client(t)
	m := login(t, e, c)
	e.gw.QueueWhileRunning = true
	e.gw.StartElsewhere("s1", "tui-1", "work", false, true)
	u := e.url + "/api/sessions/pc/s1/prompt"
	resp, body := postForm(t, c, u, m.CSRF, e.url, part{field: kText, data: []byte("see this")}, part{field: "steer", data: []byte("true")},
		part{field: fieldFile, name: "shot.png", data: []byte("\x89PNG\r\n\x1a\nxx")})
	if resp.StatusCode != http.StatusAccepted || !strings.Contains(body, `"steered":true`) {
		t.Fatalf("steer with a file = %d %s", resp.StatusCode, body)
	}
	if got := e.gw.Steered(); len(got) != 1 {
		t.Fatalf("steered = %v", got)
	}
	resp, body = postForm(t, c, u, m.CSRF, e.url, part{field: kText, data: []byte("later")},
		part{field: fieldFile, name: "n.txt", data: []byte("n")})
	if resp.StatusCode != http.StatusAccepted || !strings.Contains(body, `"steered":false`) {
		t.Fatalf("a form without steer = %d %s", resp.StatusCode, body)
	}
}
