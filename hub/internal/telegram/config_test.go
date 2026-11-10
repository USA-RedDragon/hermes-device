package telegram_test

import (
	"errors"
	"testing"
	"time"

	"github.com/USA-RedDragon/hermes-hub/internal/telegram"
)

func TestConfigValidate(t *testing.T) {
	t.Parallel()
	good := telegram.Config{
		Enabled: true, Token: "t",
		DefaultBackends: []string{"alice=cluster", " bob = hers "},
		PollTimeout:     time.Second, EditInterval: time.Second, LeaderRetry: time.Second,
	}
	if err := good.Validate(); err != nil {
		t.Fatalf("good config: %v", err)
	}
	if err := (telegram.Config{}).Validate(); err != nil {
		t.Fatalf("disabled config: %v", err)
	}
	cases := []struct {
		mutate func(*telegram.Config)
		want   error
	}{
		{func(c *telegram.Config) { c.Token = "" }, telegram.ErrMissingToken},
		{func(c *telegram.Config) { c.EditInterval = 0 }, telegram.ErrBadInterval},
		{func(c *telegram.Config) { c.LeaderRetry = 0 }, telegram.ErrBadInterval},
		{func(c *telegram.Config) { c.DefaultBackends = []string{userAlice} }, telegram.ErrBadDefaultBackend},
		{func(c *telegram.Config) { c.DefaultBackends = []string{"=x"} }, telegram.ErrBadDefaultBackend},
	}
	if _, err := telegram.New(good, telegram.Options{}); !errors.Is(err, telegram.ErrMissingPublicURL) {
		t.Errorf("New without PublicURL: got %v", err)
	}
	for i, tc := range cases {
		c := good
		tc.mutate(&c)
		if err := c.Validate(); !errors.Is(err, tc.want) {
			t.Errorf("case %d: got %v, want %v", i, err, tc.want)
		}
		if _, err := telegram.New(c, telegram.Options{PublicURL: "https://hub.example"}); !errors.Is(err, tc.want) {
			t.Errorf("case %d: New got %v, want %v", i, err, tc.want)
		}
	}
}
