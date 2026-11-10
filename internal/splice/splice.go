// Package splice is the run daemon's loopback gateway splice: the local
// Hermes TUI dials it, and it forwards to the hub's gateway leg with the
// rewrites and denials of docs/protocol.md section 5.
package splice

import (
	"context"
	"errors"
	"net"
	"time"

	"github.com/USA-RedDragon/hermes-device/internal/hubapi"
)

// ErrNotImplemented is returned until the splice is built.
var ErrNotImplemented = errors.New("splice not implemented")

// Options configure a Server.
type Options struct {
	// Hub dials the gateway leg and binds sessions.
	Hub *hubapi.Client
	// PingInterval is the WebSocket ping interval on both hops.
	PingInterval time.Duration
}

// Server serves /api/ws for the TUI and /control/tokens for chat.
type Server struct {
	opts          Options
	controlSecret string
}

// New returns a server with a fresh control secret.
func New(opts Options) *Server {
	return &Server{opts: opts}
}

// ControlSecret is the secret chat sends to /control/tokens.
func (s *Server) ControlSecret() string { return s.controlSecret }

// Serve serves on ln until ctx ends.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	_ = ctx
	_ = ln
	return ErrNotImplemented
}
