package cmd

import (
	"strings"
	"testing"
)

func TestValidToken(t *testing.T) {
	t.Parallel()
	good := "123456789:" + strings.Repeat("A", 35)
	if !validToken(good) {
		t.Fatal("a good token was refused")
	}
	for _, bad := range []string{"", "nope", "abc:" + strings.Repeat("A", 35), "123:short", good + " x", "123456789:" + strings.Repeat("A", 20) + "/x"} {
		if validToken(bad) {
			t.Fatalf("%q was accepted", bad)
		}
	}
}
