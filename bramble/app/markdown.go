package app

import (
	"github.com/charmbracelet/glamour"
)

// MarkdownRenderer wraps glamour for terminal markdown rendering.
type MarkdownRenderer struct { //nolint:govet // fieldalignment: readability over padding
	renderer     *glamour.TermRenderer
	width        int
	glamourStyle string // "dark", "light", or "auto"
}

// NewMarkdownRenderer creates a new markdown renderer with the given width and style.
// If glamourStyle is empty, "auto" is used.
func NewMarkdownRenderer(width int, glamourStyle string) (*MarkdownRenderer, error) {
	if glamourStyle == "" {
		glamourStyle = "auto"
	}
	opt := glamour.WithAutoStyle()
	switch glamourStyle {
	case "dark":
		opt = glamour.WithStandardStyle("dark")
	case "light":
		opt = glamour.WithStandardStyle("light")
	}
	r, err := glamour.NewTermRenderer(
		opt,
		glamour.WithWordWrap(width),
	)
	if err != nil {
		return nil, err
	}
	return &MarkdownRenderer{
		renderer:     r,
		width:        width,
		glamourStyle: glamourStyle,
	}, nil
}

// Render renders markdown text for terminal display.
func (m *MarkdownRenderer) Render(text string) (string, error) {
	return m.renderer.Render(text)
}

// SetWidth updates the word wrap width and recreates the renderer.
func (m *MarkdownRenderer) SetWidth(width int) error {
	if width == m.width {
		return nil
	}
	opt := glamour.WithAutoStyle()
	switch m.glamourStyle {
	case "dark":
		opt = glamour.WithStandardStyle("dark")
	case "light":
		opt = glamour.WithStandardStyle("light")
	}
	r, err := glamour.NewTermRenderer(
		opt,
		glamour.WithWordWrap(width),
	)
	if err != nil {
		return err
	}
	m.renderer = r
	m.width = width
	return nil
}
