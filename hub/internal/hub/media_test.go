package hub_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/USA-RedDragon/hermes-hub/internal/hub"
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
