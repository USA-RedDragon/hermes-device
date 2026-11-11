package hub_test

import (
	"strings"
	"testing"
	"time"

	"github.com/USA-RedDragon/hermes-hub/internal/hub"
)

func TestCleanName(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"../../evil.sh": "evil.sh", `dir\x.txt`: "x.txt", ".bashrc": "bashrc", "a\nb\x00c": "a_b_c", "\u00a0.\u00a0": "attachment",
		strings.Repeat("é", 100): strings.Repeat("é", 60),
	} {
		if got := hub.CleanName(in); got != want {
			t.Errorf("CleanName(%q) = %q, want %q", in, got, want)
		}
	}
	start := time.Now()
	if got := hub.CleanName(strings.Repeat("x", 10<<20)); len(got) != 120 {
		t.Fatalf("long name = %d bytes", len(got))
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("a 10 MB name took %s", d)
	}
}

func TestImageMIME(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{"\x89PNG\r\n\x1a\nx": hub.MIMEPNG, "\xff\xd8\xff\xe0": hub.MIMEJPEG, "GIF89a": hub.MIMEGIF,
		"RIFF0000WEBPVP8L": hub.MIMEWebP, "%PDF-1.7": "", "<svg": ""} {
		if got := hub.ImageMIME([]byte(in)); got != want {
			t.Errorf("ImageMIME(%q) = %q, want %q", in, got, want)
		}
	}
}
