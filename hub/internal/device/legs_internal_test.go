package device

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncateUTF8(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in   string
		n    int
		want string
	}{
		{"short", 120, "short"},
		{strings.Repeat("a", 119) + "é", 120, strings.Repeat("a", 119)},
		{strings.Repeat("a", 118) + "€x", 120, strings.Repeat("a", 118)},
		{strings.Repeat("a", 117) + "€x", 120, strings.Repeat("a", 117) + "€"},
		{"€€", 4, "€"},
	} {
		got := truncateUTF8(tc.in, tc.n)
		if got != tc.want || !utf8.ValidString(got) {
			t.Errorf("truncateUTF8(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
		}
	}
}
