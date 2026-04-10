package render

import (
	"io"
	"os"
)

// ColorMode controls how color output is handled.
type ColorMode int

const (
	// ColorAuto detects whether stdout is a terminal and enables colors accordingly.
	ColorAuto ColorMode = iota
	// ColorAlways forces color output on.
	ColorAlways
	// ColorNever disables color output.
	ColorNever
)

// Palette controls whether ANSI color codes are emitted.
type Palette struct {
	enabled bool
}

// DefaultPalette returns a palette with colors enabled.
func DefaultPalette() Palette {
	return Palette{enabled: true}
}

// NoPalette returns a palette with colors disabled.
func NoPalette() Palette {
	return Palette{}
}

// resolvePalette determines color enablement based on ColorMode and output writer.
func resolvePalette(mode ColorMode, out io.Writer) Palette {
	switch mode {
	case ColorAlways:
		return DefaultPalette()
	case ColorNever:
		return NoPalette()
	default: // ColorAuto
		if isTerminalWriter(out) {
			return DefaultPalette()
		}
		return NoPalette()
	}
}

// colorFor returns the ANSI code if colors are enabled, empty string otherwise.
func (p Palette) colorFor(c string) string {
	if !p.enabled {
		return ""
	}
	return c
}

// isTerminalWriter checks if the writer is backed by a terminal.
func isTerminalWriter(w io.Writer) bool {
	if f, ok := w.(*os.File); ok {
		stat, err := f.Stat()
		if err != nil {
			return false
		}
		return (stat.Mode() & os.ModeCharDevice) != 0
	}
	return false
}
