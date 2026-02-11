package remote

import (
	"context"
	"encoding/hex"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/bazelment/yoloswe/bramble/session"

	pb "github.com/bazelment/yoloswe/bramble/remote/proto"
)

// ============================================================================
// Token generation tests
// ============================================================================

func TestGenerateToken_Length(t *testing.T) {
	t.Parallel()
	token, err := GenerateToken()
	require.NoError(t, err)
	assert.Len(t, token, 64, "token should be 64 hex characters (32 bytes)")
}

func TestGenerateToken_ValidHex(t *testing.T) {
	t.Parallel()
	token, err := GenerateToken()
	require.NoError(t, err)

	_, err = hex.DecodeString(token)
	assert.NoError(t, err, "token should be valid hex")
}

func TestGenerateToken_Unique(t *testing.T) {
	t.Parallel()
	token1, err := GenerateToken()
	require.NoError(t, err)
	token2, err := GenerateToken()
	require.NoError(t, err)

	assert.NotEqual(t, token1, token2, "two generated tokens should differ")
}

// ============================================================================
// Interceptor unit tests (via full gRPC round-trip)
// ============================================================================

// setupAuthTestEnv starts a gRPC server with auth interceptors and returns
// a client connection. The caller can choose which credentials to attach.
func setupAuthTestEnv(t *testing.T, serverToken string, clientDialOpts ...grpc.DialOption) (*grpc.ClientConn, func()) {
	t.Helper()

	sessionSvc := newMockSessionService()
	broadcaster := NewEventBroadcaster()

	lis, err := net.Listen("tcp", "localhost:0")
	require.NoError(t, err)

	srv := grpc.NewServer(
		grpc.UnaryInterceptor(TokenAuthInterceptor(serverToken)),
		grpc.StreamInterceptor(TokenStreamInterceptor(serverToken)),
	)
	pb.RegisterBrambleSessionServiceServer(srv, NewSessionServer(sessionSvc, broadcaster))

	go func() {
		_ = srv.Serve(lis)
	}()

	// Always need insecure transport since no TLS
	baseOpts := []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
	allOpts := append(baseOpts, clientDialOpts...)

	conn, err := grpc.NewClient(lis.Addr().String(), allOpts...)
	require.NoError(t, err)

	cleanup := func() {
		conn.Close()
		srv.GracefulStop()
	}
	return conn, cleanup
}

func TestAuth_Unary_MissingToken(t *testing.T) {
	t.Parallel()
	conn, cleanup := setupAuthTestEnv(t, "server-secret")
	defer cleanup()

	client := pb.NewBrambleSessionServiceClient(conn)
	_, err := client.GetAllSessions(context.Background(), &pb.GetAllSessionsRequest{})
	require.Error(t, err)

	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.Unauthenticated, st.Code())
}

func TestAuth_Unary_WrongToken(t *testing.T) {
	t.Parallel()
	conn, cleanup := setupAuthTestEnv(t, "server-secret",
		grpc.WithPerRPCCredentials(TokenCallCredentials("wrong-token")),
	)
	defer cleanup()

	client := pb.NewBrambleSessionServiceClient(conn)
	_, err := client.GetAllSessions(context.Background(), &pb.GetAllSessionsRequest{})
	require.Error(t, err)

	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.Unauthenticated, st.Code())
}

func TestAuth_Unary_CorrectToken(t *testing.T) {
	t.Parallel()
	conn, cleanup := setupAuthTestEnv(t, "server-secret",
		grpc.WithPerRPCCredentials(TokenCallCredentials("server-secret")),
	)
	defer cleanup()

	client := pb.NewBrambleSessionServiceClient(conn)
	resp, err := client.GetAllSessions(context.Background(), &pb.GetAllSessionsRequest{})
	require.NoError(t, err)
	assert.NotNil(t, resp)
}

func TestAuth_Stream_MissingToken(t *testing.T) {
	t.Parallel()
	conn, cleanup := setupAuthTestEnv(t, "server-secret")
	defer cleanup()

	client := pb.NewBrambleSessionServiceClient(conn)
	stream, err := client.StreamEvents(context.Background(), &pb.StreamEventsRequest{})
	// The stream may open without error, but the first Recv should fail
	if err != nil {
		st, ok := status.FromError(err)
		require.True(t, ok)
		assert.Equal(t, codes.Unauthenticated, st.Code())
		return
	}

	_, err = stream.Recv()
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.Unauthenticated, st.Code())
}

func TestAuth_Stream_WrongToken(t *testing.T) {
	t.Parallel()
	conn, cleanup := setupAuthTestEnv(t, "server-secret",
		grpc.WithPerRPCCredentials(TokenCallCredentials("wrong-token")),
	)
	defer cleanup()

	client := pb.NewBrambleSessionServiceClient(conn)
	stream, err := client.StreamEvents(context.Background(), &pb.StreamEventsRequest{})
	if err != nil {
		st, ok := status.FromError(err)
		require.True(t, ok)
		assert.Equal(t, codes.Unauthenticated, st.Code())
		return
	}

	_, err = stream.Recv()
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.Unauthenticated, st.Code())
}

func TestAuth_Stream_CorrectToken(t *testing.T) {
	t.Parallel()
	conn, cleanup := setupAuthTestEnv(t, "server-secret",
		grpc.WithPerRPCCredentials(TokenCallCredentials("server-secret")),
	)
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client := pb.NewBrambleSessionServiceClient(conn)
	stream, err := client.StreamEvents(ctx, &pb.StreamEventsRequest{})
	require.NoError(t, err)

	// Cancel the context so that Recv returns instead of blocking forever
	cancel()
	_, err = stream.Recv()
	// We expect a context canceled error, NOT unauthenticated
	st, ok := status.FromError(err)
	if ok {
		assert.NotEqual(t, codes.Unauthenticated, st.Code())
	}
}

// ============================================================================
// E2E with generated token
// ============================================================================

func TestAuth_E2E_GeneratedToken(t *testing.T) {
	t.Parallel()

	token, err := GenerateToken()
	require.NoError(t, err)

	conn, cleanup := setupAuthTestEnv(t, token,
		grpc.WithPerRPCCredentials(TokenCallCredentials(token)),
	)
	defer cleanup()

	proxy := NewSessionProxy(context.Background(), conn)
	defer proxy.Close()

	_, err = proxy.StartSession(session.SessionTypeBuilder, "/wt/test", "test prompt")
	require.NoError(t, err)
}

func TestAuth_E2E_GeneratedToken_WrongClient(t *testing.T) {
	t.Parallel()

	serverToken, err := GenerateToken()
	require.NoError(t, err)
	clientToken, err := GenerateToken()
	require.NoError(t, err)

	conn, cleanup := setupAuthTestEnv(t, serverToken,
		grpc.WithPerRPCCredentials(TokenCallCredentials(clientToken)),
	)
	defer cleanup()

	client := pb.NewBrambleSessionServiceClient(conn)
	_, err = client.GetAllSessions(context.Background(), &pb.GetAllSessionsRequest{})
	require.Error(t, err)

	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.Unauthenticated, st.Code())
}

// ============================================================================
// TokenCallCredentials tests
// ============================================================================

func TestTokenCallCredentials_GetRequestMetadata(t *testing.T) {
	t.Parallel()
	creds := TokenCallCredentials("my-token")

	md, err := creds.GetRequestMetadata(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "Bearer my-token", md["authorization"])
}

func TestTokenCallCredentials_RequireTransportSecurity(t *testing.T) {
	t.Parallel()
	creds := TokenCallCredentials("my-token")
	assert.False(t, creds.RequireTransportSecurity())
}
