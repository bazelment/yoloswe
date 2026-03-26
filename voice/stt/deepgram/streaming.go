// Package deepgram implements the stt interfaces using the Deepgram API.
package deepgram

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	dginterfaces "github.com/deepgram/deepgram-go-sdk/v3/pkg/api/listen/v1/websocket/interfaces"
	dgclientifs "github.com/deepgram/deepgram-go-sdk/v3/pkg/client/interfaces"
	dgclient "github.com/deepgram/deepgram-go-sdk/v3/pkg/client/listen/v1/websocket"

	"github.com/bazelment/yoloswe/voice/stt"
)

// StreamingProvider implements stt.StreamingTranscriber using Deepgram's
// WebSocket-based live transcription API.
type StreamingProvider struct {
	apiKey string
	host   string // custom host for testing; empty means default Deepgram API
}

// NewStreamingProvider creates a Deepgram streaming provider.
// If apiKey is empty, the SDK reads DEEPGRAM_API_KEY from the environment.
func NewStreamingProvider(apiKey string) *StreamingProvider {
	return &StreamingProvider{apiKey: apiKey}
}

// NewStreamingProviderWithHost creates a Deepgram streaming provider that
// connects to a custom host instead of the default Deepgram API.
// This is primarily useful for testing with a mock WebSocket server.
func NewStreamingProviderWithHost(apiKey, host string) *StreamingProvider {
	return &StreamingProvider{apiKey: apiKey, host: host}
}

// StartSession connects to Deepgram and returns a streaming Session.
func (p *StreamingProvider) StartSession(ctx context.Context, cfg stt.AudioConfig, opts stt.StreamOpts) (stt.Session, error) {
	s := &session{
		events:          make(chan stt.Event, 64),
		done:            make(chan struct{}),
		livenessTimeout: 10 * time.Second,
		lastSendAt:      &atomic.Int64{},
		lastEventAt:     &atomic.Int64{},
	}
	s.lastEventAt.Store(time.Now().UnixMilli())

	model := opts.Model
	if model == "" {
		model = "nova-3"
	}

	tOptions := &dgclientifs.LiveTranscriptionOptions{
		Model:      model,
		Punctuate:  true,
		Encoding:   "linear16",
		Channels:   cfg.Channels,
		SampleRate: cfg.SampleRate,
	}
	if opts.Language != "" {
		tOptions.Language = opts.Language
	}
	if opts.VAD {
		tOptions.InterimResults = true
		tOptions.UtteranceEndMs = "1000"
		tOptions.VadEvents = true
	}
	if opts.Interim {
		tOptions.InterimResults = true
	}

	cOptions := &dgclientifs.ClientOptions{
		APIKey: p.apiKey,
	}
	if p.host != "" {
		cOptions.Host = p.host
	}

	// The callback adapter bridges Deepgram's LiveMessageCallback to our session.
	// This avoids method signature conflicts between stt.Session.Close() and
	// LiveMessageCallback.Close(*CloseResponse).
	cb := &callbackAdapter{session: s}

	var err error
	s.ws, err = dgclient.NewUsingCallback(ctx, "", cOptions, tOptions, cb)
	if err != nil {
		return nil, fmt.Errorf("deepgram: create client: %w", err)
	}

	if !s.ws.Connect() {
		return nil, fmt.Errorf("deepgram: failed to connect")
	}

	// Start liveness watchdog.
	s.ctx, s.cancel = context.WithCancel(ctx)
	go s.livenessWatchdog()

	return s, nil
}

// session implements stt.Session backed by a Deepgram WebSocket connection.
type session struct {
	ws              *dgclient.WSCallback
	events          chan stt.Event
	done            chan struct{}
	ctx             context.Context
	cancel          context.CancelFunc
	lastSendAt      *atomic.Int64 // unix millis of last SendAudio call; 0 means no audio sent
	lastEventAt     *atomic.Int64
	livenessTimeout time.Duration
	closeOnce       sync.Once
	eventsClosed    atomic.Bool
}

func (s *session) SendAudio(data []byte) error {
	s.lastSendAt.Store(time.Now().UnixMilli())
	_, err := s.ws.Write(data)
	if err != nil {
		return fmt.Errorf("deepgram: send audio: %w", err)
	}
	return nil
}

