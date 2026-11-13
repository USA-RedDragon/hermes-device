package web_test

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// authrules.js holds the rules of the login, setup and Users pages;
// testdata/authrules/run.js checks the password and name rules, the
// meter, the next address, the guards on admins and the lines of the
// people list.
func TestAuthRulesScript(t *testing.T) {
	t.Parallel()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	out, err := exec.CommandContext(t.Context(), node, filepath.Join("testdata", "authrules", "run.js"), filepath.Join("static", "authrules.js")).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}
