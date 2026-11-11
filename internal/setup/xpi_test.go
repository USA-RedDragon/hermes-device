package setup_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/USA-RedDragon/hermes-device/internal/setup"
)

func TestXPIURLFollowsCarriedUpdates(t *testing.T) {
	t.Parallel()
	var base string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/releases/download/v1.2.0/talaria-firefox-1.2.0.xpi":
		case "/releases/download/v1.3.0/updates.json":
			_, _ = w.Write([]byte(`{"addons":{"browser@example.com":{"updates":[{"version":"1.2.0","update_link":"` + base + `/download/v1.2.0/talaria-firefox-1.2.0.xpi"}]}}}`))
		case "/releases/download/v1.4.0/updates.json":
			_, _ = w.Write([]byte(`{"addons":{"browser@example.com":{"updates":[{"version":"1.2.0","update_link":"https://elsewhere.example.com/x.xpi"}]}}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	base = srv.URL + "/releases"
	rel := &setup.Releases{BaseURL: base, Client: srv.Client()}
	ctx := context.Background()
	for tag, want := range map[string]string{
		"v1.2.0": base + "/download/v1.2.0/talaria-firefox-1.2.0.xpi",
		"v1.3.0": base + "/download/v1.2.0/talaria-firefox-1.2.0.xpi",
	} {
		if got, err := rel.XPIURL(ctx, tag); err != nil || got != want {
			t.Errorf("XPIURL(%s) = %q, %v; want %q", tag, got, err, want)
		}
	}
	for _, tag := range []string{"v1.4.0", "v1.5.0"} {
		if got, err := rel.XPIURL(ctx, tag); !errors.Is(err, setup.ErrNoAsset) {
			t.Errorf("XPIURL(%s) = %q, %v; want ErrNoAsset", tag, got, err)
		}
	}
}
