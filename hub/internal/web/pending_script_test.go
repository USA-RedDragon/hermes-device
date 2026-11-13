package web_test

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// A background result or another agent's message that Hermes hands to the
// running turn shows as a notice card, never as the user's bubble, so the
// steering branch of the snapshot must ask HubOrigin like the queued one.
func TestSteeringPendingAsksOrigin(t *testing.T) {
	t.Parallel()
	b, err := os.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?s)p\.status === "steering"\) \{(.*?)this\.userText\(`).FindStringSubmatch(string(b))
	if m == nil || !strings.Contains(m[1], "HubOrigin.classify(p)") {
		t.Fatal("the steering branch of the pending list does not classify the input before it shows a bubble")
	}
}

// A chat that Hermes no longer has, opened from an old link, says so and
// offers the way back to the chats; it is not left loading for ever. Only a
// fresh list asked for after the chat opened may say it, or a chat made a
// moment ago would read as gone.
func TestGoneChatSaysSo(t *testing.T) {
	t.Parallel()
	b, err := os.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	m := regexp.MustCompile(`(?s)if \(!page && view && (.*?)\) showGone\(\);`).FindStringSubmatch(src)
	if m == nil {
		t.Fatal("refreshList never shows a chat that is gone")
	}
	for _, want := range []string{"!down.size", "!refreshing.size", "view.openedAt < asked", "!sessions.some(isCurrent)"} {
		if !strings.Contains(m[1], want) {
			t.Fatalf("the gone check lacks %q: %s", want, m[1])
		}
	}
	if !strings.Contains(src, "This chat no longer exists.") || !strings.Contains(src, `"Back to your chats"`) {
		t.Fatal("the gone view lacks its line or its link")
	}
}
