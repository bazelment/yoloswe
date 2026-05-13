package main

import (
	"bytes"
	"errors"
	"testing"

	"github.com/bazelment/yoloswe/voice/stt"
)

func TestRenderEvent(t *testing.T) {
	t.Parallel()

	requireRenderedEvent(t, stt.Event{Type: stt.EventSpeechStart}, "[speech-start]\n", "")
	requireRenderedEvent(t, stt.Event{Type: stt.EventPartialText, Text: "hello"}, "[partial] \"hello\"\n", "")
	requireRenderedEvent(t, stt.Event{Type: stt.EventFinalText, Text: "done", Confidence: 0.875}, "[final] \"done\" (confidence: 0.88)\n", "")
	requireRenderedEvent(t, stt.Event{Type: stt.EventSpeechEnd}, "[speech-end]\n", "")
	requireRenderedEvent(t, stt.Event{Type: stt.EventError, Error: errors.New("boom")}, "", "[error] boom\n")
}

func TestRenderEventIgnoresUnknownEvent(t *testing.T) {
	t.Parallel()

	requireRenderedEvent(t, stt.Event{Type: stt.EventType(99), Text: "ignored"}, "", "")
}

func requireRenderedEvent(t *testing.T, evt stt.Event, wantOut, wantErr string) {
	t.Helper()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	renderEvent(&stdout, &stderr, evt)

	if got := stdout.String(); got != wantOut {
		t.Fatalf("stdout for %s = %q, want %q", evt.Type, got, wantOut)
	}
	if got := stderr.String(); got != wantErr {
		t.Fatalf("stderr for %s = %q, want %q", evt.Type, got, wantErr)
	}
}
