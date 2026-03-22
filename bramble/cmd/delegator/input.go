package delegator

import (
	"bufio"
	"io"
	"os"
	"path/filepath"

	"github.com/ergochat/readline"
)

// InputReader provides line-editing and history for interactive input.
// When stdin is not a terminal, it falls back to bufio.Scanner.
type InputReader struct {
	rl    *readline.Instance // nil when falling back to scanner
	lines chan string
	done  chan struct{}
}

// NewInputReader creates an InputReader. If stdin is a real terminal,
// it uses readline for line editing and history; otherwise it falls back
// to a plain bufio.Scanner.
func NewInputReader(prompt string) (*InputReader, error) {
	ir := &InputReader{
		lines: make(chan string),
		done:  make(chan struct{}),
	}

	stat, _ := os.Stdin.Stat()
	isTerminal := (stat.Mode() & os.ModeCharDevice) != 0

	if isTerminal {
		historyFile := defaultHistoryFile()
		rl, err := readline.NewEx(&readline.Config{
			Prompt:      prompt,
			HistoryFile: historyFile,
		})
		if err != nil {
			// Fall back to scanner if readline fails to initialize.
			ir.startScanner(os.Stdin, prompt)
			return ir, nil
		}
		ir.rl = rl
		ir.startReadline()
	} else {
		ir.startScanner(os.Stdin, "")
	}
	return ir, nil
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
func (ir *InputReader) Close() {
	if ir.rl != nil {
		ir.rl.Close()
	}
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
			ir.lines <- line
		}
	}()
}

func (ir *InputReader) startScanner(r io.Reader, prompt string) {
	go func() {
		defer close(ir.lines)
		scanner := bufio.NewScanner(r)
		for scanner.Scan() {
			ir.lines <- scanner.Text()
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
