package web_test

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// el(tag, class, text) sets textContent, so an element passed as the text
// shows as "[object HTMLElement]". Settings > Add a skill showed that once.
func TestElTextIsNotAnElement(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob(filepath.Join("static", "*.js"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no scripts: %v", err)
	}
	bad := regexp.MustCompile(`\bel\("[a-z0-9]+", "[^"]*", el\(`)
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if m := bad.Find(src); m != nil {
			t.Errorf("%s: %s passes an element as text; append it instead", f, m)
		}
	}
}
