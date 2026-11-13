package device_test

import (
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/USA-RedDragon/talaria/hub/internal/device"
	"github.com/USA-RedDragon/talaria/hub/internal/gateway/gatewaytest"
)

// clientMethods is every gateway method name that the Hermes v0.23.1 Ink TUI
// (ui-tui/src, and the gateway.ping heartbeat and client.capabilities of
// apps/shared), the classic
// CLI (hermes_cli) and tui_gateway/owner_forward.py mention, with the rule
// the gateway leg applies. TestClientMethodsAgainstSource rebuilds the list
// from a checkout, so a Hermes upgrade that adds or renames one fails.
func clientMethods() map[string]string {
	return map[string]string{
		"approval.respond": device.RuleAllowed, "billing.auto_reload": device.RuleDenied, "billing.charge": device.RuleDenied,
		"billing.charge_status": device.RuleDenied, "billing.keep_going_on_credits": device.RuleDenied,
		"billing.state": device.RuleDenied, "billing.step_up": device.RuleDenied, "browser.manage": device.RuleDenied,
		"clarify.lock": device.RuleAllowed, "client.capabilities": device.RuleChecked, "clarify.respond": device.RuleAllowed, "clipboard.paste": device.RuleDenied,
		"command.dispatch": device.RuleChecked, "command.observe": device.RuleAllowed, "commands.catalog": device.RuleAllowed,
		"complete.path": device.RuleRewritten, "complete.slash": device.RuleAllowed, "config.get": device.RuleChecked,
		"config.set": device.RuleChecked, "connection.respond": device.RuleDenied, "connectors.accounts": device.RuleDenied,
		"connectors.accounts.remove": device.RuleDenied, "connectors.accounts.rename": device.RuleDenied,
		"connectors.connect": device.RuleDenied, "connectors.list": device.RuleDenied,
		"connectors.operation.status": device.RuleDenied, "delegation.pause": device.RuleDenied,
		"delegation.status": device.RuleDenied, "display.observe": device.RuleDenied, "file.attach": device.RuleChecked,
		"free_tier.status": device.RuleDenied, "gateway.ping": device.RuleAllowed, "i18n.catalog": device.RuleAllowed,
		"image.attach": device.RuleDenied, "image.attach_bytes": device.RuleAllowed, "image.detach": device.RuleAllowed,
		"image.render": device.RuleChecked, "input.detect_drop": device.RuleDenied, "kanban.run": device.RuleDenied, "learning.delete": device.RuleDenied,
		"learning.detail": device.RuleAllowed, "learning.edit": device.RuleDenied, "learning.frames": device.RuleRewritten,
		"model.disconnect": device.RuleDenied, "model.options": device.RuleRewritten, "model.save_key": device.RuleDenied,
		"onboarding.start": device.RuleDenied, "onboarding.state": device.RuleDenied, "paste.collapse": device.RuleDenied,
		"pet.cells": device.RuleRewritten, "pet.gallery": device.RuleAllowed, "pet.info.meta": device.RuleAllowed,
		"pet.select": device.RuleDenied, "plugins.list": device.RuleAllowed, "plugins.manage": device.RuleDenied,
		"process.kill": device.RuleDenied, "process.list": device.RuleDenied, "process.stop": device.RuleDenied,
		"profiles.describe": device.RuleDenied, "profiles.list": device.RuleDenied, "projects.tree": device.RuleDenied,
		"prompt.background": device.RuleDenied, "prompt.btw": device.RuleAllowed, "prompt.cancel": device.RuleAllowed,
		"prompt.receipt": device.RuleAllowed, "prompt.replace_queued": device.RuleAllowed,
		"prompt.resolve_unknown": device.RuleAllowed, "prompt.submit": device.RuleAllowed, "reload.env": device.RuleDenied,
		"reload.mcp": device.RuleDenied, "rollback.diff": device.RuleDenied, "rollback.list": device.RuleDenied,
		"rollback.restore": device.RuleDenied, "rollback.session_diff": device.RuleDenied, "runtime.describe": device.RuleAllowed,
		"session.activate": device.RuleDenied, "session.active_list": device.RuleAllowed, "session.archive": device.RuleDenied,
		"session.automation": device.RuleDenied, "session.branch": device.RuleDenied, "session.close": device.RuleDenied,
		"session.compress": device.RuleDenied, "session.context_breakdown": device.RuleAllowed,
		"session.control.read": device.RuleAllowed, "session.create": device.RuleRewritten, "session.delete": device.RuleDenied,
		"session.detach": device.RuleAllowed, "session.export": device.RuleAllowed, "session.info": device.RuleAllowed,
		"session.interrupt": device.RuleAllowed, "session.list": device.RuleAllowed, "session.most_recent": device.RuleAllowed,
		"session.mutate": device.RuleRewritten, "session.prune": device.RuleDenied, "session.redirect": device.RuleAllowed,
		"session.resume": device.RuleRewritten, "session.save": device.RuleDenied, "session.stats": device.RuleAllowed,
		"session.status": device.RuleDenied, "session.steer": device.RuleAllowed, "session.title": device.RuleDenied,
		"session.undo": device.RuleDenied, "session.usage": device.RuleDenied, "setup.runtime_check": device.RuleDenied,
		"setup.status": device.RuleAllowed, "shared_metrics.slash_command": device.RuleAllowed,
		"shared_metrics.startup_latency": device.RuleAllowed, "shared_metrics.update_run": device.RuleDenied,
		"shell.exec": device.RuleDenied, "skills.manage": device.RuleDenied, "skills.reload": device.RuleDenied,
		"slash.exec": device.RuleChecked, "spawn_tree.list": device.RuleDenied, "spawn_tree.load": device.RuleDenied,
		"spawn_tree.save": device.RuleDenied, "subagent.interrupt": device.RuleAllowed, "subagent.list": device.RuleAllowed,
		"subagent.steer": device.RuleAllowed, "subagent.tail": device.RuleAllowed, "subscription.change": device.RuleDenied,
		"subscription.preview": device.RuleDenied, "subscription.resume": device.RuleDenied,
		"subscription.state": device.RuleDenied, "subscription.upgrade": device.RuleDenied, "system.battery": device.RuleAllowed,
		"terminal.resize": device.RuleAllowed, "tools.configure": device.RuleDenied, "tools.show": device.RuleAllowed,
		"voice.record": device.RuleDenied, "voice.toggle": device.RuleDenied, "wake.resume": device.RuleDenied,
		"wake.start": device.RuleDenied, "wake.status": device.RuleDenied, "wake.stop": device.RuleDenied,
	}
}

