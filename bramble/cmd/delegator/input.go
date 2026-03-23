package delegator

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/ergochat/readline"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude/render"
)

// InputReader provides line-editing and history for interactive input.
// When stdin is not a terminal, it falls back to bufio.Scanner.
type InputReader struct {
	rl        *readline.Instance // nil when falling back to scanner
	lines     chan string
	quit      chan struct{} // closed by Close() to unblock goroutines stuck on channel send
	closeOnce sync.Once
}

// NewInputReader creates an InputReader. If stdin is a real terminal,
// it uses readline for line editing and history; otherwise it falls back
// to a plain bufio.Scanner.
// NewInputReader always succeeds: readline initialization errors cause a
// silent fallback to the scanner path.
func NewInputReader(prompt string) *InputReader {
	ir := &InputReader{
		lines: make(chan string),
		quit:  make(chan struct{}),
	}

	if render.IsTerminal(os.Stdin) {
		historyFile := defaultHistoryFile()
		rl, err := readline.NewEx(&readline.Config{
			Prompt:      prompt,
			HistoryFile: historyFile,
			// Write prompts and line-editing output to stderr, not stdout.
			// The delegator reserves stdout for LLM answer text only.
			Stdout: os.Stderr,
			Stderr: os.Stderr,
		})
		if err != nil {
			// Fall back to scanner if readline fails to initialize.
			ir.startScanner(os.Stdin, prompt)
			return ir
		}
		ir.rl = rl
		ir.startReadline()
	} else {
		ir.startScanner(os.Stdin, "")
	}
	return ir
}

// Lines returns a channel that receives each line of user input.
// The channel is closed on EOF or error.
func (ir *InputReader) Lines() <-chan string {
	return ir.lines
}

// SetPrompt changes the prompt displayed to the user.
// No-op when in scanner fallback mode.
func (ir *InputReader) SetPrompt(prompt string) {
	if ir.rl != nil {
		ir.rl.SetPrompt(prompt)
	}
}

// RefreshPrompt redraws the current prompt on the terminal.
// This is useful after other output has been written to stderr.
// No-op when in scanner fallback mode.
func (ir *InputReader) RefreshPrompt() {
	if ir.rl != nil {
		ir.rl.Refresh()
	}
}

// Close shuts down the input reader and releases resources.
// It closes the internal quit channel so that goroutines blocked on channel
// sends exit immediately, even if the consumer has stopped reading Lines().
// Close is idempotent and safe to call multiple times.
func (ir *InputReader) Close() {
	ir.closeOnce.Do(func() {
		close(ir.quit)
		if ir.rl != nil {
			ir.rl.Close()
		}
	})
}

func (ir *InputReader) startReadline() {
	go func() {
		defer close(ir.lines)
		for {
			line, err := ir.rl.Readline()
			if err != nil {
				// EOF, Ctrl-C, or terminal error.
				return
			}
			select {
			case ir.lines <- line:
			case <-ir.quit:
				return
			}
		}
	}()
}

func (ir *InputReader) startScanner(r io.Reader, prompt string) {
	go func() {
		defer close(ir.lines)
		scanner := bufio.NewScanner(r)
		// Print prompt before the first read and before each subsequent read,
		// so the user always sees the prompt before they need to type.
		if prompt != "" {
			fmt.Fprint(os.Stderr, prompt)
		}
		for scanner.Scan() {
			select {
			case ir.lines <- scanner.Text():
			case <-ir.quit:
				return
			}
			if prompt != "" {
				fmt.Fprint(os.Stderr, prompt)
			}
		}
	}()
}

// defaultHistoryFile returns the path for delegator command history.
func defaultHistoryFile() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	dir := filepath.Join(home, ".bramble")
	// Ensure the directory exists (best-effort).
	os.MkdirAll(dir, 0o755)
	return filepath.Join(dir, "delegator_history")
}
