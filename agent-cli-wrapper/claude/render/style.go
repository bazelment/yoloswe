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

// ParseColorMode converts a string to a ColorMode.
// Returns ColorAuto for unrecognized values.
func ParseColorMode(s string) ColorMode {
	switch s {
	case "always":
		return ColorAlways
	case "never":
		return ColorNever
	default:
		return ColorAuto
	}
}

// Palette controls whether ANSI color codes are emitted.
type Palette struct {
	enabled bool
}

// defaultPalette returns a palette with colors enabled.
func defaultPalette() Palette {
	return Palette{enabled: true}
}

// noPalette returns a palette with colors disabled.
func noPalette() Palette {
	return Palette{}
}

// resolvePalette determines color enablement based on ColorMode and output writer.
func resolvePalette(mode ColorMode, out io.Writer) Palette {
	switch mode {
	case ColorAlways:
		return defaultPalette()
	case ColorNever:
		return noPalette()
	default: // ColorAuto
		if isTerminalWriter(out) {
			return defaultPalette()
		}
		return noPalette()
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
