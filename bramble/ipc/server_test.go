package ipc

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPingRoundTrip(t *testing.T) {
	t.Parallel()
	sockPath := filepath.Join(t.TempDir(), "test.sock")

	srv := NewServer(sockPath)
	srv.Handle(RequestPing, func(_ context.Context, _ *Request) (any, error) {
		return "pong", nil
	})
	require.NoError(t, srv.Start())
	defer srv.Close()

	client := NewClient(sockPath)
	require.NoError(t, client.Ping())
}

func TestNewSessionRoundTrip(t *testing.T) {
	t.Parallel()
	sockPath := filepath.Join(t.TempDir(), "test.sock")

	srv := NewServer(sockPath)
	srv.Handle(RequestNewSession, func(_ context.Context, req *Request) (any, error) {
		params := req.Params.(*NewSessionParams)
		return &NewSessionResult{
			SessionID:    "test-session-123",
			WorktreePath: "/tmp/wt/" + params.Branch,
		}, nil
	})
	require.NoError(t, srv.Start())
	defer srv.Close()

	client := NewClient(sockPath)
	resp, err := client.Send(&Request{
		Type: RequestNewSession,
		ID:   "req-1",
		Params: &NewSessionParams{
			SessionType:    "planner",
			Branch:         "feature/foo",
			CreateWorktree: true,
			Prompt:         "implement OAuth",
		},
	})
	require.NoError(t, err)
	require.True(t, resp.OK, "expected OK, got error: %s", resp.Error)
	require.Equal(t, "req-1", resp.ID)
}

func TestListSessionsRoundTrip(t *testing.T) {
	t.Parallel()
	sockPath := filepath.Join(t.TempDir(), "test.sock")

	srv := NewServer(sockPath)
	srv.Handle(RequestListSessions, func(_ context.Context, _ *Request) (any, error) {
		return &ListSessionsResult{
			Sessions: []SessionSummary{
				{ID: "s1", Type: "planner", Status: "running", WorktreeName: "main"},
			},
		}, nil
	})
	require.NoError(t, srv.Start())
	defer srv.Close()

	client := NewClient(sockPath)
	resp, err := client.Send(&Request{Type: RequestListSessions, ID: "req-2"})
	require.NoError(t, err)
	require.True(t, resp.OK)
}

func TestUnknownRequestType(t *testing.T) {
	t.Parallel()
	sockPath := filepath.Join(t.TempDir(), "test.sock")

	srv := NewServer(sockPath)
	require.NoError(t, srv.Start())
	defer srv.Close()

	client := NewClient(sockPath)
	resp, err := client.Send(&Request{Type: "bogus", ID: "req-3"})
	require.NoError(t, err)
	require.False(t, resp.OK)
	require.Contains(t, resp.Error, "unknown request type")
}

func TestClientFromEnvNotSet(t *testing.T) {
	// Cannot use t.Parallel() with t.Setenv()
	t.Setenv(SockEnvVar, "")
	_, err := NewClientFromEnv()
	require.Error(t, err)
	require.Contains(t, err.Error(), SockEnvVar)
}

func TestStaleSocketRemoved(t *testing.T) {
	t.Parallel()
	sockPath := filepath.Join(t.TempDir(), "test.sock")

	// Create a stale file at the socket path before any server starts.
	require.NoError(t, os.WriteFile(sockPath, []byte("stale"), 0o600))

	// Server should remove the stale file and start successfully.
	srv := NewServer(sockPath)
	srv.Handle(RequestPing, func(_ context.Context, _ *Request) (any, error) {
		return "pong", nil
	})
	require.NoError(t, srv.Start())
	defer srv.Close()

	client := NewClient(sockPath)
	require.NoError(t, client.Ping())
}

func TestNotifyRoundTrip(t *testing.T) {
	t.Parallel()
	sockPath := filepath.Join(t.TempDir(), "test.sock")

	var receivedSessionID string
	srv := NewServer(sockPath)
	srv.Handle(RequestNotify, func(_ context.Context, req *Request) (any, error) {
		params, ok := req.Params.(*NotifyParams)
		if !ok {
			return nil, fmt.Errorf("invalid params type")
		}
		receivedSessionID = params.SessionID
		return "ok", nil
	})
	require.NoError(t, srv.Start())
	defer srv.Close()

	client := NewClient(sockPath)
	resp, err := client.Send(&Request{
		Type:   RequestNotify,
		ID:     "req-notify",
		Params: &NotifyParams{SessionID: "main-builder-abc123"},
	})
	require.NoError(t, err)
	require.True(t, resp.OK, "expected OK, got error: %s", resp.Error)
	require.Equal(t, "req-notify", resp.ID)
	require.Equal(t, "main-builder-abc123", receivedSessionID)
}

func TestHandlerError(t *testing.T) {
	t.Parallel()
	sockPath := filepath.Join(t.TempDir(), "test.sock")

	srv := NewServer(sockPath)
	srv.Handle(RequestNewSession, func(_ context.Context, _ *Request) (any, error) {
		return nil, fmt.Errorf("worktree not found")
	})
	require.NoError(t, srv.Start())
	defer srv.Close()

	client := NewClient(sockPath)
	resp, err := client.Send(&Request{
		Type:   RequestNewSession,
		ID:     "req-err",
		Params: &NewSessionParams{Prompt: "test"},
	})
	require.NoError(t, err)
	require.False(t, resp.OK)
	require.Equal(t, "req-err", resp.ID)
	require.Contains(t, resp.Error, "worktree not found")
}
