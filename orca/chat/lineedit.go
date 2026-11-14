package chat

import (
	"strings"
	"unicode"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
)

// setInput replaces input and moves the cursor to its end.
func (m *Model) setInput(s string) { m.input, m.cursor = s, len(s) }

// insert places s at the cursor.
func (m *Model) insert(s string) {
	m.clampCursor()
	m.input = m.input[:m.cursor] + s + m.input[m.cursor:]
	m.cursor += len(s)
}

func (m *Model) clampCursor() { m.cursor = min(max(m.cursor, 0), len(m.input)) }

// edit applies readline-style movement, deletion, and insertion keys.
func (m *Model) edit(k tea.KeyPressMsg) {
	m.clampCursor()
	before, after := m.input[:m.cursor], m.input[m.cursor:]
	ctrl := func(c rune) bool { return k.Mod == tea.ModCtrl && k.Code == c }
	switch {
	case ctrl('a') || k.Code == tea.KeyHome:
		m.cursor = 0
	case ctrl('e') || k.Code == tea.KeyEnd:
		m.cursor = len(m.input)
	case ctrl('b') || (k.Code == tea.KeyLeft && k.Mod == 0):
		_, n := utf8.DecodeLastRuneInString(before)
		m.cursor -= n
	case ctrl('f') || (k.Code == tea.KeyRight && k.Mod == 0):
		_, n := utf8.DecodeRuneInString(after)
		m.cursor += n
	case ctrl('u'):
		m.input, m.cursor = after, 0
	case ctrl('k'):
		m.input = before
	case ctrl('w') || (k.Code == tea.KeyBackspace && k.Mod == tea.ModAlt):
		cut := len(strings.TrimRightFunc(before, unicode.IsSpace))
		cut = strings.LastIndexFunc(before[:cut], unicode.IsSpace) + 1
		m.input, m.cursor = before[:cut]+after, cut
	case ctrl('h') || (k.Code == tea.KeyBackspace && k.Mod == 0):
		_, n := utf8.DecodeLastRuneInString(before)
		m.input, m.cursor = before[:len(before)-n]+after, m.cursor-n
	case ctrl('d') || (k.Code == tea.KeyDelete && k.Mod == 0):
		_, n := utf8.DecodeRuneInString(after)
		m.input = before + after[n:]
	case k.Text != "" && k.Mod&^tea.ModShift == 0:
		m.insert(k.Text)
	}
}

// inputView renders input with the cursor highlighted.
func (m Model) inputView() string {
	m.clampCursor()
	before, after := m.input[:m.cursor], m.input[m.cursor:]
	under := " "
	if after != "" {
		_, n := utf8.DecodeRuneInString(after)
		under, after = after[:n], after[n:]
	}
	return before + "\x1b[7m" + under + "\x1b[27m" + after
}
