package web_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/USA-RedDragon/talaria/hub/internal/config"
	"github.com/USA-RedDragon/talaria/hub/internal/store"
	"github.com/USA-RedDragon/talaria/hub/internal/web"
)

// A hub at an https public URL sets Secure __Host- cookies; one at a plain
// http URL sets plain ones, which a browser keeps on http.
func TestLoginCookieSecureFollowsPublicURL(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, publicURL, cookie string
		secure                  bool
	}{
		{"https", "https://hub.example.test", "__Host-hub_login", true},
		{"http", "", "hub_login", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := setupWith(t, func(cfg *config.Config, _ *web.Options, _ *store.Store) {
				if tc.publicURL != "" {
					cfg.HTTP.PublicURL = tc.publicURL
				}
			})
			c := client(t)
			c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
			req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, e.url+"/auth/login?next=/", nil)
			resp, err := c.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			var got *http.Cookie
			for _, ck := range resp.Cookies() {
				if strings.HasSuffix(ck.Name, "login") {
					got = ck
				}
			}
			if got == nil {
				t.Fatalf("no login cookie in %v", resp.Header["Set-Cookie"])
			}
			if got.Name != tc.cookie || got.Secure != tc.secure || !got.HttpOnly || got.SameSite != http.SameSiteLaxMode {
				t.Fatalf("cookie = %+v, want name %s secure %v", got, tc.cookie, tc.secure)
			}
		})
	}
}
