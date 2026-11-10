package gateway_test

import (
	"context"
	"errors"
	"testing"

	"github.com/USA-RedDragon/hermes-hub/internal/gateway"
	"github.com/USA-RedDragon/hermes-hub/internal/gateway/gatewaytest"
)

const (
	providerBasic = "basic"
	gwUser        = "hub"
)

func TestDialLogsInAndCalls(t *testing.T) {
	t.Parallel()
	g := gatewaytest.New(gwUser, "pw")
	defer g.Close()
	g.AddSession(&gatewaytest.Session{ID: "a", Title: "A", Live: true})
	d := gateway.NewDialer(gateway.Credentials{BaseURL: g.URL(), Provider: providerBasic, Username: gwUser, Password: "pw"})
	ctx := context.Background()
	c, err := d.Dial(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var res gateway.ListResult
	if err := c.Call(ctx, gateway.MethodSessionList, gateway.ListParams{Scope: gateway.ScopeLive, Limit: 10}, &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Sessions) != 1 || res.Sessions[0].Title != "A" {
		t.Fatalf("sessions = %+v", res.Sessions)
	}
	var rerr *gateway.RPCError
	if err := c.Call(ctx, "no.such.method", map[string]any{}, nil); !errors.As(err, &rerr) {
		t.Fatalf("unknown method error = %v", err)
	}
	c2, err := d.Dial(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	c2.Close()
	if g.Logins() != 1 {
		t.Fatalf("logins = %d, want the cookie reused", g.Logins())
	}
	c.Close()
	if err := c.Call(ctx, gateway.MethodSessionList, gateway.ListParams{}, nil); !errors.Is(err, gateway.ErrClosed) {
		t.Fatalf("call after close = %v", err)
	}
}

func TestDialBadPassword(t *testing.T) {
	t.Parallel()
	g := gatewaytest.New(gwUser, "pw")
	defer g.Close()
	d := gateway.NewDialer(gateway.Credentials{BaseURL: g.URL(), Provider: providerBasic, Username: gwUser, Password: "nope"})
	if _, err := d.Dial(context.Background(), nil); !errors.Is(err, gateway.ErrUnauthorized) {
		t.Fatalf("err = %v", err)
	}
}

func TestRestartErrorsAreRetryable(t *testing.T) {
	t.Parallel()
	g := gatewaytest.New(gwUser, "pw")
	defer g.Close()
	g.AddSession(&gatewaytest.Session{ID: "a", Live: true})
	d := gateway.NewDialer(gateway.Credentials{BaseURL: g.URL(), Provider: providerBasic, Username: gwUser, Password: "pw"})
	ctx := context.Background()
	c, err := d.Dial(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	g.BeginRestart()
	err = c.Call(ctx, gateway.MethodSessionList, gateway.ListParams{Scope: gateway.ScopeLive, Limit: 10}, nil)
	if !errors.Is(err, gateway.ErrClosed) || !errors.Is(err, gateway.ErrNotReady) || !gateway.Retryable(err) {
		t.Fatalf("call during drain = %v", err)
	}
	if _, err := d.Dial(ctx, nil); !errors.Is(err, gateway.ErrNotReady) || !gateway.Retryable(err) {
		t.Fatalf("dial during drain = %v", err)
	}
	g.Stop()
	if _, err := d.Dial(ctx, nil); !errors.Is(err, gateway.ErrUnreachable) || !gateway.Retryable(err) {
		t.Fatalf("dial while stopped = %v", err)
	}
	g.FinishRestart()
	c2, err := d.Dial(ctx, nil)
	if err != nil {
		t.Fatalf("dial after restart = %v", err)
	}
	c2.Close()

	for _, tc := range []struct {
		err  error
		want bool
	}{
		{&gateway.RPCError{Code: 4001, Message: gateway.ReasonRuntimeDraining}, true},
		{&gateway.RPCError{Code: 4001, Message: gateway.ReasonNotFound}, false},
		{gateway.ErrUnauthorized, false},
		{gateway.ErrProtocol, false},
		{context.DeadlineExceeded, false},
	} {
		if got := gateway.Retryable(tc.err); got != tc.want {
			t.Errorf("Retryable(%v) = %v", tc.err, got)
		}
	}
}
