// Package server runs the hub's HTTP listeners: the web UI on IPv4 and
// IPv6, and optionally Prometheus metrics on their own port.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/USA-RedDragon/hermes-hub/internal/config"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/sync/errgroup"
)

const defTimeout = 10 * time.Second

// Server is the set of listeners.
type Server struct {
	cfg     *config.HTTP
	servers []*http.Server
	names   []string
}

// New wraps the UI handler with request metrics and builds the listeners.
func New(cfg *config.HTTP, ui http.Handler, reg prometheus.Registerer) *Server {
	requests := promauto.With(reg).NewCounterVec(prometheus.CounterOpts{
		Name: "hermes_hub_http_requests_total", Help: "HTTP requests by status code and method.",
	}, []string{"code", "method"})
	handler := promhttp.InstrumentHandlerCounter(requests, ui)
	mk := func(host string, port uint16, h http.Handler) *http.Server {
		return &http.Server{Addr: net.JoinHostPort(host, strconv.Itoa(int(port))), Handler: h,
			ReadHeaderTimeout: defTimeout, IdleTimeout: 2 * time.Minute}
	}
	s := &Server{cfg: cfg}
	s.add("HTTP IPv4", mk(cfg.IPV4Host, cfg.Port, handler))
	s.add("HTTP IPv6", mk(cfg.IPV6Host, cfg.Port, handler))
	if cfg.Metrics.Enabled {
		mux := http.NewServeMux()
		mux.Handle("GET /metrics", promhttp.Handler())
		s.add("Metrics IPv4", mk(cfg.Metrics.IPV4Host, cfg.Metrics.Port, mux))
		s.add("Metrics IPv6", mk(cfg.Metrics.IPV6Host, cfg.Metrics.Port, mux))
	}
	return s
}

func (s *Server) add(name string, srv *http.Server) {
	s.names = append(s.names, name)
	s.servers = append(s.servers, srv)
}

// Start opens every listener. An IPv6 listener that cannot open, as on a
// host without IPv6, is logged and skipped.
func (s *Server) Start(ctx context.Context) error {
	var lc net.ListenConfig
	for i, srv := range s.servers {
		network := "tcp4"
		if i%2 == 1 {
			network = "tcp6"
		}
		l, err := lc.Listen(ctx, network, srv.Addr)
		if err != nil {
			if network == "tcp6" {
				slog.Warn(s.names[i]+" listener not started", "addr", srv.Addr, "error", err.Error())
				s.servers[i] = nil
				continue
			}
			return fmt.Errorf("listen %s: %w", srv.Addr, err)
		}
		go func() {
			if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
				slog.Error(s.names[i]+" server error", "error", err.Error())
			}
		}()
	}
	slog.Info("HTTP server started", "ipv4", s.cfg.IPV4Host, "ipv6", s.cfg.IPV6Host, "port", s.cfg.Port)
	if s.cfg.Metrics.Enabled {
		slog.Info("Metrics server started", "ipv4", s.cfg.Metrics.IPV4Host, "ipv6", s.cfg.Metrics.IPV6Host, "port", s.cfg.Metrics.Port)
	}
	return nil
}

// Stop shuts every listener down, waiting up to 5s for open requests.
func (s *Server) Stop() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var g errgroup.Group
	for _, srv := range s.servers {
		if srv != nil {
			g.Go(func() error { return srv.Shutdown(ctx) })
		}
	}
	return g.Wait()
}
