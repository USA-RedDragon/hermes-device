package web_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// A reply drawn block by block while it streams (hubMarkdown.render with inc)
// is the same DOM a full render of the same text gives, for random replies of
// paragraphs, lists, quotes, tables, code fences and headings cut at random
// places. testdata/mdinc/run.js drives the real page in headless Chrome.
func TestIncrementalMarkdownMatchesFullRenderChrome(t *testing.T) {
	t.Parallel()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	chrome := chromePath(t)
	e := setup(t)
	prof, err := os.MkdirTemp("", "mdinc-profile-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(prof) })
	out, err := exec.CommandContext(t.Context(), node, filepath.Join("testdata", "mdinc", "run.js"), chrome, e.url, prof).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}
