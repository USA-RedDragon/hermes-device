package secretbox_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/USA-RedDragon/talaria/hub/internal/secretbox"
	"github.com/USA-RedDragon/talaria/hub/internal/store"
)

func TestSealedInTheDatabase(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	box, err := secretbox.New(st, strings.Repeat("s", 40))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := box.Get(ctx, "telegram_token"); ok || err != nil {
		t.Fatalf("unset = %v %v", ok, err)
	}
	const tok = "123456:secret-token-value-abcdefghij"
	if err := box.Put(ctx, "telegram_token", tok); err != nil {
		t.Fatal(err)
	}
	raw, _, _ := st.Setting(ctx, "secret:telegram_token")
	if strings.Contains(raw, tok) || strings.Contains(raw, "secret-token") || !strings.HasPrefix(raw, "v1:") {
		t.Fatalf("stored in the clear: %q", raw)
	}
	if v, ok, err := box.Get(ctx, "telegram_token"); !ok || err != nil || v != tok {
		t.Fatalf("get = %q %v %v", v, ok, err)
	}
	other, _ := secretbox.New(st, strings.Repeat("t", 40))
	if _, ok, err := other.Get(ctx, "telegram_token"); ok || !errors.Is(err, secretbox.ErrSealedElsewhere) {
		t.Fatalf("other key = %v %v", ok, err)
	}
	// A sealed value moved to another name does not open.
	if err := st.SetSetting(ctx, "secret:other", raw); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := box.Get(ctx, "other"); ok || err == nil {
		t.Fatal("a value opened under another name")
	}
	if err := box.Delete(ctx, "telegram_token"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := box.Get(ctx, "telegram_token"); ok {
		t.Fatal("still there after Delete")
	}
	if _, err := secretbox.New(st, "short"); err == nil {
		t.Fatal("a short secret was accepted")
	}
}

func TestSnapshot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(dir, "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SetSetting(ctx, "k", "v"); err != nil {
		t.Fatal(err)
	}
	copyPath := filepath.Join(dir, "copy.db")
	if err := st.Snapshot(ctx, copyPath); err != nil {
		t.Fatal(err)
	}
	if err := st.Snapshot(ctx, copyPath); err == nil {
		t.Fatal("overwrote an existing file")
	}
	cp, err := store.Open(ctx, copyPath)
	if err != nil {
		t.Fatal(err)
	}
	defer cp.Close()
	if v, ok, err := cp.Setting(ctx, "k"); !ok || err != nil || v != "v" {
		t.Fatalf("copy = %q %v %v", v, ok, err)
	}
}