func (s *session) Events() <-chan stt.Event {
	return s.events
}

func (s *session) Close() error {
	s.closeOnce.Do(func() {
		s.cancel()
		s.ws.Finalize()
		s.ws.Finish()
		close(s.done)
		s.closeEvents()
	})
	return nil
}

func (s *session) closeEvents() {
	if s.eventsClosed.CompareAndSwap(false, true) {
		close(s.events)
	}
}

func (s *session) livenessWatchdog() {
	ticker := time.NewTicker(s.livenessTimeout / 2)
	defer ticker.Stop()

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-s.done:
			return
		case <-ticker.C:
			lastSend := s.lastSendAt.Load()
			if lastSend == 0 || time.Since(time.UnixMilli(lastSend)) > s.livenessTimeout {
				continue
			}
			lastEvt := time.UnixMilli(s.lastEventAt.Load())
			if time.Since(lastEvt) > s.livenessTimeout {
				s.emitEvent(stt.Event{
					Type:      stt.EventError,
					Error:     fmt.Errorf("liveness timeout: no events for %v while sending audio", s.livenessTimeout),
					Timestamp: time.Now(),
				})
			}
		}
	}
}

func (s *session) emitEvent(evt stt.Event) {
	if s.eventsClosed.Load() {
		return
	}
	s.lastEventAt.Store(time.Now().UnixMilli())
	select {
	case s.events <- evt:
	case <-s.done:
	case <-s.ctx.Done():
	}
}

// callbackAdapter implements dginterfaces.LiveMessageCallback and forwards
// events to the session. This is a separate type to avoid method signature
// conflicts between stt.Session and LiveMessageCallback.
type callbackAdapter struct {
	session *session
}

func (c *callbackAdapter) Open(_ *dginterfaces.OpenResponse) error {
	return nil
}

func (c *callbackAdapter) Message(mr *dginterfaces.MessageResponse) error {
	if mr == nil || len(mr.Channel.Alternatives) == 0 {
		return nil
	}

	alt := mr.Channel.Alternatives[0]
	text := alt.Transcript
	if text == "" {
		return nil
	}

	raw, _ := json.Marshal(mr)

	evtType := stt.EventPartialText
	isFinal := false
	if mr.IsFinal {
		evtType = stt.EventFinalText
		isFinal = true
	}

	c.session.emitEvent(stt.Event{
		Type:       evtType,
		Text:       text,
		Confidence: alt.Confidence,
		IsFinal:    isFinal,
		Timestamp:  time.Now(),
		Raw:        raw,
	})
	return nil
}

func (c *callbackAdapter) SpeechStarted(ssr *dginterfaces.SpeechStartedResponse) error {
	raw, _ := json.Marshal(ssr)
	c.session.emitEvent(stt.Event{
		Type:      stt.EventSpeechStart,
		Timestamp: time.Now(),
		Raw:       raw,
	})
	return nil
}

func (c *callbackAdapter) UtteranceEnd(ur *dginterfaces.UtteranceEndResponse) error {
	raw, _ := json.Marshal(ur)
	c.session.emitEvent(stt.Event{
		Type:      stt.EventSpeechEnd,
		Timestamp: time.Now(),
		Raw:       raw,
	})
	return nil
}

func (c *callbackAdapter) Metadata(_ *dginterfaces.MetadataResponse) error {
	c.session.lastEventAt.Store(time.Now().UnixMilli())
	return nil
}

func (c *callbackAdapter) Close(_ *dginterfaces.CloseResponse) error {
	c.session.closeEvents()
	return nil
}

func (c *callbackAdapter) Error(er *dginterfaces.ErrorResponse) error {
	var errMsg string
	if er != nil {
		errMsg = er.Description
	}
	c.session.emitEvent(stt.Event{
		Type:      stt.EventError,
		Error:     fmt.Errorf("deepgram error: %s", errMsg),
		Timestamp: time.Now(),
	})
	return nil
}

func (c *callbackAdapter) UnhandledEvent(_ []byte) error {
	return nil
}
