package device

import (
	"encoding/json"
	"testing"
)

const (
	testKey   = "key"
	testScope = "scope"
	testValue = "value"
)

func TestConfigSetRule(t *testing.T) {
	t.Parallel()
	for _, key := range SessionConfigKeys() {
		if !configSetAllowed(map[string]json.RawMessage{testKey: mustRaw(key), testValue: mustRaw("high")}) {
			t.Errorf("session key %q refused", key)
		}
	}
	for _, p := range []map[string]any{
		{testKey: "skin", testValue: "x"}, {testKey: "theme", testValue: "dark"}, {testKey: "details_mode", testValue: "hidden"},
		{testKey: "focus", testValue: "on"}, {testKey: "personality", testValue: "x"}, {testKey: "approvals.mode", testValue: "off"},
		{testKey: "daybreak", testValue: "on"}, {testKey: "display.in_app_tips", testValue: "off"}, {testKey: "voice.voice_chat_mode", testValue: "chained"},
		{testKey: "model", testValue: "x"}, {testKey: "reasoning", testValue: "high", testScope: "global"},
		{testKey: "reasoning", testValue: " Hide "}, {testKey: "yolo", testValue: "1", testScope: "session"},
		{testKey: "verbose", testValue: "all", testScope: "global"}, {testKey: 7}, {},
	} {
		params := map[string]json.RawMessage{}
		for k, v := range p {
			params[k] = mustRaw(v)
		}
		if configSetAllowed(params) {
			t.Errorf("profile-wide config.set %v allowed", p)
		}
	}
	if !configSetAllowed(map[string]json.RawMessage{testKey: mustRaw("reasoning"), testValue: mustRaw("reset"), testScope: mustRaw("session")}) {
		t.Error("session reasoning reset refused")
	}
}

func TestFrameKeepsLastDuplicate(t *testing.T) {
	t.Parallel()
	f, err := parseFrame([]byte(`{"id":1,"method":"config.set","params":{"key":"verbose","key":"skin"},"method":"config.set"}`))
	if err != nil {
		t.Fatal(err)
	}
	params, _ := decodeParams(f.Params)
	if !denied(f.Method, params) {
		t.Fatal("duplicate key read as the first, unlike Python")
	}
	out, _ := f.encode(params)
	var back map[string]map[string]string
	_ = json.Unmarshal(out, &back)
	if back["params"][testKey] != "skin" {
		t.Fatalf("re-encoded = %s", out)
	}
}
