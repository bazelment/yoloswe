package control

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
)

// SockEnvVar is the environment variable carrying the control socket path,
// discovered by CLI subcommands (parallel to ipc.SockEnvVar for the legacy IPC).
const SockEnvVar = "BRAMBLE_CONTROL_SOCK"

// UnixServer listens on a Unix domain socket and serves the control protocol
// against a Dispatcher. It is the local transport for bramble's own CLI
// subcommands (send-input, send-key, etc.); the remote transport (hub
// WebSocket) reuses the same Dispatcher and Serve loop.
type UnixServer struct {
	disp       *Dispatcher
	ln         net.Listener
	ctx        context.Context
	cancel     context.CancelFunc
	socketPath string
	wg         sync.WaitGroup
}

// NewUnixServer creates a control server bound to socketPath (not yet started).
func NewUnixServer(socketPath string, disp *Dispatcher) *UnixServer {
	ctx, cancel := context.WithCancel(context.Background())
	return &UnixServer{socketPath: socketPath, disp: disp, ctx: ctx, cancel: cancel}
}

// SocketPath returns the listening socket path.
func (s *UnixServer) SocketPath() string { return s.socketPath }

// Start removes any stale socket and begins accepting connections.
func (s *UnixServer) Start() error {
	if err := os.Remove(s.socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("control: remove stale socket: %w", err)
	}
	ln, err := net.Listen("unix", s.socketPath)
	if err != nil {
		return fmt.Errorf("control: listen %s: %w", s.socketPath, err)
	}
	s.ln = ln
	s.wg.Add(1)
	go s.acceptLoop()
	return nil
}

func (s *UnixServer) acceptLoop() {
	defer s.wg.Done()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			if s.ctx.Err() != nil {
				return
			}
			continue
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			c := NewJSONConn(conn)
			_ = Serve(s.ctx, c, s.disp)
		}()
	}
}

// Close stops the server, closes the listener, waits for in-flight connections,
// and removes the socket file.
func (s *UnixServer) Close() error {
	s.cancel()
	var err error
	if s.ln != nil {
		err = s.ln.Close()
	}
	s.wg.Wait()
	os.Remove(s.socketPath)
	return err
}
