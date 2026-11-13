package claude_test

import (
	"math"
	"testing"
	"time"

	"github.com/USA-RedDragon/talaria/hub/internal/claude"
	"github.com/USA-RedDragon/talaria/hub/internal/hub"
)

func poolAccount(label string, used float64, resets time.Time) hub.ClaudeAccount {
	return hub.ClaudeAccount{ID: label, Label: label, Provider: "claude-" + label, SignedIn: true,
		Usage: &hub.ClaudeUsage{Windows: []hub.ClaudeWindow{
			{Kind: hub.ClaudeWindowSession, UsedPercent: &used},
			{Kind: hub.ClaudeWindowWeekly, UsedPercent: &used, ResetsAt: resets, ResetsText: resets.Format("Mon 3:04 PM")}}}}
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestPoolUsageExample(t *testing.T) {
	t.Parallel()
	sat := time.Date(2026, 11, 14, 21, 0, 0, 0, time.UTC)
	sun := sat.Add(24 * time.Hour)
	accounts := make([]hub.ClaudeAccount, 0, 7)
	accounts = append(accounts, poolAccount("jaam", 100, sun), poolAccount("jamaag", 100, sun.Add(time.Hour)),
		poolAccount("ja2000aag", 130, sun), poolAccount("caam", 54, sat), poolAccount("maam", 0, sat))
	credits := hub.ClaudeAccount{ID: "x", Label: "x", Provider: "claude-api-jaam", SignedIn: true}
	chatgpt := hub.ClaudeAccount{ID: "gpt", Label: "ChatGPT", Provider: "openai-codex", SignedIn: true, NoSignIn: true}
	accounts[0].Credits = &hub.ClaudeCredits{Provider: "claude-api-jaam"}
	p := claude.PoolUsage(append(accounts, credits, chatgpt))
	if p == nil || p.Accounts != 5 || p.Read != 5 || p.UsedPercent == nil {
		t.Fatalf("pool = %+v", p)
	}
	if got := math.Round(*p.UsedPercent); got != 71 {
		t.Errorf("used = %v, want 71", *p.UsedPercent)
	}
	if !near(*p.UsedPercent, 70.8) || !near(p.RoomAccounts, 1.46) {
		t.Errorf("used %v room %v, want 70.8 and 1.46", *p.UsedPercent, p.RoomAccounts)
	}
	if p.NextAccount != "jaam" || !p.NextAt.Equal(sun) {
		t.Errorf("next = %q at %v, want jaam at %v", p.NextAccount, p.NextAt, sun)
	}
}

func TestPoolUsageUnreadableAccount(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 14, 21, 0, 0, 0, time.UTC)
	out := poolAccount("out", 0, at)
	out.SignedIn = false
	none := poolAccount("none", 0, at)
	none.Usage = &hub.ClaudeUsage{Unavailable: "no read"}
	p := claude.PoolUsage([]hub.ClaudeAccount{poolAccount("a", 100, at), poolAccount("b", 50, at), out, none})
	if p.Accounts != 4 || p.Read != 2 || !near(*p.UsedPercent, 75) || !near(p.RoomAccounts, 0.5) {
		t.Errorf("pool = %+v", p)
	}
	all := claude.PoolUsage([]hub.ClaudeAccount{out, none})
	if all == nil || all.Read != 0 || all.UsedPercent != nil {
		t.Errorf("none read: %+v", all)
	}
	if claude.PoolUsage(nil) != nil {
		t.Error("no accounts gave a pool")
	}
}

func TestPoolUsageNoFullAccount(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 14, 21, 0, 0, 0, time.UTC)
	p := claude.PoolUsage([]hub.ClaudeAccount{poolAccount("a", 99, at), poolAccount("b", 20, at)})
	if p.NextAccount != "" || !p.NextAt.IsZero() || p.NextText != "" {
		t.Errorf("next set with no full account: %+v", p)
	}
	if !near(*p.UsedPercent, 59.5) || !near(p.RoomAccounts, 0.01+0.8) {
		t.Errorf("pool = %+v", p)
	}
}
