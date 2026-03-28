package symphttp

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/bazelment/yoloswe/symphony/orchestrator"
)

// Server is the optional HTTP server for observability and operational control.
// Spec Section 13.7.
type Server struct {
	orch   *orchestrator.Orchestrator
	logger *slog.Logger
	srv    *http.Server
	addr   string
}

// NewServer creates a new HTTP server bound to loopback on the given port.
// Use port 0 for an ephemeral port.
func NewServer(orch *orchestrator.Orchestrator, port int, logger *slog.Logger) *Server {
	s := &Server{
		orch:   orch,
		logger: logger,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleDashboard)
	mux.HandleFunc("/api/v1/state", s.handleState)
	mux.HandleFunc("/api/v1/refresh", s.handleRefresh)
	mux.HandleFunc("/api/v1/", s.handleIssue)

	s.srv = &http.Server{
		Addr:    fmt.Sprintf("127.0.0.1:%d", port),
		Handler: mux,
	}

	return s
}

// Start binds the server and begins serving requests.
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", s.srv.Addr)
	if err != nil {
		return fmt.Errorf("http listen: %w", err)
	}
	s.addr = ln.Addr().String()
	s.logger.Info("http server listening", "addr", s.addr)

	go func() {
		if err := s.srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			s.logger.Error("http server error", "error", err)
		}
	}()
	return nil
}

// Addr returns the bound address (host:port). Only valid after Start.
func (s *Server) Addr() string {
	return s.addr
}

// Shutdown gracefully shuts down the HTTP server.
func (s *Server) Shutdown(ctx context.Context) error {
	s.logger.Info("shutting down http server")
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return s.srv.Shutdown(ctx)
}
