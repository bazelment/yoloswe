package ipc

import (
	"context"
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

	// Start first server
	srv1 := NewServer(sockPath)
	srv1.Handle(RequestPing, func(_ context.Context, _ *Request) (any, error) {
		return "pong1", nil
	})
	require.NoError(t, srv1.Start())
	require.NoError(t, srv1.Close())

	// Start second server on the same path (should remove stale socket)
	srv2 := NewServer(sockPath)
	srv2.Handle(RequestPing, func(_ context.Context, _ *Request) (any, error) {
		return "pong2", nil
	})
	require.NoError(t, srv2.Start())
	defer srv2.Close()

	client := NewClient(sockPath)
	require.NoError(t, client.Ping())
}
