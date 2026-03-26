package stt

import (
	"time"
)

// FakeSession is a Session that replays a canned sequence of events with
// configurable timing. It is intended for use in tests and manual spikes that
// need a working Session without a real STT provider.
//
// Usage:
//
//	sess := stt.NewFakeSession([]stt.Event{
//	    {Type: stt.EventSpeechStart},
//	    {Type: stt.EventPartialText, Text: "hello"},
//	    {Type: stt.EventFinalText, Text: "hello world", IsFinal: true, Confidence: 0.99},
//	    {Type: stt.EventSpeechEnd},
//	}, 150*time.Millisecond)
type FakeSession struct {
	ch       chan Event
	done     chan struct{}
	events   []Event
	interval time.Duration
}

// NewFakeSession creates a FakeSession that emits each event after interval.
// The Events() channel closes after all events are sent or Close() is called.
// Timestamps are set to time.Now() at emission time.
func NewFakeSession(events []Event, interval time.Duration) *FakeSession {
	fs := &FakeSession{
		events:   events,
		interval: interval,
		ch:       make(chan Event, len(events)+1),
		done:     make(chan struct{}),
	}
	go fs.run()
	return fs
}

func (fs *FakeSession) run() {
	defer close(fs.ch)
	ticker := time.NewTicker(fs.interval)
	defer ticker.Stop()
	for _, evt := range fs.events {
		select {
		case <-fs.done:
			return
		case <-ticker.C:
			evt.Timestamp = time.Now()
			fs.ch <- evt
		}
	}
}

// SendAudio is a no-op for FakeSession; the event stream is driven by timing.
func (fs *FakeSession) SendAudio(_ []byte) error { return nil }

// Events returns the channel of pre-programmed events.
func (fs *FakeSession) Events() <-chan Event { return fs.ch }

// Close stops event emission and closes the Events() channel.
func (fs *FakeSession) Close() error {
	select {
	case <-fs.done:
	default:
		close(fs.done)
	}
	return nil
}
