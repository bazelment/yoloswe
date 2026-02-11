package remote

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

const tokenBytes = 32 // 32 bytes = 64 hex characters

// GenerateToken generates a cryptographically random token as a 64-character hex string.
func GenerateToken() (string, error) {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// validateToken extracts the Bearer token from gRPC metadata and validates it
// against the expected token using constant-time comparison.
func validateToken(ctx context.Context, expected string) error {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return status.Error(codes.Unauthenticated, "missing metadata")
	}

	values := md.Get("authorization")
	if len(values) == 0 {
		return status.Error(codes.Unauthenticated, "missing authorization token")
	}

	token := values[0]
	const bearerPrefix = "Bearer "
	if !strings.HasPrefix(token, bearerPrefix) {
		return status.Error(codes.Unauthenticated, "invalid authorization format")
	}
	token = token[len(bearerPrefix):]

	if subtle.ConstantTimeCompare([]byte(token), []byte(expected)) != 1 {
		return status.Error(codes.Unauthenticated, "invalid token")
	}

	return nil
}

// TokenAuthInterceptor returns a gRPC unary server interceptor that validates
// a Bearer token on every unary RPC call.
func TokenAuthInterceptor(token string) grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req interface{},
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (interface{}, error) {
		if err := validateToken(ctx, token); err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

// TokenStreamInterceptor returns a gRPC stream server interceptor that validates
// a Bearer token on every streaming RPC call.
func TokenStreamInterceptor(token string) grpc.StreamServerInterceptor {
	return func(
		srv interface{},
		ss grpc.ServerStream,
		info *grpc.StreamServerInfo,
		handler grpc.StreamHandler,
	) error {
		if err := validateToken(ss.Context(), token); err != nil {
			return err
		}
		return handler(srv, ss)
	}
}

// TokenCreds implements credentials.PerRPCCredentials to attach
// a Bearer token as metadata on every outgoing gRPC call.
type TokenCreds struct {
	token string
}

// TokenCallCredentials returns a gRPC PerRPCCredentials that attaches the given
// token as a Bearer authorization header on every RPC call.
func TokenCallCredentials(token string) *TokenCreds {
	return &TokenCreds{token: token}
}

// GetRequestMetadata returns the authorization metadata for each RPC.
func (t *TokenCreds) GetRequestMetadata(_ context.Context, _ ...string) (map[string]string, error) {
	return map[string]string{
		"authorization": "Bearer " + t.token,
	}, nil
}

// RequireTransportSecurity returns false because we don't require TLS for this dev tool.
func (t *TokenCreds) RequireTransportSecurity() bool {
	return false
}
