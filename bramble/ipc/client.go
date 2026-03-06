package ipc

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
)

// Client connects to a bramble IPC server via Unix domain socket.
type Client struct {
	socketPath string
}

// NewClient creates a client that connects to the given socket path.
func NewClient(socketPath string) *Client {
	return &Client{socketPath: socketPath}
}

// NewClientFromEnv creates a client using the BRAMBLE_SOCK environment variable.
// Returns an error if the variable is not set.
func NewClientFromEnv() (*Client, error) {
	socketPath := os.Getenv(SockEnvVar)
	if socketPath == "" {
		return nil, fmt.Errorf("$%s is not set — are you running inside a bramble session?", SockEnvVar)
	}
	return NewClient(socketPath), nil
}

// Send sends a request and returns the response. Each call opens a new connection.
func (c *Client) Send(req *Request) (*Response, error) {
	conn, err := net.Dial("unix", c.socketPath)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to bramble at %s: %w", c.socketPath, err)
	}
	defer conn.Close()

	enc := json.NewEncoder(conn)
	if err := enc.Encode(req); err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}

	var resp Response
	dec := json.NewDecoder(conn)
	if err := dec.Decode(&resp); err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	return &resp, nil
}

// Ping sends a ping request to verify the server is alive.
func (c *Client) Ping() error {
	resp, err := c.Send(&Request{Type: RequestPing, ID: "ping"})
	if err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("ping failed: %s", resp.Error)
	}
	return nil
}