func TestClientMethodsAgainstAllowlist(t *testing.T) {
	t.Parallel()
	unknown := device.DeniedReason("no.such.method")
	for m, want := range clientMethods() {
		if got := device.GatewayRule(m); got != want {
			t.Errorf("%s: rule %s, want %s", m, got, want)
		}
		if want == device.RuleDenied && device.DeniedReason(m) == unknown {
			t.Errorf("%s is denied without a reason of its own", m)
		}
	}
	for _, m := range device.GatewayMethods() {
		if _, ok := clientMethods()[m]; !ok {
			t.Errorf("%s is allowed but no client sends it", m)
		}
	}
	for _, m := range []string{"vault.list", "session.foreign.import", "worker.register", "session.events.since", "ping", "x"} {
		if device.GatewayRule(m) != device.RuleDenied {
			t.Errorf("%s is not denied", m)
		}
	}
}

// serverMethods are the method names of the gateway's method catalogs.
func serverMethods(t *testing.T, src string) map[string]bool {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(src, "tui_gateway", "contracts", "*.py"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no method catalogs in %s: %v", src, err)
	}
	res := []*regexp.Regexp{
		regexp.MustCompile(`method\(\s*["']([a-z_][a-z0-9_]*(?:\.[a-z0-9_]+)+)["']`),
		regexp.MustCompile(`\(\s*'([a-z_]+(?:\.[a-z0-9_]+)+)',\s*(?:Canonical\w*|[A-Z]\w*Params)`),
		regexp.MustCompile(`(?m)^\s+"([a-z_]+(?:\.[a-z0-9_]+)+)",$`),
	}
	out := map[string]bool{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, re := range res {
			for _, m := range re.FindAllStringSubmatch(string(b), -1) {
				out[m[1]] = true
			}
		}
	}
	return out
}

// sentMethods are the catalog method names that appear as string literals
// in the client sources, outside tests.
func sentMethods(t *testing.T, src string, server map[string]bool) map[string]bool {
	t.Helper()
	literal := regexp.MustCompile("[\"'`]([a-z_][a-z0-9_]*(?:\\.[a-z0-9_]+)+)[\"'`]")
	out := map[string]bool{}
	scan := func(path string) {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range literal.FindAllStringSubmatch(string(b), -1) {
			if server[m[1]] {
				out[m[1]] = true
			}
		}
	}
	for root, exts := range map[string][]string{"ui-tui/src": {".ts", ".tsx"}, "hermes_cli": {".py"}} {
		err := filepath.WalkDir(filepath.Join(src, root), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !slices.Contains(exts, filepath.Ext(path)) || strings.Contains(path, "/test") ||
				strings.Contains(path, "__tests__") || strings.Contains(filepath.Base(path), ".test.") {
				return nil
			}
			scan(path)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	scan(filepath.Join(src, "tui_gateway", "owner_forward.py"))
	b, err := os.ReadFile(filepath.Join(src, "apps", "shared", "src", "json-rpc-channel.ts"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "method: 'gateway.ping'") {
		out["gateway.ping"] = true
	}
	if strings.Contains(string(b), "'client.capabilities', { server_requests: true }") {
		out["client.capabilities"] = true
	}
	return out
}

func TestClientMethodsAgainstSource(t *testing.T) {
	t.Parallel()
	src := gatewaytest.HermesSource(t, filepath.Join("tui_gateway", "contracts"))
	sent := sentMethods(t, src, serverMethods(t, src))
	want := clientMethods()
	for m := range sent {
		if _, ok := want[m]; !ok {
			t.Errorf("Hermes clients send %s, which clientMethods does not classify", m)
		}
	}
	for m := range want {
		if !sent[m] {
			t.Errorf("clientMethods lists %s, which no Hermes client sends", m)
		}
	}
	if t.Failed() {
		t.Logf("sent: %v", slices.Sorted(maps.Keys(sent)))
	}
}
