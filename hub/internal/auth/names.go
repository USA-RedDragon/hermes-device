package auth

import (
	"regexp"
	"strconv"
	"strings"
)

var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// ValidName reports whether name may name an account: lowercase letters,
// digits, dots, dashes and underscores, at most 64, starting with a letter
// or digit.
func ValidName(name string) bool { return namePattern.MatchString(name) }

// NameFor turns an identity, display name or email into an account name:
// the part of an email before @, lowercased, with other characters
// dropped. It is "user" when nothing is left.
func NameFor(candidates ...string) string {
	for _, c := range candidates {
		c, _, _ = strings.Cut(strings.ToLower(strings.TrimSpace(c)), "@")
		var b strings.Builder
		for _, r := range c {
			switch {
			case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
				b.WriteRune(r)
			case r == ' ':
				b.WriteRune('.')
			}
		}
		name := strings.TrimLeft(b.String(), "._-")
		if len(name) > 56 {
			name = name[:56]
		}
		if ValidName(name) {
			return name
		}
	}
	return "user"
}

// Numbered is name with n added, for a second account that NameFor gives
// the same name: alice, alice2, alice3.
func Numbered(name string, n int) string {
	if n < 2 {
		return name
	}
	return name + strconv.Itoa(n)
}
