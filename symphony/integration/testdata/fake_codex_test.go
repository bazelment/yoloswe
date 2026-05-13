package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestServeFakeCodexWritesProtocolResponses(t *testing.T) {
	t.Parallel()

	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize"}`,
		`{"jsonrpc":"2.0","method":"initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"thread/start"}`,
		`{"jsonrpc":"2.0","id":3,"method":"turn/start"}`,
		`{"jsonrpc":"2.0","id":99,"result":{"ok":true}}`,
		`{"jsonrpc":"2.0","id":4,"method":"unknown/method"}`,
		`not-json`,
	}, "\n")
	var out bytes.Buffer
	var sleeps []time.Duration

	err := serveFakeCodex(strings.NewReader(input), &out, false, func(delay time.Duration) {
		sleeps = append(sleeps, delay)
	})

	require.NoError(t, err)
	require.Equal(t, []time.Duration{100 * time.Millisecond}, sleeps)

	lines := outputLines(out.String())
	require.Len(t, lines, 6)
	require.JSONEq(t, `{"jsonrpc":"2.0","id":1,"result":{"capabilities":{}}}`, lines[0])
	require.JSONEq(t, `{"jsonrpc":"2.0","id":2,"result":{"thread":{"id":"thread-fake-001"}}}`, lines[1])
	require.JSONEq(t, `{"jsonrpc":"2.0","id":3,"result":{"turn":{"id":"turn-fake-001"}}}`, lines[2])
	require.JSONEq(t, `{"jsonrpc":"2.0","method":"thread/tokenUsage/updated","params":{"input_tokens":100,"output_tokens":50,"total_tokens":150}}`, lines[3])
	require.JSONEq(t, `{"jsonrpc":"2.0","method":"turn/completed","params":{"usage":{"total_token_usage":{"input_tokens":100,"output_tokens":50,"total_tokens":150}}}}`, lines[4])
	require.JSONEq(t, `{"jsonrpc":"2.0","id":4,"error":{"code":-32601,"message":"method not found: unknown/method"}}`, lines[5])
}

func TestServeFakeCodexSlowModeUsesSlowDelays(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	var sleeps []time.Duration

	err := serveFakeCodex(
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"turn/start"}`),
		&out,
		true,
		func(delay time.Duration) {
			sleeps = append(sleeps, delay)
		},
	)

	require.NoError(t, err)
	require.Equal(t, []time.Duration{2 * time.Second, 500 * time.Millisecond}, sleeps)
	require.Len(t, outputLines(out.String()), 3)
}

func outputLines(output string) []string {
	if strings.TrimSpace(output) == "" {
		return nil
	}
	return strings.Split(strings.TrimSpace(output), "\n")
}
