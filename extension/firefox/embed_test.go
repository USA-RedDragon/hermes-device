package firefox_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	firefox "github.com/USA-RedDragon/hermes-device/extension/firefox"
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

const hubURL = "https://hub.example.com"

func TestValidHubURL(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		hubURL:                               hubURL,
		hubURL + "/":                         hubURL,
		"\thttps://hub.example.com/hermes//": hubURL + "/hermes",
		"http://localhost:8080/":             "http://localhost:8080",
		"http://127.0.0.1:9000":              "http://127.0.0.1:9000",
		"http://[::1]:9000":                  "http://[::1]:9000",
		"http://hub.example.com":             "",
		"https://alice:pw@hub.example.com":   "",
		"https://hub.example.com/?x=1":       "",
		"https://hub.example.com/#x":         "",
		"https://hub.example.com/#":          "",
		"ftp://hub.example.com":              "",
		"hub.example.com":                    "",
		"":                                   "",
	} {
		got, err := firefox.ValidHubURL(in)
		if (err != nil) != (want == "") || got != want {
			t.Errorf("ValidHubURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestDefaults(t *testing.T) {
	t.Parallel()
	orig, err := firefox.Files.ReadFile("defaults.js")
	if err != nil {
		t.Fatal(err)
	}
	same, err := firefox.Defaults("")
	if err != nil || !bytes.Equal(same, orig) {
		t.Fatalf("Defaults(\"\") changed the file: %v", err)
	}
	got, err := firefox.Defaults("https://hub.example.com/")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(got, []byte(`const DEFAULT_HUB_URL = "https://hub.example.com";`)) {
		t.Fatalf("defaults.js = %s", got)
	}
	quoted, err := firefox.Defaults(`https://hub.example.com/";alert(1)//`)
	if err != nil || bytes.Contains(quoted, []byte(`";alert`)) {
		t.Fatalf("an address with a quote escapes the string: %s, %v", quoted, err)
	}
	if _, err := firefox.Defaults("http://hub.example.com"); !errors.Is(err, firefox.ErrHubURL) {
		t.Fatalf("Defaults(http) = %v, want ErrHubURL", err)
	}
}
