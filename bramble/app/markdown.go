package app

import (
	"github.com/charmbracelet/glamour"
)

// MarkdownRenderer wraps glamour for terminal markdown rendering.
type MarkdownRenderer struct {
	renderer *glamour.TermRenderer
	width    int
}

// NewMarkdownRenderer creates a new markdown renderer with the given width.
func NewMarkdownRenderer(width int) (*MarkdownRenderer, error) {
	r, err := glamour.NewTermRenderer(
		glamour.WithAutoStyle(),
		glamour.WithWordWrap(width),
	)
	if err != nil {
		return nil, err
	}
	return &MarkdownRenderer{
		renderer: r,
		width:    width,
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
	r, err := glamour.NewTermRenderer(
		glamour.WithAutoStyle(),
		glamour.WithWordWrap(width),
	)
	if err != nil {
		return err
	}
	m.renderer = r
	m.width = width
	return nil
}
