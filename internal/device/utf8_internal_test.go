package device

import "testing"

func TestCompleteUTF8(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		in   string
		want int
	}{
		"empty":             {"", 0},
		"ascii":             {"abc", 3},
		"whole":             {"caf\xc3\xa9", 5},
		"cut two":           {"caf\xc3", 3},
		"cut three":         {"a\xe2\x82", 1},
		"cut four":          {"a\xf0\x9f\x98", 1},
		"invalid":           {"a\xff", 2},
		"invalid then cut":  {"\xffa\xc3", 2},
		"lone continuation": {"a\x80\x80\x80\x80", 5},
	}
	for name, tc := range cases {
		if got := completeUTF8([]byte(tc.in)); got != tc.want {
			t.Errorf("%s: completeUTF8(%q) = %d, want %d", name, tc.in, got, tc.want)
		}
	}
}
