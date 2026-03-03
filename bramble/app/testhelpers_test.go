package app

import tea "charm.land/bubbletea/v2"

// keyPress creates a KeyPressMsg for a printable character.
func keyPress(r rune) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: r, Text: string(r)}
}

// specialKey creates a KeyPressMsg for a non-printable/special key.
func specialKey(code rune) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: code}
}
