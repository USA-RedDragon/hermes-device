package host_test

import (
	"testing"

	"github.com/USA-RedDragon/hermes-device/internal/browser/host"
)

func TestInvocation(t *testing.T) {
	t.Parallel()
	const bin = "talaria"
	cases := []struct {
		args []string
		ok   bool
	}{
		{[]string{bin, "/home/alice/.mozilla/native-messaging-hosts/hermes_device.json", host.ExtensionID}, true},
		{[]string{bin, "run"}, false},
		{[]string{bin, "relative.json", host.ExtensionID}, false},
		{[]string{bin, "browser", "install-host"}, false},
	}
	for _, c := range cases {
		_, id, ok := host.Invocation(c.args)
		if ok != c.ok || (ok && id != host.ExtensionID) {
			t.Errorf("Invocation(%q) = %q, %v", c.args, id, ok)
		}
	}
}
