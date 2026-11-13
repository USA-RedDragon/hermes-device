package hub_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/USA-RedDragon/talaria/hub/internal/hub"
)

const shot = "/opt/data/cache/screenshots/hermes-device/s1/shot.jpg"

func TestMediaDirectives(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		in, text string
		paths    []string
	}{
		{"Here it is:\nMEDIA:" + shot + "\nDone.", "Here it is:\n\nDone.", []string{shot}},
		{"Here it is: MEDIA:" + shot + ".", "Here it is:", []string{shot}},
		{"**MEDIA:" + shot + "**", "", []string{shot}},
		{"`MEDIA:" + shot + "`", "", []string{shot}},
		{"MEDIA: `/opt/data/my file.pdf` ok", "ok", []string{"/opt/data/my file.pdf"}},
		{"(MEDIA:" + shot + ")", "()", []string{shot}},
		{"media:" + shot + " and MEDIA:" + shot, "and", []string{shot}},
		{"MEDIA:/a.pngMEDIA:/b.png", "", []string{"/a.png", "/b.png"}},
		{"MEDIA:" + shot + "\n\n\n\nnext", "next", []string{shot}},
	} {
		paths, text := hub.MediaDirectives(c.in)
		if text != c.text || !slices.Equal(paths, c.paths) {
			t.Errorf("MediaDirectives(%q) = %q, %q; want %q, %q", c.in, paths, text, c.paths, c.text)
		}
	}
}

func TestMediaDirectivesLeaveOthers(t *testing.T) {
	t.Parallel()
	for _, in := range []string{
		"social media: Twitter",
		"MEDIA:relative/a.png",
		"MEDIA:~/a.png",
		"MEDIA:/a/../etc/passwd.txt",
		"MEDIA:/etc/passwd",
		"MEDIA:/a.exe",
		"```\nMEDIA:" + shot + "\n```",
		"~~~md\nMEDIA:" + shot + "\n~~~",
		"use `echo MEDIA:" + shot + "` to send",
		"> MEDIA:" + shot,
		"MEDIA:/a\x01b.png",
		"MEDIA:/" + strings.Repeat("a", 5000) + ".png",
	} {
		paths, text := hub.MediaDirectives(in)
		if len(paths) != 0 || text != in {
			t.Errorf("MediaDirectives(%q) = %q, %q; want it unchanged", in, paths, text)
		}
	}
}

func TestMediaDirectivesAfterFence(t *testing.T) {
	t.Parallel()
	in := "```\nMEDIA:/x/a.png\n```\nMEDIA:" + shot
	paths, text := hub.MediaDirectives(in)
	if !slices.Equal(paths, []string{shot}) || text != "```\nMEDIA:/x/a.png\n```" {
		t.Fatalf("got %q, %q", paths, text)
	}
}

func TestMediaPath(t *testing.T) {
	t.Parallel()
	for p, want := range map[string]bool{shot: true, "/a.PNG": true, "/a/b.tar.gz": true, "a.png": false, "/a/./b.png": false,
		"//a.png": false, "/a/b/": false, "/": false, "/a.png/": false, "/a\nb.png": false, "/a.sh": false} {
		if got := hub.MediaPath(p); got != want {
			t.Errorf("MediaPath(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestAttachedImages(t *testing.T) {
	t.Parallel()
	in := "look at these\n\n[Image attached at: /opt/data/cache/images/a.png]\n[Image attached: https://example.com/b.png]\n" +
		"@image:`/opt/data/cache/images/my c.jpg`\n@image:/opt/data/cache/images/d.webp\n@image:/opt/data/cache/images/a.png\n" +
		"[Image attached at: /etc/../x.png]\n[Image attached at: /opt/data/notes.txt]\nsee [Image attached at: /opt/data/cache/images/e.png]"
	want := []string{"/opt/data/cache/images/a.png", "/opt/data/cache/images/my c.jpg", "/opt/data/cache/images/d.webp"}
	if got := hub.AttachedImages(in); !slices.Equal(got, want) {
		t.Fatalf("AttachedImages = %q, want %q", got, want)
	}
	note := hub.FileNote("image.png", "/home/alice/src/.hermes/attachments/image.png") + "\n" +
		hub.FileNote("notes.txt", "/home/alice/src/.hermes/attachments/notes.txt") + "\n" +
		hub.FileNote("up.png", "/home/alice/../x.png") + "\nsee " + hub.FileNote("a.png", "/home/alice/a.png")
	if got, want := hub.AttachedImages("hi\n\n"+note), []string{"/home/alice/src/.hermes/attachments/image.png"}; !slices.Equal(got, want) {
		t.Fatalf("file notes = %q, want %q", got, want)
	}
	if got := hub.AttachedImages("no images"); got != nil {
		t.Fatalf("no images = %q", got)
	}
}

func TestMarkdownMedia(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, in string
		paths    []string
		text     string
	}{
		{"image", "See ![a cat](/out/cat.png) here", []string{"/out/cat.png"}, "See a cat here"},
		{"link", "Get [the report](/out/report.pdf).", []string{"/out/report.pdf"}, "Get the report."},
		{"angle brackets", "![shot](</out/my shot.png> \"title\") and [f](</out/a b.pdf>)", []string{"/out/my shot.png", "/out/a b.pdf"}, "shot and f"},
		{"repeat", "![a](/o/a.png) ![b](/o/a.png)", []string{"/o/a.png"}, "a b"},
		{"relative stays", "![a](out/cat.png) [b](./x.pdf)", nil, "![a](out/cat.png) [b](./x.pdf)"},
		{"remote stays", "![a](https://example.com/a.png) [b](//example.com/b.pdf)", nil, "![a](https://example.com/a.png) [b](//example.com/b.pdf)"},
		{"unclean stays", "![a](/out/../etc/a.png)", nil, "![a](/out/../etc/a.png)"},
		{"bad extension stays", "[a](/out/run.sh)", nil, "[a](/out/run.sh)"},
		{"code stays", "`![a](/o/a.png)`\n```\n[b](/o/b.pdf)\n```", nil, "`![a](/o/a.png)`\n```\n[b](/o/b.pdf)\n```"},
		{"nested", "[![a](/o/a.png)](/o/b.pdf)", []string{"/o/a.png"}, "[a](/o/b.pdf)"},
		{"unclosed", "![a](/o/a.png", nil, "![a](/o/a.png"},
	} {
		if got := hub.MarkdownMedia(c.in); !slices.Equal(got, c.paths) {
			t.Errorf("%s: MarkdownMedia(%q) = %q, want %q", c.name, c.in, got, c.paths)
		}
		if got := hub.MarkdownMediaText(c.in); got != c.text {
			t.Errorf("%s: MarkdownMediaText(%q) = %q, want %q", c.name, c.in, got, c.text)
		}
	}
}

func TestMarkdownMediaTextDropImages(t *testing.T) {
	t.Parallel()
	in := "Look:\n\n![the plot](/a/p.png)\n\nInline ![alt](/a/q.png) here and [doc](/a/r.pdf)."
	want := "Look:\n\nInline alt here and doc."
	if got := hub.MarkdownMediaTextDropImages(in); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
