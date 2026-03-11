package app

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"

	"github.com/bazelment/yoloswe/bramble/session"
)

func TestDropdownSizing_AppliedAtModelCreation(t *testing.T) {
	m := setupModel(t, session.SessionModeTUI, nil, "test-repo")

	assert.Equal(t, 76, m.worktreeDropdown.Width())
	assert.Equal(t, 76, m.sessionDropdown.Width())
	assert.Equal(t, 76, m.repoDropdown.Width())

	assert.Equal(t, 18, m.worktreeDropdown.maxVisible)
	assert.Equal(t, 18, m.sessionDropdown.maxVisible)
	assert.Equal(t, 18, m.repoDropdown.maxVisible)
}

func TestDropdownSizing_UpdatesOnWindowResize(t *testing.T) {
	m := setupModel(t, session.SessionModeTUI, nil, "test-repo")

	newModel, _ := m.Update(tea.WindowSizeMsg{Width: 132, Height: 41})
	m2 := newModel.(Model)

	assert.Equal(t, 128, m2.worktreeDropdown.Width())
	assert.Equal(t, 128, m2.sessionDropdown.Width())
	assert.Equal(t, 128, m2.repoDropdown.Width())

	assert.Equal(t, 35, m2.worktreeDropdown.maxVisible)
	assert.Equal(t, 35, m2.sessionDropdown.maxVisible)
	assert.Equal(t, 35, m2.repoDropdown.maxVisible)
}
