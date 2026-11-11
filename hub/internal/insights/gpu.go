package insights

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/USA-RedDragon/hermes-hub/internal/hub"
	"golang.org/x/sync/errgroup"
)

// GPU reads the local GPU's time from the metrics of ollama-priority-proxy
// through a Prometheus query API.
type GPU struct {
	base string
	http *http.Client

	mu  sync.Mutex
	at  time.Time
	out []hub.GPUWindow
}

// gpuCache is how long Windows reuses its last answer: the totals are
// the same for every user and change slowly.
const gpuCache = time.Minute

// NewGPU returns a GPU that queries the Prometheus API at base.
func NewGPU(base string, c *http.Client) *GPU {
	if c == nil {
		c = http.DefaultClient
	}
	return &GPU{base: base, http: c}
}

// gpuWindows are the windows the Usage page shows.
func gpuWindows() []struct{ label, rng string } {
	return []struct{ label, rng string }{
		{"Last 24 hours", "24h"},
		{"Last 7 days", "7d"},
		{"Last 30 days", "30d"},
	}
}

// Proxy metric names (ollama-priority-proxy README, Metrics). Class high
// is people's requests, low is background work.
const (
	metricInFlight = "ollama_priority_proxy_in_flight"
	metricDuration = "ollama_priority_proxy_upstream_duration_seconds"
	classPeople    = "high"
	classBack      = "low"
)

// errPrometheus is a query Prometheus did not answer as expected.
var errPrometheus = errors.New("prometheus query failed")

// Windows reads, for each window, how long the GPU had a request running
// (sampled each minute), the summed request time of each class and the
// number of requests.
func (g *GPU) Windows(ctx context.Context) ([]hub.GPUWindow, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.out != nil && time.Since(g.at) < gpuCache {
		return g.out, nil
	}
	out, err := g.read(ctx)
	if err != nil {
		return nil, err
	}
	g.out, g.at = out, time.Now()
	return out, nil
}

func (g *GPU) read(ctx context.Context) ([]hub.GPUWindow, error) {
	windows := gpuWindows()
	out := make([]hub.GPUWindow, len(windows))
	eg, ctx := errgroup.WithContext(ctx)
	for i, w := range windows {
		out[i].Label = w.label
		eg.Go(func() error {
			busy, err := g.query(ctx, fmt.Sprintf("sum_over_time((sum(%s) > bool 0)[%s:1m]) * 60", metricInFlight, w.rng))
			if err != nil {
				return err
			}
			out[i].Busy = busy[""]
			return nil
		})
		eg.Go(func() error {
			by, err := g.query(ctx, fmt.Sprintf("sum by (class) (increase(%s_sum[%s]))", metricDuration, w.rng))
			if err != nil {
				return err
			}
			out[i].People, out[i].Background = by[classPeople], by[classBack]
			return nil
		})
		eg.Go(func() error {
			n, err := g.query(ctx, fmt.Sprintf("sum(increase(%s_count[%s]))", metricDuration, w.rng))
			if err != nil {
				return err
			}
			out[i].Requests = n[""]
			return nil
		})
	}
	return out, eg.Wait()
}

type promAnswer struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			Value  [2]any            `json:"value"`
		} `json:"result"`
	} `json:"data"`
}

// query runs an instant query and returns each sample by its class label
// ("" for none). An empty result is no samples, not an error.
func (g *GPU) query(ctx context.Context, q string) (map[string]float64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.base+"/api/v1/query?query="+url.QueryEscape(q), nil)
	if err != nil {
		return nil, err
	}
	resp, err := g.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var a promAnswer
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&a); err != nil {
		return nil, fmt.Errorf("%w: %w", errPrometheus, err)
	}
	if resp.StatusCode != http.StatusOK || a.Status != "success" || a.Data.ResultType != "vector" {
		return nil, fmt.Errorf("%w: status %d", errPrometheus, resp.StatusCode)
	}
	out := map[string]float64{}
	for _, r := range a.Data.Result {
		s, _ := r.Value[1].(string)
		v, err := strconv.ParseFloat(s, 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			continue
		}
		out[r.Metric["class"]] += v
	}
	return out, nil
}
