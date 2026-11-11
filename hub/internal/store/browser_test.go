package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/USA-RedDragon/hermes-hub/internal/hub"
	"github.com/USA-RedDragon/hermes-hub/internal/store"
)

const (
	instanceA = "instance-aaaaaaaaaaaa"
	instanceB = "instance-bbbbbbbbbbbb"
	firefox   = "Firefox 157.0"
)

func pairedDevice(t *testing.T, s *store.Store, user, name string) hub.Device {
	t.Helper()
	ctx := context.Background()
	_, code, err := s.StartPairing(ctx, hub.PairingRequest{Name: name, Hostname: name}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.ApprovePairing(ctx, code, user, "pc", name)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestBrowserCodes(t *testing.T) {
	t.Parallel()
	backends(t, func(t *testing.T, s *store.Store) {
		ctx := context.Background()
		now := time.Unix(1_800_000_000, 0)
		s.SetClock(func() time.Time { return now })
		g := hub.BrowserGrant{User: userAlice, DeviceID: "d_1", InstanceID: instanceA, Browser: firefox, Challenge: "c", RedirectURI: "r"}
		code, err := s.CreateBrowserCode(ctx, g)
		if err != nil || code == "" {
			t.Fatalf("create = %q %v", code, err)
		}
		got, ok, err := s.RedeemBrowserCode(ctx, code)
		if !ok || err != nil || got != g {
			t.Fatalf("redeem = %+v %v %v", got, ok, err)
		}
		if _, ok, _ := s.RedeemBrowserCode(ctx, code); ok {
			t.Fatal("a code redeemed twice")
		}
		code, _ = s.CreateBrowserCode(ctx, g)
		now = now.Add(hub.BrowserCodeTTL)
		if _, ok, _ := s.RedeemBrowserCode(ctx, code); ok {
			t.Fatal("an expired code was redeemed")
		}
	})
}

func TestBrowserSessions(t *testing.T) {
	t.Parallel()
	backends(t, func(t *testing.T, s *store.Store) {
		ctx := context.Background()
		now := time.Unix(1_800_000_000, 0)
		s.SetClock(func() time.Time { return now })
		d := pairedDevice(t, s, userAlice, deviceName)
		tok, b, err := s.CreateBrowserSession(ctx, userAlice, d.ID, instanceA, firefox)
		if err != nil || !strings.HasPrefix(tok, "hbt_") || len(tok) != 47 {
			t.Fatalf("create = %q %v", tok, err)
		}
		got, ok, err := s.BrowserSessionByToken(ctx, tok)
		if !ok || err != nil || got.ID != b.ID || got.User != userAlice || got.DeviceID != d.ID || got.Browser != firefox {
			t.Fatalf("by token = %+v %v %v", got, ok, err)
		}
		if _, ok, _ := s.BrowserSessionByToken(ctx, "hbt_"+strings.Repeat("x", 43)); ok {
			t.Fatal("unknown token found")
		}
		tok2, b2, err := s.CreateBrowserSession(ctx, userAlice, d.ID, instanceA, firefox)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok, _ := s.BrowserSessionByToken(ctx, tok); ok {
			t.Fatal("signing in again kept the old sign-in of the same instance")
		}
		now = now.Add(time.Second)
		_, b3, _ := s.CreateBrowserSession(ctx, userAlice, d.ID, instanceB, firefox)
		list, err := s.BrowserSessions(ctx, userAlice)
		if err != nil || len(list) != 2 || list[0].ID != b2.ID || list[1].ID != b3.ID {
			t.Fatalf("list = %+v %v", list, err)
		}
		if l, _ := s.BrowserSessions(ctx, userBob); len(l) != 0 {
			t.Fatalf("bob's list = %+v", l)
		}
		if _, ok, _ := s.RevokeBrowserSession(ctx, userBob, b2.ID); ok {
			t.Fatal("bob revoked alice's sign-in")
		}
		if dev, ok, err := s.RevokeBrowserSession(ctx, userAlice, b2.ID); !ok || err != nil || dev != d.ID {
			t.Fatalf("revoke = %q %v %v", dev, ok, err)
		}
		if _, ok, _ := s.BrowserSessionByToken(ctx, tok2); ok {
			t.Fatal("revoked sign-in still works")
		}

		browserExpiry(t, s, &now, d.ID, b3.ID)
	})
}

// browserExpiry checks idle expiry, sign-out by token and the end of a
// device's sign-ins when the device is revoked.
func browserExpiry(t *testing.T, s *store.Store, clock *time.Time, deviceID, id string) {
	t.Helper()
	ctx := context.Background()
	*clock = clock.Add(hub.BrowserIdleTTL - time.Hour)
	if err := s.TouchBrowserSession(ctx, id); err != nil {
		t.Fatal(err)
	}
	*clock = clock.Add(2 * time.Hour)
	if _, ok, _ := s.BrowserSession(ctx, id); !ok {
		t.Fatal("a sign-in in use expired")
	}
	*clock = clock.Add(hub.BrowserIdleTTL)
	if _, ok, _ := s.BrowserSession(ctx, id); ok {
		t.Fatal("an idle sign-in did not expire")
	}

	tok4, b4, _ := s.CreateBrowserSession(ctx, userAlice, deviceID, instanceB, firefox)
	if got, ok, err := s.RevokeBrowserToken(ctx, tok4); !ok || err != nil || got.ID != b4.ID {
		t.Fatalf("revoke token = %+v %v %v", got, ok, err)
	}
	if _, ok, _ := s.RevokeBrowserToken(ctx, tok4); ok {
		t.Fatal("revoked twice")
	}
	_, b5, _ := s.CreateBrowserSession(ctx, userAlice, deviceID, instanceB, firefox)
	if ok, err := s.RevokeDevice(ctx, userAlice, deviceID); !ok || err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.BrowserSession(ctx, b5.ID); ok {
		t.Fatal("a sign-in of a revoked device still works")
	}
}
