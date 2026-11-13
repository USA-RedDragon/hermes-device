package updates_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/USA-RedDragon/talaria/hub/internal/updates"
)

const feed = `[
 {"tag_name":"hub/v0.27.0","html_url":"https://example.com/hub","published_at":"2026-11-14T00:00:00Z"},
 {"tag_name":"hub/v0.28.0-rc1","prerelease":true},
 {"tag_name":"hub/v0.26.70"},
 {"tag_name":"hermes/v0.24.0-p1"},
 {"tag_name":"hermes/v0.23.1-p40"},
 {"tag_name":"v0.27.3"},
 {"tag_name":"v0.26.0"},
 {"tag_name":"aio/v1.2.0"}
]`

func serve(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(feed)) }))
	t.Cleanup(srv.Close)
	return srv.URL
}

const running = "0.23.1"

func TestStatus(t *testing.T) {
	t.Parallel()
	src := updates.Sources{
		Agents:    func(context.Context) map[string]string { return map[string]string{"Talaria": running} },
		Computers: func(context.Context) [][2]string { return [][2]string{{"laptop", "0.25.2"}, {"desk", "v0.27.3"}} },
	}
	c := updates.New(serve(t), "0.26.70", "compose", src, nil)
	if st := c.Status(context.Background()); st.Latest != nil || !st.CheckedAt.IsZero() {
		t.Fatalf("before a check = %+v", st)
	}
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	st := c.Status(context.Background())
	if st.Latest == nil || st.Latest.Version != "0.27.0" || st.Latest.URL != "https://example.com/hub" {
		t.Fatalf("latest = %+v", st.Latest)
	}
	if len(st.Agent) != 1 || st.Agent[0].Latest != "0.24.0-p1" || st.Agent[0].Running != running {
		t.Fatalf("agent = %+v", st.Agent)
	}
	if len(st.Computers) != 1 || st.Computers[0].Name != "laptop" || st.Computers[0].Latest != "0.27.3" {
		t.Fatalf("computers = %+v", st.Computers)
	}
	cur := updates.New(serve(t), "0.27.0", "", updates.Sources{}, nil)
	_ = cur.Refresh(context.Background())
	if st := cur.Status(context.Background()); st.Latest != nil {
		t.Fatalf("up to date = %+v", st.Latest)
	}
}

func TestParse(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		a, b string
		less bool
	}{{"0.26.70", "0.27.0", true}, {"v0.27.0", "0.26.99", false}, {"0.23.1-p33", "0.23.1-p34", true}, {"0.23.1", "0.23.1-p1", true}, {"1.0", "1.0.0", false}} {
		a, ok1 := updates.Parse(c.a)
		b, ok2 := updates.Parse(c.b)
		if !ok1 || !ok2 || updates.Less(a, b) != c.less {
			t.Fatalf("%s < %s = %v", c.a, c.b, updates.Less(a, b))
		}
	}
	for _, bad := range []string{"", "dev", "1.x"} {
		if _, ok := updates.Parse(bad); ok {
			t.Fatalf("%q parsed", bad)
		}
	}
}
