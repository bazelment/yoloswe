package app

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestRepoPicker(repos []string) RepoPickerModel {
	m := RepoPickerModel{
		repos:  repos,
		width:  80,
		height: 24,
	}
	return m
}

func TestRepoPickerFilter_TypeToFilter(t *testing.T) {
	m := newTestRepoPicker([]string{"foo", "bar", "baz"})

	// Type 'b' — should filter to bar and baz
	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'b'}})
	m = m2.(RepoPickerModel)

	eff := m.effectiveRepos()
	require.Len(t, eff, 2)
	assert.Equal(t, "bar", eff[0])
	assert.Equal(t, "baz", eff[1])
	assert.Equal(t, "b", m.filterText)
}

func TestRepoPickerFilter_TypeMultipleChars(t *testing.T) {
	m := newTestRepoPicker([]string{"alpha", "beta", "gamma"})

	// Type "al" — should filter to just alpha
	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	m = m2.(RepoPickerModel)
	m2, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
	m = m2.(RepoPickerModel)

	eff := m.effectiveRepos()
	require.Len(t, eff, 1)
	assert.Equal(t, "alpha", eff[0])
}

func TestRepoPickerFilter_BackspaceRemovesChar(t *testing.T) {
	m := newTestRepoPicker([]string{"foo", "bar", "baz"})

	// Type "ba"
	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'b'}})
	m = m2.(RepoPickerModel)
	m2, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	m = m2.(RepoPickerModel)
	require.Equal(t, "ba", m.filterText)

	eff := m.effectiveRepos()
	require.Len(t, eff, 2) // bar and baz

	// Backspace — back to "b"
	m2, _ = m.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	m = m2.(RepoPickerModel)
	assert.Equal(t, "b", m.filterText)

	eff = m.effectiveRepos()
	assert.Len(t, eff, 2) // still bar and baz
}

func TestRepoPickerFilter_EscClearsFilter(t *testing.T) {
	m := newTestRepoPicker([]string{"alpha", "beta", "gamma"})

	// Type "a"
	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	m = m2.(RepoPickerModel)
	require.Equal(t, "a", m.filterText)

	// Esc clears filter (does not quit)
	m2, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEscape})
	m = m2.(RepoPickerModel)
	assert.Equal(t, "", m.filterText)
	assert.Nil(t, cmd) // Should NOT quit

	eff := m.effectiveRepos()
	assert.Len(t, eff, 3) // All repos back
}

func TestRepoPickerFilter_EscQuitsWhenNoFilter(t *testing.T) {
	m := newTestRepoPicker([]string{"alpha", "beta"})

	// Esc with no filter should quit
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEscape})
	assert.NotNil(t, cmd) // Should be tea.Quit
}

func TestRepoPickerFilter_EnterSelectsFromFiltered(t *testing.T) {
	m := newTestRepoPicker([]string{"alpha", "beta", "gamma"})

	// Type "be" to filter to beta
	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'b'}})
	m = m2.(RepoPickerModel)
	m2, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
	m = m2.(RepoPickerModel)

	eff := m.effectiveRepos()
	require.Len(t, eff, 1)
	assert.Equal(t, "beta", eff[0])

	// Enter should select beta
	m2, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = m2.(RepoPickerModel)
	assert.Equal(t, "beta", m.SelectedRepo())
}

func TestRepoPickerFilter_CaseInsensitive(t *testing.T) {
	m := newTestRepoPicker([]string{"Alpha", "BETA", "gamma"})

	// Type "beta" in lowercase — should match "BETA"
	for _, r := range "beta" {
		m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = m2.(RepoPickerModel)
	}

	eff := m.effectiveRepos()
	require.Len(t, eff, 1)
	assert.Equal(t, "BETA", eff[0])
}

func TestRepoPickerFilter_NoMatchesShowsMessage(t *testing.T) {
	m := newTestRepoPicker([]string{"alpha", "beta"})

	// Type "xyz" — no matches
	for _, r := range "xyz" {
		m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = m2.(RepoPickerModel)
	}

	eff := m.effectiveRepos()
	assert.Len(t, eff, 0)

	view := m.View()
	assert.Contains(t, view, "No matches")
}

func TestRepoPickerFilter_EscAfterNoMatchesRestoresSelection(t *testing.T) {
	m := newTestRepoPicker([]string{"alpha", "beta", "gamma"})

	// Type "xyz" — no matches, selectedIdx becomes -1
	for _, r := range "xyz" {
		m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = m2.(RepoPickerModel)
	}
	require.Equal(t, -1, m.selectedIdx)
	require.Len(t, m.effectiveRepos(), 0)

	// Esc clears filter — selection should be valid (not -1)
	m2, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEscape})
	m = m2.(RepoPickerModel)
	assert.Nil(t, cmd) // Should not quit
	assert.Equal(t, "", m.filterText)
	assert.Len(t, m.effectiveRepos(), 3)
	assert.GreaterOrEqual(t, m.selectedIdx, 0)

	// Enter should now select a repo
	m2, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = m2.(RepoPickerModel)
	assert.NotEmpty(t, m.SelectedRepo())
}

func TestRepoPickerFilter_QFilterCharWhenActive(t *testing.T) {
	m := newTestRepoPicker([]string{"query-service", "beta"})

	// Start filter with 'a'
	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	m = m2.(RepoPickerModel)
	require.Equal(t, "a", m.filterText)

	// Now 'q' should be appended to filter, not quit
	m2, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	m = m2.(RepoPickerModel)
	assert.Equal(t, "aq", m.filterText)
	assert.Nil(t, cmd) // Should NOT quit
}
