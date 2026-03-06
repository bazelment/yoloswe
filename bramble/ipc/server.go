package ipc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"sync"
)

// Handler processes an IPC request and returns a result or error.
type Handler func(ctx context.Context, req *Request) (any, error)

// Server listens on a Unix domain socket and dispatches JSON requests to handlers.
type Server struct {
	listener   net.Listener
	handlers   map[RequestType]Handler
	ctx        context.Context
	cancel     context.CancelFunc
	socketPath string
	wg         sync.WaitGroup
}

// NewServer creates a new IPC server but does not start listening.
func NewServer(socketPath string) *Server {
	ctx, cancel := context.WithCancel(context.Background())
	return &Server{
		socketPath: socketPath,
		handlers:   make(map[RequestType]Handler),
		ctx:        ctx,
		cancel:     cancel,
	}
}

// Handle registers a handler for a request type.
func (s *Server) Handle(reqType RequestType, handler Handler) {
	s.handlers[reqType] = handler
}

// SocketPath returns the path to the Unix domain socket.
func (s *Server) SocketPath() string {
	return s.socketPath
}

// Start begins listening on the Unix domain socket. It removes any stale socket file first.
func (s *Server) Start() error {
	// Remove stale socket if it exists
	if err := os.Remove(s.socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("failed to remove stale socket %s: %w", s.socketPath, err)
	}

	ln, err := net.Listen("unix", s.socketPath)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", s.socketPath, err)
	}
	s.listener = ln

	s.wg.Add(1)
	go s.acceptLoop()

	return nil
}

// Close shuts down the server, closes the listener, waits for in-flight connections, and removes the socket file.
func (s *Server) Close() error {
	s.cancel()
	var err error
	if s.listener != nil {
		err = s.listener.Close()
	}
	s.wg.Wait()
	os.Remove(s.socketPath)
	return err
}

func (s *Server) acceptLoop() {
	defer s.wg.Done()
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			if s.ctx.Err() != nil {
				return // shutting down
			}
			log.Printf("ipc: accept error: %v", err)
			continue
		}
		s.wg.Add(1)
		go s.handleConn(conn)
	}
}

func (s *Server) handleConn(conn net.Conn) {
	defer s.wg.Done()
	defer conn.Close()

	dec := json.NewDecoder(conn)
	enc := json.NewEncoder(conn)

	var req Request
	if err := dec.Decode(&req); err != nil {
		resp := Response{ID: req.ID, OK: false, Error: "invalid request: " + err.Error()}
		enc.Encode(resp) //nolint:errcheck
		return
	}

	handler, ok := s.handlers[req.Type]
	if !ok {
		resp := Response{ID: req.ID, OK: false, Error: fmt.Sprintf("unknown request type: %s", req.Type)}
		enc.Encode(resp) //nolint:errcheck
		return
	}

	// Re-decode params into the correct type based on request type
	if err := s.decodeParams(&req); err != nil {
		resp := Response{ID: req.ID, OK: false, Error: "invalid params: " + err.Error()}
		enc.Encode(resp) //nolint:errcheck
		return
	}

	result, err := handler(s.ctx, &req)
	if err != nil {
		resp := Response{ID: req.ID, OK: false, Error: err.Error()}
		enc.Encode(resp) //nolint:errcheck
		return
	}

	resp := Response{ID: req.ID, OK: true, Result: result}
	enc.Encode(resp) //nolint:errcheck
}

// decodeParams re-marshals req.Params (which is a map after initial decode)
// into the correct typed struct.
func (s *Server) decodeParams(req *Request) error {
	if req.Params == nil {
		return nil
	}
	raw, err := json.Marshal(req.Params)
	if err != nil {
		return err
	}
	switch req.Type {
	case RequestNewSession:
		var p NewSessionParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return err
		}
		req.Params = &p
	case RequestNotify:
		var p NotifyParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return err
		}
		req.Params = &p
	default:
		// No typed params needed
	}
	return nil
}
