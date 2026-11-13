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
