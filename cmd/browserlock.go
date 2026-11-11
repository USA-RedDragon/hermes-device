package cmd

import (
	"context"
	"encoding/json"

	"github.com/USA-RedDragon/hermes-device/internal/browser/broker"
	"github.com/USA-RedDragon/hermes-device/internal/browser/launch"
)

// lockReporter adds the screen lock state to the browser check op, which
// the extension cannot see (docs/browser.md section 7.4).
type lockReporter struct {
	*broker.Broker
}

func (r lockReporter) Call(ctx context.Context, op string, args any) (json.RawMessage, error) {
	res, err := r.Broker.Call(ctx, op, args)
	if err != nil || op != "check" {
		return res, err
	}
	return withLock(res, launch.New("").ScreenLocked(ctx)), nil
}

func withLock(res json.RawMessage, lock launch.Lock) json.RawMessage {
	var m map[string]any
	if json.Unmarshal(res, &m) != nil || m == nil {
		return res
	}
	m["screen_lock"] = lock
	out, err := json.Marshal(m)
	if err != nil {
		return res
	}
	return out
}
