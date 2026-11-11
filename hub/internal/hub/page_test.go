package hub_test

import (
	"slices"
	"testing"

	"github.com/USA-RedDragon/hermes-hub/internal/hub"
)

func TestPageBlocks(t *testing.T) {
	t.Parallel()
	const page = "<!doctype html>\n<title>Pick</title>\n<p>hi</p>"
	for _, c := range []struct {
		name, in, text string
		pages          []string
	}{
		{"plain", "Here:\n```page\n" + page + "\n```\nDone.", "Here:\nDone.", []string{page}},
		{"aliases", "~~~ HTML Page\n" + page + "\n~~~\n```hermes-page\nb\n```", "", []string{page, "b"}},
		{"longer close", "````page\n```\ninner\n```\n`````", "", []string{"```\ninner\n```"}},
		{"indented", "   ```page  \n" + page + "\n   ```", "", []string{page}},
		{"repeat", "```page\nx\n```\n```page\nx\n```", "", []string{"x"}},
		{"crlf", "```page\r\na\r\nb\r\n```\r\nend", "end", []string{"a\r\nb\r"}},
		{"html code", "```html\n" + page + "\n```", "```html\n" + page + "\n```", nil},
		{"in other fence", "````md\n```page\nx\n```\n````", "````md\n```page\nx\n```\n````", nil},
		{"unclosed", "```page\n" + page, "```page\n" + page, nil},
		{"wrong closer", "```page\nx\n~~~", "```page\nx\n~~~", nil},
		{"empty", "```page\n  \n```", "```page\n  \n```", nil},
		{"quoted", "> ```page\n> x\n> ```", "> ```page\n> x\n> ```", nil},
		{"four spaces", "    ```page\nx\n```", "    ```page\nx\n```", nil},
		{"non-ASCII fold", "```hermeſ-page\nx\n```", "```hermeſ-page\nx\n```", nil},
		{"blank runs", "a\n\n```page\nx\n```\n\nb", "a\n\nb", []string{"x"}},
	} {
		pages, text := hub.PageBlocks(c.in)
		if text != c.text || !slices.Equal(pages, c.pages) {
			t.Errorf("%s: PageBlocks = %q %q, want %q %q", c.name, pages, text, c.pages, c.text)
		}
	}
}

func TestPageID(t *testing.T) {
	t.Parallel()
	id := hub.PageID("<p>hi</p>")
	if id != "0a4735281db700223af63abc387c351f" || !hub.PageIDOK(id) {
		t.Fatalf("PageID = %q", id)
	}
	if hub.PageID("<p>hi</p>") != id || hub.PageID("<p>ho</p>") == id {
		t.Fatal("PageID is not a function of the page")
	}
	for _, bad := range []string{"", id[:31], id + "0", "ABCDEF0123456789ABCDEF0123456789", "../../../../etc/passwd0000000000"} {
		if hub.PageIDOK(bad) {
			t.Errorf("PageIDOK(%q)", bad)
		}
	}
}

func TestPageTitleAndPath(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"<html><head><TITLE lang=en>  Pick &amp; send\n entries </TITLE>": "Pick & send entries",
		"<title></title>":                 "",
		"no title":                        "",
		"<title>&#8364; &#x20AC;</title>": "€ €",
	} {
		if got := hub.PageTitle(in); got != want {
			t.Errorf("PageTitle(%q) = %q, want %q", in, got, want)
		}
	}
	long := "<title>" + string(slices.Repeat([]byte("a"), 300)) + "</title>"
	if got := []rune(hub.PageTitle(long)); len(got) != 120 || got[119] != '…' {
		t.Errorf("long title = %q", string(got))
	}
	for p, want := range map[string]bool{"/w/pages/a.html": true, "/w/a.HTM": true, "/w/a.png": false, "w/a.html": false, "/w/../a.html": false} {
		if hub.PagePath(p) != want {
			t.Errorf("PagePath(%q) != %v", p, want)
		}
	}
}
