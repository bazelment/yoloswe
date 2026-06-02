package control

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"sync"
)

// Conn is a bidirectional, message-framed control connection. Both the local
// Unix-socket transport and the remote WebSocket transport implement it so the
// dispatcher-serving loop and the requesting client are transport-agnostic.
//
// Messages are framed as one JSON object per line (newline-delimited JSON):
// simple, streaming-friendly, and adequate for both transports. WriteMsg is
// safe for concurrent use; ReadMsg is not (call it from a single reader loop).
type Conn interface {
	ReadMsg() (*Msg, error)
	WriteMsg(*Msg) error
	Close() error
}

// jsonConn is a Conn over any io.ReadWriteCloser using newline-delimited JSON.
type jsonConn struct {
	rwc io.ReadWriteCloser
	r   *bufio.Reader
	wmu sync.Mutex // serializes writes
}

// NewJSONConn wraps an io.ReadWriteCloser (e.g. a net.Conn) as a Conn.
func NewJSONConn(rwc io.ReadWriteCloser) Conn {
	return &jsonConn{rwc: rwc, r: bufio.NewReader(rwc)}
}

func (c *jsonConn) ReadMsg() (*Msg, error) {
	line, err := c.r.ReadBytes('\n')
	if err != nil {
		if len(line) == 0 {
			return nil, err
		}
		// Trailing line without newline (peer closed after writing): still parse.
	}
	var m Msg
	if err := json.Unmarshal(line, &m); err != nil {
		return nil, fmt.Errorf("control: decode msg: %w", err)
	}
	return &m, nil
}

func (c *jsonConn) WriteMsg(m *Msg) error {
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err = c.rwc.Write(raw)
	return err
}

func (c *jsonConn) Close() error { return c.rwc.Close() }

// Serve reads requests from conn and dispatches each through handler, writing
// responses back. It returns when conn hits EOF/error or ctx is cancelled.
// Each request is handled in its own goroutine so a slow op does not block the
// read loop or other requests; WriteMsg's mutex serializes the replies.
func Serve(ctx context.Context, conn Conn, handler *Dispatcher) error {
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		msg, err := conn.ReadMsg()
		if err != nil {
			return err
		}
		wg.Add(1)
		go func(req *Msg) {
			defer wg.Done()
			resp := handler.Handle(ctx, req)
			_ = conn.WriteMsg(resp)
		}(msg)
	}
}

// --- Unix-socket transport (local CLI) --------------------------------------

// DialUnix connects to a control server on a Unix domain socket.
func DialUnix(socketPath string) (Conn, error) {
	c, err := net.Dial("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("control: dial %s: %w", socketPath, err)
	}
	return NewJSONConn(c), nil
}

// Request performs one request/response round-trip over a fresh connection.
// Convenience for one-shot CLI calls. For streaming, use Serve/DialUnix and the
// Conn directly.
func Request(ctx context.Context, socketPath string, req *Msg) (*Msg, error) {
	conn, err := DialUnix(socketPath)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if err := conn.WriteMsg(req); err != nil {
		return nil, err
	}
	type res struct {
		m   *Msg
		err error
	}
	ch := make(chan res, 1)
	go func() {
		m, err := conn.ReadMsg()
		ch <- res{m, err}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-ch:
		return r.m, r.err
	}
}
