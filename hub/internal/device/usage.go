package device

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/USA-RedDragon/talaria/assertion"
	"github.com/USA-RedDragon/talaria/hub/internal/hub"
)

// The Claude Code usage leg of acp-host (talaria docs/protocol.md section
// 13). The hub's talaria module predates them, so they are spelled here.
const (
	legUsage      = "usage"
	acpUsagePath  = "/usage"
	maxUsageBody  = 32 << 20
	usageSubject  = "talaria-hub"
	usageDeviceID = "hub"
)

// UsageSink stores the Claude Code usage a computer sends.
// *claudecode.Service is one.
type UsageSink interface {
	IngestDevice(ctx context.Context, d hub.Device, body []byte) error
}

// SetUsage sets where POST /api/device/usage goes. Before it is set the
// route answers 404.
func (s *Server) SetUsage(u UsageSink) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.usage = u
}

func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	sink := s.usage
	s.mu.Unlock()
	if sink == nil {
		apiError(w, http.StatusNotFound, "not_found", "this hub does not collect Claude Code usage")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxUsageBody+1))
	if err != nil {
		apiError(w, http.StatusBadRequest, codeBadRequest, "could not read the body")
		return
	}
	if len(body) > maxUsageBody {
		apiError(w, http.StatusRequestEntityTooLarge, codeBadRequest, "the report is too large")
		return
	}
	d := deviceFrom(r)
	if err := sink.IngestDevice(r.Context(), d, body); err != nil {
		if errors.Is(err, hub.ErrBadClaudeCodeReport) {
			apiError(w, http.StatusBadRequest, codeBadRequest, err.Error())
			return
		}
		s.log.Error("store Claude Code usage", "device", d.ID, "error", err.Error())
		apiError(w, http.StatusServiceUnavailable, codeInternal, "the hub could not store the report")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// HermesUsage reads the Claude Code usage report of a backend's acp-host,
// with a hub assertion for the usage leg.
func (s *Server) HermesUsage(ctx context.Context, backendID string) ([]byte, error) {
	b := s.backends[backendID]
	if b == nil || b.cfg.ACPURL == "" {
		return nil, fmt.Errorf("backend %s has no acp-host", backendID)
	}
	now := s.opts.Now()
	tok, err := assertion.Sign(s.opts.Key, assertion.Claims{
		Aud: backendID, Leg: legUsage, Method: http.MethodGet, Path: acpUsagePath,
		Sub: usageSubject, DeviceID: usageDeviceID, DeviceName: usageSubject,
		IssuedAt: now.Unix(), Expires: now.Add(AssertionLifetime).Unix(),
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(b.cfg.ACPURL, "/")+acpUsagePath, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set(assertion.Header, tok)
	resp, err := s.usageClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("acp-host answered %s", resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxUsageBody+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxUsageBody {
		return nil, errors.New("acp-host usage report too large")
	}
	return data, nil
}

func (s *Server) usageClient() *http.Client {
	return &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}
