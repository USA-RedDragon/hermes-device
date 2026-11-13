package firefox_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	firefox "github.com/USA-RedDragon/talaria/extension/firefox"
)

func TestValidVersion(t *testing.T) {
	t.Parallel()
	for v, want := range map[string]bool{
		"1": true, "0.4.3": true, "1.2.3.4": true, "123456789.0": true,
		"": false, "v1.2.3": false, "1.2.3-rc.1": false, "01.2": false, "1..2": false,
		"1.2.3.4.5": false, "1234567890": false, "1.2.": false,
	} {
		if got := firefox.ValidVersion(v); got != want {
			t.Errorf("ValidVersion(%q) = %v, want %v", v, got, want)
		}
	}
}

func TestManifestVersion(t *testing.T) {
	t.Parallel()
	orig, err := firefox.Files.ReadFile("manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	same, err := firefox.Manifest("")
	if err != nil || !bytes.Equal(same, orig) {
		t.Fatalf("Manifest(\"\") changed the manifest: %v", err)
	}
	got, err := firefox.Manifest("12.3.4")
	if err != nil {
		t.Fatal(err)
	}
	var a, b map[string]any
	if err := json.Unmarshal(orig, &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(got, &b); err != nil {
		t.Fatal(err)
	}
	if b["version"] != "12.3.4" {
		t.Fatalf("version = %v", b["version"])
	}
	a["version"], b["version"] = nil, nil
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("more than the version changed:\n%v\n%v", a, b)
	}
	if _, err := firefox.Manifest("1.2.3-rc.1"); !errors.Is(err, firefox.ErrVersion) {
		t.Fatalf("Manifest(pre-release) = %v, want ErrVersion", err)
	}
}

func TestManifestUpdateURL(t *testing.T) {
	t.Parallel()
	data, err := firefox.Files.ReadFile("manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		BSS struct {
			Gecko struct {
				ID        string `json:"id"`
				UpdateURL string `json:"update_url"`
			} `json:"gecko"`
		} `json:"browser_specific_settings"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	if m.BSS.Gecko.ID != "browser@hermes-device" {
		t.Errorf("id = %q", m.BSS.Gecko.ID)
	}
	if m.BSS.Gecko.UpdateURL != firefox.UpdateURL {
		t.Errorf("update_url = %q, want %q", m.BSS.Gecko.UpdateURL, firefox.UpdateURL)
	}
}

// TestManifestPermissions keeps the extension to what browser control
// needs: it talks only to the native host, so it needs no identity or
// storage.
func TestManifestPermissions(t *testing.T) {
	t.Parallel()
	data, err := firefox.Files.ReadFile("manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Permissions []string `json:"permissions"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	want := []string{"nativeMessaging", "tabs", "tabGroups", "scripting", "webRequest", "webRequestBlocking", "dns", "<all_urls>"}
	if !reflect.DeepEqual(m.Permissions, want) {
		t.Errorf("permissions = %q, want %q", m.Permissions, want)
	}
}

func TestManifestFilesEmbedded(t *testing.T) {
	t.Parallel()
	data, err := firefox.Files.ReadFile("manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Background    struct{ Scripts []string } `json:"background"`
		BrowserAction struct {
			Popup string `json:"default_popup"`
		} `json:"browser_action"`
		Sidebar struct {
			Panel string `json:"default_panel"`
		} `json:"sidebar_action"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	if m.BrowserAction.Popup != "pagechat.html" || m.Sidebar.Panel != "pagechat.html" {
		t.Errorf("popup %q, sidebar %q, want the page chat in both", m.BrowserAction.Popup, m.Sidebar.Panel)
	}
	page, err := firefox.Files.ReadFile("pagechat.html")
	if err != nil {
		t.Fatal(err)
	}
	names := append([]string{m.BrowserAction.Popup, m.Sidebar.Panel}, m.Background.Scripts...)
	for _, ref := range regexp.MustCompile(`(?:src|href)="([^"]+)"`).FindAllSubmatch(page, -1) {
		names = append(names, string(ref[1]))
	}
	for _, n := range names {
		if _, err := firefox.Files.ReadFile(n); err != nil {
			t.Errorf("%s is not embedded: %v", n, err)
		}
	}
}

// TestManifestNameAndIcons keeps the user-visible name plain and checks that
// every icon the manifest names is embedded.
func TestManifestNameAndIcons(t *testing.T) {
	t.Parallel()
	data, err := firefox.Files.ReadFile("manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Name          string            `json:"name"`
		Description   string            `json:"description"`
		Icons         map[string]string `json:"icons"`
		BrowserAction struct {
			Title string            `json:"default_title"`
			Icon  map[string]string `json:"default_icon"`
			Theme []struct {
				Light string `json:"light"`
				Dark  string `json:"dark"`
			} `json:"theme_icons"`
		} `json:"browser_action"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	if m.Name != "Talaria" {
		t.Errorf("name = %q", m.Name)
	}
	for _, s := range []string{m.Name, m.Description, m.BrowserAction.Title} {
		if strings.Contains(strings.ToLower(s), "hermes") {
			t.Errorf("%q names Hermes", s)
		}
	}
	paths := []string{}
	for _, icons := range []map[string]string{m.Icons, m.BrowserAction.Icon} {
		if len(icons) != 4 {
			t.Errorf("icon sizes = %v, want 16, 32, 48 and 96", icons)
		}
		for _, p := range icons {
			paths = append(paths, p)
		}
	}
	for _, th := range m.BrowserAction.Theme {
		paths = append(paths, th.Light, th.Dark)
	}
	for _, p := range paths {
		if _, err := firefox.Files.ReadFile(p); err != nil {
			t.Errorf("icon %s is not embedded: %v", p, err)
		}
	}
}

func TestWriteIncludesIcons(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := firefox.Write(dir, "1.2.3"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "icons", "icon-96.png")); err != nil {
		t.Error(err)
	}
}
