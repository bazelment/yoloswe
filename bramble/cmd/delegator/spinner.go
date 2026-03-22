package delegator

import (
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// Spinner displays a braille animation on stderr while the LLM is thinking.
// It is safe for concurrent use; Start and Stop are idempotent.
type Spinner struct {
	writer  io.Writer
	stopCh  chan struct{}
	doneCh  chan struct{}
	message string
	frames  []string
	mu      sync.Mutex
	active  bool
}

// NewSpinner creates a spinner that writes to the given writer.
// If w is nil, it defaults to os.Stderr.
// The spinner is a no-op if the writer is not a terminal.
func NewSpinner(w io.Writer) *Spinner {
	if w == nil {
		w = os.Stderr
	}
	return &Spinner{
		writer: w,
		frames: []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"},
	}
}

// Start begins the spinner animation with the given message.
// If already active, this is a no-op.
func (s *Spinner) Start(message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active {
		return
	}
	// Only animate if the writer is a terminal.
	if !isWriterTerminal(s.writer) {
		return
	}
	s.active = true
	s.message = message
	s.stopCh = make(chan struct{})
	s.doneCh = make(chan struct{})

	go func() {
		defer close(s.doneCh)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		i := 0
		for {
			select {
			case <-s.stopCh:
				// Clear the spinner line.
				fmt.Fprintf(s.writer, "\r\033[K")
				return
			case <-ticker.C:
				frame := s.frames[i%len(s.frames)]
				fmt.Fprintf(s.writer, "\r%s %s", frame, s.message)
				i++
			}
		}
	}()
}

// Stop halts the spinner and clears the line. Safe to call when not active.
func (s *Spinner) Stop() {
	s.mu.Lock()
	if !s.active {
		s.mu.Unlock()
		return
	}
	s.active = false
	close(s.stopCh)
	doneCh := s.doneCh
	s.mu.Unlock()
	<-doneCh
}

// isWriterTerminal checks if w is backed by a terminal file descriptor.
func isWriterTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	stat, err := f.Stat()
	if err != nil {
		return false
	}
	return (stat.Mode() & os.ModeCharDevice) != 0
}
