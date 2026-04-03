// Package elevenlabs implements the tts.TextToSpeech interface using the
// ElevenLabs HTTP API.
package elevenlabs

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/bazelment/yoloswe/voice/tts"
)

const (
	defaultBaseURL = "https://api.elevenlabs.io"
	defaultVoice   = "21m00Tcm4TlvDq8ikWAM" // "Rachel" — default ElevenLabs voice
	apiVersion     = "v1"
)

// Provider implements tts.TextToSpeech using the ElevenLabs API.
type Provider struct {
	client  *http.Client
	apiKey  string
	baseURL string
}

// Option configures a Provider.
type Option func(*Provider)

// WithBaseURL overrides the API base URL (useful for testing).
func WithBaseURL(url string) Option {
	return func(p *Provider) { p.baseURL = url }
}

// WithHTTPClient overrides the HTTP client.
func WithHTTPClient(c *http.Client) Option {
	return func(p *Provider) { p.client = c }
}

// NewProvider creates an ElevenLabs TTS provider.
// If apiKey is empty, it reads ELEVENLABS_API_KEY from the environment.
func NewProvider(apiKey string, opts ...Option) (*Provider, error) {
	if apiKey == "" {
		apiKey = os.Getenv("ELEVENLABS_API_KEY")
	}
	if apiKey == "" {
		return nil, fmt.Errorf("elevenlabs: API key required (set ELEVENLABS_API_KEY or pass explicitly)")
	}
	p := &Provider{
		apiKey:  apiKey,
		baseURL: defaultBaseURL,
		client:  http.DefaultClient,
	}
	for _, opt := range opts {
		opt(p)
	}
	return p, nil
}

// requestBody is the JSON body for the ElevenLabs text-to-speech endpoint.
type requestBody struct {
	Text          string        `json:"text"`
	ModelID       string        `json:"model_id"`
	VoiceSettings voiceSettings `json:"voice_settings"`
}

type voiceSettings struct {
	Stability       float64 `json:"stability"`
	SimilarityBoost float64 `json:"similarity_boost"`
	Speed           float64 `json:"speed,omitempty"`
}

// Synthesize converts text to speech audio via the ElevenLabs API.
func (p *Provider) Synthesize(ctx context.Context, text string, opts tts.SynthOpts) (*tts.Audio, error) {
	voice := opts.Voice
	if voice == "" {
		voice = defaultVoice
	}

	speed := opts.Speed
	if speed == 0 {
		speed = 1.0
	}

	body := requestBody{
		Text:    text,
		ModelID: "eleven_multilingual_v2",
		VoiceSettings: voiceSettings{
			Stability:       0.5,
			SimilarityBoost: 0.75,
			Speed:           speed,
		},
	}

	bodyJSON, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("elevenlabs: marshal request: %w", err)
	}

	url := fmt.Sprintf("%s/%s/text-to-speech/%s", p.baseURL, apiVersion, voice)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(bodyJSON)))
	if err != nil {
		return nil, fmt.Errorf("elevenlabs: create request: %w", err)
	}
	req.Header.Set("xi-api-key", p.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "audio/mpeg")

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("elevenlabs: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("elevenlabs: rate limited (HTTP 429)")
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("elevenlabs: unauthorized — check API key")
	}
	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("elevenlabs: HTTP %d: %s", resp.StatusCode, string(errBody))
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("elevenlabs: read response: %w", err)
	}

	return &tts.Audio{
		Data:   data,
		Format: tts.AudioFormatMP3,
	}, nil
}
