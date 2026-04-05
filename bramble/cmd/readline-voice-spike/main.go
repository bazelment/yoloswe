// Command readline-voice-spike is a manual spike binary that verifies
// ANSI escape sequences for partial voice text coexist with readline's
// terminal control without garbling output.
//
// This spike is the prerequisite for voice/stt Phase 2 integration into the
// delegator's VoiceInputSource. Run it on a real terminal; it will not work
// correctly in a non-TTY environment.
//
// Usage:
//
//	bazel run //bramble/cmd/readline-voice-spike
//
// See README.md for success/failure criteria.
package main

import (
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/ergochat/readline"

	"github.com/bazelment/yoloswe/logging/klogfmt"
)

func main() {
	klogfmt.Init()
	if !isTerminal(os.Stdin) {
		fmt.Fprintln(os.Stderr, "readline-voice-spike: must be run on a real terminal (stdin is not a TTY)")
		os.Exit(1)
	}

	fmt.Fprintln(os.Stderr, "readline-voice-spike: starting — type freely while voice partials appear below the prompt")
	fmt.Fprintln(os.Stderr, "Press Ctrl-C or Ctrl-D to exit early.")

	rl, err := readline.NewEx(&readline.Config{
		Prompt: ">>> ",
		// Match the delegator's config: both streams go to stderr.
		Stdout: os.Stderr,
		Stderr: os.Stderr,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "readline init error: %v\n", err)
		os.Exit(1)
	}
	defer rl.Close()

	vr := &voiceRenderer{rl: rl}

	// Drive a fake STT event stream.
	events := fakeEvents()
	doneCh := make(chan struct{})

	go func() {
		defer close(doneCh)
		vr.run(events)
	}()

	// Accept readline input concurrently while voice events render.
	lineCh := make(chan string)
	go func() {
		defer close(lineCh)
		for {
			line, err := rl.Readline()
			if err != nil {
				return
			}
			lineCh <- line
		}
	}()

	for {
		select {
		case line, ok := <-lineCh:
			if !ok {
				return
			}
			// Print typed lines to stdout (delegator pattern).
			fmt.Printf("you typed: %q\n", line)
		case <-doneCh:
			fmt.Fprintln(os.Stderr, "\nreadline-voice-spike: voice session complete")
			fmt.Fprintln(os.Stderr, "Check above: did the prompt stay stable? Were partials readable?")
			// Drain remaining input lines until Ctrl-D/C.
			for range lineCh {
			}
			return
		}
	}
}

// voiceRenderer renders partial voice text below the readline prompt using
// DECSC/DECRC (save/restore cursor) ANSI sequences. A mutex serializes all
// writes to stderr so readline and the voice goroutine never interleave.
type voiceRenderer struct {
	rl  *readline.Instance
	mu  sync.Mutex
	has bool // true when a partial line is currently displayed
}

// renderPartial writes text below the readline prompt using DECSC/DECRC.
//
// Sequence:
//
//	\0337  — DECSC: save cursor position (terminal stores row, col)
//	\n     — move down one line below the prompt
//	\033[K — erase to end of line (clear previous partial)
//	\033[2m — dim style for partial text
//	text
//	\033[0m — reset style
//	\0338  — DECRC: restore cursor (back where readline left it)
func (vr *voiceRenderer) renderPartial(text string) {
	vr.mu.Lock()
	defer vr.mu.Unlock()
	fmt.Fprintf(os.Stderr, "\0337\n\033[K\033[2m[voice] %s\033[0m\0338", text)
	vr.has = true
}

// clearPartial erases the partial line below the prompt and refreshes readline.
func (vr *voiceRenderer) clearPartial() {
	vr.mu.Lock()
	defer vr.mu.Unlock()
	if vr.has {
		fmt.Fprintf(os.Stderr, "\0337\n\033[K\0338")
		vr.has = false
	}
	vr.rl.Refresh()
}

// run processes a slice of pre-timed events.
func (vr *voiceRenderer) run(events []timedEvent) {
	for _, te := range events {
		time.Sleep(te.delay)
		switch te.typ {
		case evSpeechStart:
			vr.mu.Lock()
			fmt.Fprintf(os.Stderr, "\0337\n\033[K\033[2m[voice] ...\033[0m\0338")
			vr.has = true
			vr.mu.Unlock()
		case evPartial:
			vr.renderPartial(te.text)
		case evFinal:
			vr.clearPartial()
			// Print the final transcription above the prompt (like committed text).
			vr.mu.Lock()
			fmt.Fprintf(os.Stderr, "\r\033[K[voice final] %q\n", te.text)
			vr.mu.Unlock()
			vr.rl.Refresh()
		case evSpeechEnd:
			vr.clearPartial()
		}
	}
}

type eventKind int

const (
	evSpeechStart eventKind = iota
	evPartial
	evFinal
	evSpeechEnd
)

type timedEvent struct {
	text  string
	delay time.Duration
	typ   eventKind
}

// fakeEvents returns two utterances with realistic timing to stress-test the
// DECSC/DECRC approach across multiple speech-start/end cycles.
func fakeEvents() []timedEvent {
	ev := func(typ eventKind, delay time.Duration, text string) timedEvent {
		return timedEvent{typ: typ, delay: delay, text: text}
	}
	return []timedEvent{
		// Utterance 1.
		ev(evSpeechStart, 500*time.Millisecond, ""),
		ev(evPartial, 150*time.Millisecond, "hell"),
		ev(evPartial, 120*time.Millisecond, "hello"),
		ev(evPartial, 130*time.Millisecond, "hello wo"),
		ev(evPartial, 110*time.Millisecond, "hello world"),
		ev(evPartial, 120*time.Millisecond, "hello world how"),
		ev(evPartial, 130*time.Millisecond, "hello world how are"),
		ev(evPartial, 110*time.Millisecond, "hello world how are you"),
		ev(evFinal, 300*time.Millisecond, "hello world how are you"),
		ev(evSpeechEnd, 100*time.Millisecond, ""),

		// Pause between utterances.
		ev(evSpeechStart, 1500*time.Millisecond, ""),

		// Utterance 2.
		ev(evPartial, 150*time.Millisecond, "this"),
		ev(evPartial, 130*time.Millisecond, "this is"),
		ev(evPartial, 120*time.Millisecond, "this is a"),
		ev(evPartial, 140*time.Millisecond, "this is a test"),
		ev(evFinal, 300*time.Millisecond, "this is a test"),
		ev(evSpeechEnd, 100*time.Millisecond, ""),

		// Give the user 3s to observe the final state before the goroutine exits.
		ev(evSpeechEnd, 3*time.Second, ""),
	}
}

// isTerminal reports whether the file is connected to a terminal.
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}
