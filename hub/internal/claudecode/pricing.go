package claudecode

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/USA-RedDragon/talaria/hub/internal/hub"
)

// pricesJSON is the Claude part of LiteLLM's
// model_prices_and_context_window.json, the price list ccusage uses. The
// Update prices workflow refreshes it; the hub never fetches it.
//
//go:embed prices.json
var pricesJSON []byte

// Price is a model's USD price per token.
type Price struct {
	Input        float64 `json:"input_cost_per_token"`
	Output       float64 `json:"output_cost_per_token"`
	CacheWrite   float64 `json:"cache_creation_input_token_cost"`
	CacheWrite1h float64 `json:"cache_creation_input_token_cost_above_1hr"`
	CacheRead    float64 `json:"cache_read_input_token_cost"`
}

// Prices finds the price of a model.
type Prices struct {
	m map[string]Price
}

// ParsePrices reads a price list in LiteLLM's format.
func ParsePrices(data []byte) (*Prices, error) {
	var m map[string]Price
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	for k, p := range m {
		if p.Input <= 0 {
			delete(m, k)
		}
	}
	return &Prices{m: m}, nil
}

// DefaultPrices is the embedded price list. An unreadable or empty list
// is an error, so that a bad update cannot price everything at $0.
func DefaultPrices() (*Prices, error) {
	p, err := ParsePrices(pricesJSON)
	if err != nil {
		return nil, fmt.Errorf("embedded price list: %w", err)
	}
	if len(p.m) == 0 {
		return nil, errors.New("embedded price list has no prices")
	}
	return p, nil
}

var dateSuffix = regexp.MustCompile(`-\d{8}$`)

// Find returns the price of model: its own entry, the entry under
// anthropic/, or the entry without a -fast suffix (fast mode costs more
// than this) or a -YYYYMMDD date.
func (p *Prices) Find(model string) (Price, bool) {
	if model == "" {
		return Price{}, false
	}
	m := strings.TrimSuffix(model, "-fast")
	for _, k := range []string{model, "anthropic/" + model, m, "anthropic/" + m, dateSuffix.ReplaceAllString(m, "")} {
		if x, ok := p.m[k]; ok {
			return fill(x), true
		}
	}
	return Price{}, false
}

// fill supplies the rates LiteLLM leaves out with Anthropic's multiples of
// the input rate: 1.25 for a 5-minute cache write, 2 for a 1-hour one (as
// ccusage prices it) and 0.1 for a cache read.
func fill(p Price) Price {
	if p.CacheWrite <= 0 {
		p.CacheWrite = p.Input * 1.25
	}
	if p.CacheWrite1h <= 0 {
		p.CacheWrite1h = p.Input * 2
	}
	if p.CacheRead <= 0 {
		p.CacheRead = p.Input * 0.1
	}
	return p
}

// Cost is what r's tokens cost at p. It is an estimate: long-context
// rates and the fast-mode multiplier are not applied.
func (p Price) Cost(r hub.ClaudeCodeRow) float64 {
	cw5m := r.CacheWrite - r.CacheWrite1h
	if cw5m < 0 {
		cw5m = 0
	}
	return float64(r.Input)*p.Input + float64(r.Output)*p.Output + float64(cw5m)*p.CacheWrite +
		float64(r.CacheWrite1h)*p.CacheWrite1h + float64(r.CacheRead)*p.CacheRead
}
