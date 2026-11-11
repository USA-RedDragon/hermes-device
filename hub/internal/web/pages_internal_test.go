package web

import "testing"

func TestPageRange(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		in       string
		from, to int
		ok       bool
	}{
		{"", 0, 0, true}, {" 3 ", 3, 3, true}, {"2-5", 2, 5, true}, {"2 - 5", 2, 5, true}, {"4-", 4, 0, true},
		{"0", 0, 0, false}, {"5-2", 0, 0, false}, {"-3", 0, 0, false}, {"a", 0, 0, false}, {"1-2-3", 0, 0, false},
		{"99999999999999999999", 0, 0, false}, {"1-9999999", 0, 0, false},
	} {
		from, to, ok := pageRange(c.in)
		if from != c.from || to != c.to || ok != c.ok {
			t.Errorf("pageRange(%q) = %d %d %v", c.in, from, to, ok)
		}
	}
}
