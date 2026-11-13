package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
)

type streamKey struct{}

//nolint:govet
type chatStream struct {
	mu      sync.Mutex
	emit    func(string)
	emitted bool
}

func (s *chatStream) text(text string) {
	if text == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.emitted = true
	s.emit(text)
}
func (s *chatStream) complete(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.emitted {
		s.emit(text)
	}
	s.emitted = false
}

type chatTransport struct{ base http.RoundTripper }

func (t chatTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	stream, _ := req.Context().Value(streamKey{}).(*chatStream)
	if stream == nil {
		return t.base.RoundTrip(req)
	}
	anthropic := strings.HasSuffix(req.URL.Path, "/messages")
	if anthropic {
		raw, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		_ = req.Body.Close()
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			return nil, err
		}
		body["stream"] = true
		raw, err = json.Marshal(body)
		if err != nil {
			return nil, err
		}
		req = req.Clone(req.Context())
		req.Body = io.NopCloser(bytes.NewReader(raw))
		req.ContentLength = int64(len(raw))
	}
	res, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode/100 != 2 || !strings.Contains(res.Header.Get("Content-Type"), "text/event-stream") {
		return res, nil
	}
	if anthropic {
		defer res.Body.Close()
		raw, err := readAnthropicChatStream(res.Body, stream)
		if err != nil {
			return nil, err
		}
		res.Body = io.NopCloser(bytes.NewReader(raw))
		res.ContentLength = int64(len(raw))
		res.Header.Set("Content-Type", "application/json")
		return res, nil
	}
	res.Body = &openAIChatBody{ReadCloser: res.Body, stream: stream}
	return res, nil
}

//nolint:govet
type openAIChatBody struct {
	io.ReadCloser
	stream  *chatStream
	pending []byte
}

func (b *openAIChatBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.pending = append(b.pending, p[:n]...)
	for {
		idx := bytes.IndexByte(b.pending, '\n')
		if idx < 0 {
			break
		}
		line := b.pending[:idx]
		var event struct{ Type, Delta string }
		if json.Unmarshal(bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:"))), &event) == nil && event.Type == "response.output_text.delta" {
			b.stream.text(event.Delta)
		}
		b.pending = b.pending[idx+1:]
	}
	return n, err
}
func readAnthropicChatStream(r io.Reader, stream *chatStream) ([]byte, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 4096), 4<<20)
	var message map[string]any
	blocks := []map[string]any{}
	inputs := map[int]string{}
	for scanner.Scan() {
		line := scanner.Bytes()
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		//nolint:govet // event fields follow the wire protocol
		var event struct {
			Type         string
			Index        int
			Message      map[string]any
			ContentBlock map[string]any `json:"content_block"`
			Delta        map[string]any
			Usage        map[string]any
		}
		if err := json.Unmarshal(bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:"))), &event); err != nil {
			return nil, err
		}
		switch event.Type {
		case "message_start":
			message = event.Message
		case "content_block_start":
			if event.Index != len(blocks) {
				return nil, fmt.Errorf("invalid streamed content index %d", event.Index)
			}
			blocks = append(blocks, event.ContentBlock)
		case "content_block_delta":
			if event.Index < 0 || event.Index >= len(blocks) {
				return nil, fmt.Errorf("invalid streamed delta index %d", event.Index)
			}
			if text, ok := event.Delta["text"].(string); ok {
				previous, _ := blocks[event.Index]["text"].(string)
				blocks[event.Index]["text"] = previous + text
				stream.text(text)
			}
			if partial, ok := event.Delta["partial_json"].(string); ok {
				inputs[event.Index] += partial
			}
		case "message_delta":
			if message == nil {
				return nil, fmt.Errorf("stream missing message_start")
			}
			for k, v := range event.Delta {
				message[k] = v
			}
			usage, _ := message["usage"].(map[string]any)
			if usage == nil {
				usage = map[string]any{}
			}
			for k, v := range event.Usage {
				usage[k] = v
			}
			message["usage"] = usage
		case "message_stop":
			if message == nil {
				return nil, fmt.Errorf("stream missing message_start")
			}
			for i, input := range inputs {
				var value any
				if err := json.Unmarshal([]byte(input), &value); err != nil {
					return nil, err
				}
				blocks[i]["input"] = value
			}
			message["content"] = blocks
			return json.Marshal(message)
		case "error":
			return nil, fmt.Errorf("concierge stream failed")
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("concierge stream ended without message_stop")
}
func withChatStream(ctx context.Context, emit func(string)) (context.Context, *chatStream) {
	s := &chatStream{emit: emit}
	return context.WithValue(ctx, streamKey{}, s), s
}
