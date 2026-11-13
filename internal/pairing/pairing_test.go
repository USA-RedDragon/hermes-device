package pairing_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/USA-RedDragon/talaria/internal/hubapi"
	"github.com/USA-RedDragon/talaria/internal/pairing"
)

const testName = "desktop"

type answer struct {
	status int
	body   any
}

type fakeHub struct {
	t       *testing.T
	start   hubapi.PairStart
	answers []answer
	mu      sync.Mutex
	polls   int
	pairReq hubapi.PairRequest
}

func (f *fakeHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Header.Get("Authorization") != "" {
		f.t.Errorf("%s sent Authorization", r.URL.Path)
	}
	switch r.URL.Path {
	case hubapi.PathPair:
		_ = json.NewDecoder(r.Body).Decode(&f.pairReq)
		f.encode(w, f.start)
	case hubapi.PathPairToken:
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["device_code"] != f.start.DeviceCode {
			f.t.Errorf("device_code = %q", body["device_code"])
		}
		f.mu.Lock()
		a := f.answers[min(f.polls, len(f.answers)-1)]
		f.polls++
		f.mu.Unlock()
		w.WriteHeader(a.status)
		f.encode(w, a.body)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeHub) encode(w http.ResponseWriter, v any) {
	if err := json.NewEncoder(w).Encode(v); err != nil {
		f.t.Errorf("encode: %v", err)
	}
}

func pending(code string) answer {
	return answer{http.StatusBadRequest, map[string]string{"error": code}}
}

func paired() answer {
	return answer{http.StatusOK, hubapi.PairResult{Token: "hdt_x", DeviceID: "d_1", DeviceName: testName, Backend: "alice"}}
}

func (f *fakeHub) pollCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.polls
}

type clock struct {
	mu     sync.Mutex
	now    time.Time
	sleeps []time.Duration
}

func (c *clock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sleeps = append(c.sleeps, d)
	c.now = c.now.Add(d)
	return nil
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func run(t *testing.T, f *fakeHub) (*hubapi.PairResult, *clock, *hubapi.PairStart, error) {
	t.Helper()
	f.t = t
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	clk := &clock{now: time.Unix(1_800_000_000, 0)}
	var prompted *hubapi.PairStart
	p := &pairing.Pairer{
		Hub: hubapi.New(srv.URL, "", "talaria/test"), Sleep: clk.Sleep, Now: clk.Now,
		Prompt: func(s *hubapi.PairStart) { prompted = s },
	}
	res, err := p.Pair(context.Background(), hubapi.PairRequest{Name: testName, Hostname: testName, OS: "linux", Arch: "amd64", ClientVersion: "v0.2.0"})
	return res, clk, prompted, err
}

func start(interval, expires int) hubapi.PairStart {
	return hubapi.PairStart{
		DeviceCode: "dc_secret", UserCode: "WDJB-MJHT", VerificationURI: "https://hub.example.com/device/pair", ExpiresIn: expires, Interval: interval,
	}
}

func TestPairSlowDown(t *testing.T) {
	t.Parallel()
	f := &fakeHub{start: start(5, 600), answers: []answer{
		pending(hubapi.PairPending), pending(hubapi.PairSlowDown), pending(hubapi.PairPending), pending(hubapi.PairSlowDown), paired(),
	}}
	res, clk, prompted, err := run(t, f)
	if err != nil {
		t.Fatal(err)
	}
	if res.Token != "hdt_x" || res.DeviceID != "d_1" || res.Backend != "alice" {
		t.Errorf("result = %+v", res)
	}
	want := []time.Duration{5 * time.Second, 5 * time.Second, 10 * time.Second, 10 * time.Second, 15 * time.Second}
	if !reflect.DeepEqual(clk.sleeps, want) {
		t.Errorf("sleeps = %v, want %v", clk.sleeps, want)
	}
	if prompted == nil || prompted.UserCode != "WDJB-MJHT" || prompted.VerificationURI == "" {
		t.Errorf("prompt = %+v", prompted)
	}
	if f.pairReq.Name != testName || f.pairReq.OS != "linux" || f.pairReq.ClientVersion != "v0.2.0" {
		t.Errorf("pair request = %+v", f.pairReq)
	}
}

func TestPairDefaultInterval(t *testing.T) {
	t.Parallel()
	_, clk, _, err := run(t, &fakeHub{start: start(0, 0), answers: []answer{paired()}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(clk.sleeps, []time.Duration{pairing.DefaultInterval}) {
		t.Errorf("sleeps = %v", clk.sleeps)
	}
}

func TestPairEnds(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		start   hubapi.PairStart
		answers []answer
		want    error
		polls   int
	}{
		"expired":       {start(5, 600), []answer{pending(hubapi.PairPending), pending(hubapi.PairExpired)}, pairing.ErrExpired, 2},
		"denied":        {start(5, 600), []answer{pending(hubapi.PairDenied)}, pairing.ErrDenied, 1},
		"local expiry":  {start(5, 12), []answer{pending(hubapi.PairPending)}, pairing.ErrExpired, 2},
		"unknown error": {start(5, 600), []answer{pending("invalid_grant")}, nil, 1},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := &fakeHub{start: tc.start, answers: tc.answers}
			res, _, _, err := run(t, f)
			if res != nil || err == nil {
				t.Fatalf("Pair = %+v, %v", res, err)
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
			var he *hubapi.Error
			if tc.want == nil && (!errors.As(err, &he) || he.Code != "invalid_grant") {
				t.Errorf("err = %v", err)
			}
			if f.pollCount() != tc.polls {
				t.Errorf("polls = %d, want %d", f.pollCount(), tc.polls)
			}
		})
	}
}

func TestPairRetriesServerErrors(t *testing.T) {
	t.Parallel()
	f := &fakeHub{start: start(5, 600), answers: []answer{{http.StatusBadGateway, map[string]string{"error": "bad_gateway"}}, paired()}}
	if _, _, _, err := run(t, f); err != nil {
		t.Fatal(err)
	}
	if f.pollCount() != 2 {
		t.Errorf("polls = %d", f.pollCount())
	}
}

func TestPairContextCancel(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(&fakeHub{t: t, start: start(1, 600), answers: []answer{pending(hubapi.PairPending)}})
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(context.Background())
	p := &pairing.Pairer{Hub: hubapi.New(srv.URL, "", ""), Prompt: func(*hubapi.PairStart) { cancel() }}
	done := make(chan error, 1)
	go func() {
		_, err := p.Pair(ctx, hubapi.PairRequest{Name: testName})
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Pair ignored cancel")
	}
}
