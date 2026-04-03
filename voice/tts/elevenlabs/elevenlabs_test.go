package elevenlabs_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/voice/tts"
	"github.com/bazelment/yoloswe/voice/tts/elevenlabs"
)

func TestSynthesize_Success(t *testing.T) {
	t.Parallel()

	fakeAudio := []byte("fake-mp3-data")
	var receivedBody map[string]interface{}
	var receivedVoice string
	var receivedAPIKey string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAPIKey = r.Header.Get("xi-api-key")

		// Extract voice ID from URL: /v1/text-to-speech/{voice_id}
		parts := splitPath(r.URL.Path)
		if len(parts) >= 3 {
			receivedVoice = parts[2]
		}

		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(body, &receivedBody))

		w.Header().Set("Content-Type", "audio/mpeg")
		w.Write(fakeAudio)
	}))
	defer server.Close()

	provider, err := elevenlabs.NewProvider("test-api-key", elevenlabs.WithBaseURL(server.URL))
	require.NoError(t, err)

	audio, err := provider.Synthesize(context.Background(), "Hello world", tts.SynthOpts{
		Voice: "custom-voice-id",
		Speed: 1.2,
	})
	require.NoError(t, err)

	assert.Equal(t, fakeAudio, audio.Data)
	assert.Equal(t, tts.AudioFormatMP3, audio.Format)
	assert.Equal(t, "test-api-key", receivedAPIKey)
	assert.Equal(t, "custom-voice-id", receivedVoice)
	assert.Equal(t, "Hello world", receivedBody["text"])
}

func TestSynthesize_DefaultVoice(t *testing.T) {
	t.Parallel()

	var receivedVoice string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := splitPath(r.URL.Path)
		if len(parts) >= 3 {
			receivedVoice = parts[2]
		}
		w.Write([]byte("audio"))
	}))
	defer server.Close()

	provider, err := elevenlabs.NewProvider("test-key", elevenlabs.WithBaseURL(server.URL))
	require.NoError(t, err)

	_, err = provider.Synthesize(context.Background(), "test", tts.SynthOpts{})
	require.NoError(t, err)

	// Should use the default voice ID when none specified.
	assert.NotEmpty(t, receivedVoice)
}

func TestSynthesize_RateLimit(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	provider, err := elevenlabs.NewProvider("test-key", elevenlabs.WithBaseURL(server.URL))
	require.NoError(t, err)

	_, err = provider.Synthesize(context.Background(), "test", tts.SynthOpts{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "rate limited")
}

func TestSynthesize_Unauthorized(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	provider, err := elevenlabs.NewProvider("bad-key", elevenlabs.WithBaseURL(server.URL))
	require.NoError(t, err)

	_, err = provider.Synthesize(context.Background(), "test", tts.SynthOpts{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unauthorized")
}

func TestSynthesize_ServerError(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("internal server error"))
	}))
	defer server.Close()

	provider, err := elevenlabs.NewProvider("test-key", elevenlabs.WithBaseURL(server.URL))
	require.NoError(t, err)

	_, err = provider.Synthesize(context.Background(), "test", tts.SynthOpts{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "HTTP 500")
}

func TestNewProvider_MissingAPIKey(t *testing.T) {
	// Cannot use t.Parallel() with t.Setenv.
	t.Setenv("ELEVENLABS_API_KEY", "")

	_, err := elevenlabs.NewProvider("")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "API key required")
}

func TestSynthesize_ContextCancelled(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Simulate slow response — context should cancel before this returns.
		<-r.Context().Done()
	}))
	defer server.Close()

	provider, err := elevenlabs.NewProvider("test-key", elevenlabs.WithBaseURL(server.URL))
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately.

	_, err = provider.Synthesize(ctx, "test", tts.SynthOpts{})
	require.Error(t, err)
}

// splitPath splits a URL path into segments, filtering out empty strings.
func splitPath(path string) []string {
	var parts []string
	for _, p := range split(path, '/') {
		if p != "" {
			parts = append(parts, p)
		}
	}
	return parts
}

func split(s string, sep byte) []string {
	var result []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == sep {
			result = append(result, s[start:i])
			start = i + 1
		}
	}
	result = append(result, s[start:])
	return result
}
