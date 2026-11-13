package claude

import (
	"math"
	"strings"

	"github.com/USA-RedDragon/hermes-hub/internal/hub"
)

// apiCreditsPrefix starts the provider of an account's API key. Those
// credits are never part of the subscription total.
const apiCreditsPrefix = "claude-api-"

// PoolUsage pools the weekly use of Claude subscription accounts. Accounts
// that are not signed in, or whose week could not be read, are left out of
// the average and counted in Accounts only. It returns nil when there are
// no accounts.
func PoolUsage(accounts []hub.ClaudeAccount) *hub.ClaudePoolUsage {
	out := &hub.ClaudePoolUsage{}
	var sum float64
	var next hub.ClaudeWindow
	for _, a := range accounts {
		if a.NoSignIn || strings.HasPrefix(a.Provider, apiCreditsPrefix) {
			continue
		}
		out.Accounts++
		if !a.SignedIn || a.Usage == nil {
			continue
		}
		var week *hub.ClaudeWindow
		for i := range a.Usage.Windows {
			if w := &a.Usage.Windows[i]; w.Kind == hub.ClaudeWindowWeekly && w.UsedPercent != nil {
				week = w
				break
			}
		}
		if week == nil {
			continue
		}
		used := math.Min(100, math.Max(0, *week.UsedPercent))
		out.Read++
		sum += used
		out.RoomAccounts += (100 - used) / 100
		if used >= 100 && !week.ResetsAt.IsZero() && (out.NextAccount == "" || week.ResetsAt.Before(next.ResetsAt)) {
			next = *week
			out.NextAccount = a.Label
		}
	}
	if out.Accounts == 0 {
		return nil
	}
	if out.Read > 0 {
		avg := sum / float64(out.Read)
		out.UsedPercent = &avg
	}
	out.NextAt, out.NextText = next.ResetsAt, next.ResetsText
	return out
}
