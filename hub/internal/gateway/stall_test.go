package gateway_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/USA-RedDragon/talaria/hub/internal/gateway"
	"github.com/USA-RedDragon/talaria/hub/internal/gateway/gatewaytest"
)

// A gateway that leaves one request of a connection unanswered holds every
// later one on it. The client ends such a connection, so a new one works.
func TestStalledRequestEndsTheConnection(t *testing.T) {
	t.Parallel()
	g := gatewaytest.New(gwUser, "pw")
	defer g.Close()
	release := g.StallNext(gateway.MethodPromptSubmit)
	defer release()
	d := gateway.NewDialer(gateway.Credentials{BaseURL: g.URL(), Provider: providerBasic, Username: gwUser, Password: "pw"})
	c, err := d.Dial(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetStallAfter(200 * time.Millisecond)
	stuck := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
		defer cancel()
		stuck <- c.Call(ctx, gateway.MethodPromptSubmit, gateway.SubmitParams{SessionID: "x", Text: "hi"}, nil)
	}()
	// Queued behind the stuck request, with a longer bound of its own.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	time.Sleep(50 * time.Millisecond)
	listErr := c.Call(ctx, gateway.MethodSessionList, gateway.ListParams{Scope: gateway.ScopeLive}, nil)
	if err := <-stuck; !errors.Is(err, gateway.ErrStalled) || !errors.Is(err, gateway.ErrClosed) {
		t.Fatalf("stuck call = %v, want ErrStalled and ErrClosed", err)
	}
	if !errors.Is(listErr, gateway.ErrStalled) || !gateway.Retryable(listErr) {
		t.Fatalf("queued call = %v, want a retryable ErrStalled", listErr)
	}
	select {
	case <-c.Done():
	case <-time.After(time.Second):
		t.Fatal("the stalled connection did not end")
	}
	c2, err := d.Dial(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	if err := c2.Call(ctx, gateway.MethodSessionList, gateway.ListParams{Scope: gateway.ScopeLive}, nil); err != nil {
		t.Fatalf("list on a new connection = %v", err)
	}
}

// A short bound that runs out early is the caller's limit, not a stall.
func TestShortDeadlineKeepsTheConnection(t *testing.T) {
	t.Parallel()
	g := gatewaytest.New(gwUser, "pw")
	defer g.Close()
	release := g.StallNext(gateway.MethodSessionList)
	d := gateway.NewDialer(gateway.Credentials{BaseURL: g.URL(), Provider: providerBasic, Username: gwUser, Password: "pw"})
	c, err := d.Dial(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := c.Call(ctx, gateway.MethodSessionList, gateway.ListParams{}, nil); !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, gateway.ErrStalled) {
		t.Fatalf("call = %v", err)
	}
	release()
	if err := c.Call(context.Background(), gateway.MethodSessionList, gateway.ListParams{}, nil); err != nil {
		t.Fatalf("call after release = %v", err)
	}
}
