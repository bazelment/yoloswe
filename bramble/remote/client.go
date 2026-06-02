// Package remote is bramble's agent-side hub client: it dials out to the cloud
// hub over a WebSocket, authenticates with a machine token, and then serves
// control-protocol requests the hub forwards from a user's browser. The dev
// machine dials out (rather than accepting inbound) so it works behind NAT with
// no open port, mirroring tmux-mobile's agent→hub model.
package remote

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/gorilla/websocket"

	"github.com/bazelment/yoloswe/bramble/control"
)

// Config configures the agent client.
type Config struct {
	// Dispatcher serves control requests forwarded by the hub.
	Dispatcher *control.Dispatcher
	// Dialer is injectable for tests (defaults to websocket.DefaultDialer).
	Dialer *websocket.Dialer

	// HubURL is the hub agent endpoint, e.g. "wss://hub.example/agent".
	HubURL string
	// Token authenticates this machine with the hub.
	Token string
	// MachineID uniquely identifies this machine to the hub.
	MachineID string
	// Hostname is a human-friendly label shown in the hub UI.
	Hostname string

	// MinBackoff/MaxBackoff bound reconnect backoff (defaults 1s/30s).
	MinBackoff time.Duration
	MaxBackoff time.Duration
}

// Client maintains a connection to the hub, reconnecting with backoff.
type Client struct {
	cfg Config
}

// New creates an agent client.
func New(cfg Config) *Client {
	if cfg.Dialer == nil {
		cfg.Dialer = websocket.DefaultDialer
	}
	if cfg.MinBackoff == 0 {
		cfg.MinBackoff = time.Second
	}
	if cfg.MaxBackoff == 0 {
		cfg.MaxBackoff = 30 * time.Second
	}
	return &Client{cfg: cfg}
}

// Run dials the hub and serves control requests until ctx is cancelled,
// reconnecting with exponential backoff on disconnect. It returns ctx.Err()
// when ctx is done.
func (c *Client) Run(ctx context.Context) error {
	backoff := c.cfg.MinBackoff
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		err := c.connectAndServe(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			slog.Warn("remote: hub connection ended", "err", err, "retry_in", backoff)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > c.cfg.MaxBackoff {
			backoff = c.cfg.MaxBackoff
		}
	}
}

// connectAndServe performs one connection lifecycle: dial, handshake, serve.
// On a successful handshake it resets nothing itself; the caller handles
// backoff. Returns nil when the connection closes cleanly.
func (c *Client) connectAndServe(ctx context.Context) error {
	ws, _, err := c.cfg.Dialer.DialContext(ctx, c.cfg.HubURL, http.Header{})
	if err != nil {
		return fmt.Errorf("remote: dial %s: %w", c.cfg.HubURL, err)
	}
	conn := control.NewWSConn(ws)
	defer conn.Close()

	if err := c.handshake(conn); err != nil {
		return err
	}
	slog.Info("remote: connected to hub", "hub", c.cfg.HubURL, "machine", c.cfg.MachineID)

	// The agent acts as the control server: the hub forwards browser requests,
	// the agent dispatches them against local tmux and replies/streams back.
	return control.Serve(ctx, conn, c.cfg.Dispatcher)
}

// handshake sends Hello and waits for an accepting HelloAck.
func (c *Client) handshake(conn control.Conn) error {
	hello, err := control.NewRequest(control.TypeHello, "", control.Hello{
		ProtocolVersion: control.ProtocolVersion,
		MachineID:       c.cfg.MachineID,
		Hostname:        c.cfg.Hostname,
		Token:           c.cfg.Token,
	})
	if err != nil {
		return err
	}
	if err := conn.WriteMsg(hello); err != nil {
		return fmt.Errorf("remote: send hello: %w", err)
	}
	ackMsg, err := conn.ReadMsg()
	if err != nil {
		return fmt.Errorf("remote: read hello ack: %w", err)
	}
	if ackMsg.Type != control.TypeHelloAck {
		return fmt.Errorf("remote: expected hello_ack, got %q", ackMsg.Type)
	}
	var ack control.HelloAck
	if err := ackMsg.DecodePayload(&ack); err != nil {
		return err
	}
	if !ack.OK {
		return fmt.Errorf("remote: hub rejected connection: %s", ack.Error)
	}
	return nil
}
