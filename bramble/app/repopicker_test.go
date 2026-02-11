package app

import (
	"context"
	"fmt"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestRepoPicker(repos []string) RepoPickerModel {
	m := RepoPickerModel{
		ctx:    context.Background(),
		repos:  repos,
		styles: NewStyles(Dark),
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

	// Type "be" — should filter to just beta
	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'b'}})
	m = m2.(RepoPickerModel)
	m2, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
	m = m2.(RepoPickerModel)

	eff := m.effectiveRepos()
	require.Len(t, eff, 1)
	assert.Equal(t, "beta", eff[0])
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

	// Type "g"
	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'g'}})
	m = m2.(RepoPickerModel)
	require.Equal(t, "g", m.filterText)

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

	// Start filter with 'b'
	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'b'}})
	m = m2.(RepoPickerModel)
	require.Equal(t, "b", m.filterText)

	// Now 'q' should be appended to filter, not quit
	m2, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	m = m2.(RepoPickerModel)
	assert.Equal(t, "bq", m.filterText)
	assert.Nil(t, cmd) // Should NOT quit
}

// --- URL input mode tests ---

func TestRepoPickerURLInput_AEntersURLMode(t *testing.T) {
	m := newTestRepoPicker([]string{"foo", "bar"})

	// Press 'a' with no filter — enters URL input mode
	m2, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	m = m2.(RepoPickerModel)

	assert.Equal(t, pickerModeURLInput, m.mode)
	assert.Equal(t, "", m.urlInput)
	assert.Nil(t, cmd)
}

func TestRepoPickerURLInput_AIsFilterCharWhenFilterActive(t *testing.T) {
	m := newTestRepoPicker([]string{"alpha", "beta"})

	// Start filter with 'b'
	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'b'}})
	m = m2.(RepoPickerModel)
	require.Equal(t, "b", m.filterText)

	// Now 'a' should be appended to filter, not enter URL mode
	m2, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	m = m2.(RepoPickerModel)
	assert.Equal(t, pickerModeList, m.mode)
	assert.Equal(t, "ba", m.filterText)
}

func TestRepoPickerURLInput_CharacterAccumulation(t *testing.T) {
	m := newTestRepoPicker([]string{"foo"})
	m.mode = pickerModeURLInput

	// Type characters
	for _, r := range "https://github.com/user/repo" {
		m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = m2.(RepoPickerModel)
	}

	assert.Equal(t, "https://github.com/user/repo", m.urlInput)
}

func TestRepoPickerURLInput_BackspaceRemovesChar(t *testing.T) {
	m := newTestRepoPicker(nil)
	m.mode = pickerModeURLInput
	m.urlInput = "https"

	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	m = m2.(RepoPickerModel)

	assert.Equal(t, "http", m.urlInput)
}

func TestRepoPickerURLInput_BackspaceOnEmptyIsNoop(t *testing.T) {
	m := newTestRepoPicker(nil)
	m.mode = pickerModeURLInput
	m.urlInput = ""

	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	m = m2.(RepoPickerModel)

	assert.Equal(t, "", m.urlInput)
	assert.Equal(t, pickerModeURLInput, m.mode)
}

func TestRepoPickerURLInput_EscReturnsToList(t *testing.T) {
	m := newTestRepoPicker([]string{"foo"})
	m.mode = pickerModeURLInput
	m.urlInput = "https://example.com"

	m2, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEscape})
	m = m2.(RepoPickerModel)

	assert.Equal(t, pickerModeList, m.mode)
	assert.Equal(t, "", m.urlInput)
	assert.Nil(t, m.cloneErr)
	assert.Nil(t, cmd)
}

func TestRepoPickerURLInput_EnterWithEmptyURLDoesNothing(t *testing.T) {
	m := newTestRepoPicker(nil)
	m.mode = pickerModeURLInput
	m.urlInput = ""

	m2, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = m2.(RepoPickerModel)

	assert.Equal(t, pickerModeURLInput, m.mode)
	assert.Nil(t, cmd)
}

func TestRepoPickerURLInput_EnterWithWhitespaceOnlyDoesNothing(t *testing.T) {
	m := newTestRepoPicker(nil)
	m.mode = pickerModeURLInput
	m.urlInput = "   "

	m2, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = m2.(RepoPickerModel)

	assert.Equal(t, pickerModeURLInput, m.mode)
	assert.Nil(t, cmd)
}

func TestRepoPickerURLInput_EnterWithURLStartsCloning(t *testing.T) {
	m := newTestRepoPicker(nil)
	m.mode = pickerModeURLInput
	m.urlInput = "https://github.com/user/repo"

	m2, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = m2.(RepoPickerModel)

	assert.Equal(t, pickerModeCloning, m.mode)
	assert.Equal(t, "https://github.com/user/repo", m.cloneRepoName)
	assert.Nil(t, m.cloneErr)
	assert.NotNil(t, cmd)           // Should return the initRepo command
	assert.NotNil(t, m.cloneCancel) // Should have a cancel function
}

func TestRepoPickerURLInput_CtrlCQuits(t *testing.T) {
	m := newTestRepoPicker(nil)
	m.mode = pickerModeURLInput

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	assert.NotNil(t, cmd) // Should be tea.Quit
}

// --- Clone result tests ---

func TestRepoPickerCloning_SuccessReloadsAndSelectsRepo(t *testing.T) {
	m := newTestRepoPicker(nil)
	m.mode = pickerModeCloning
	m.cloneRepoName = "https://github.com/user/myrepo"
	m.urlInput = "https://github.com/user/myrepo"

	m2, cmd := m.Update(repoInitSuccessMsg{repoName: "myrepo"})
	m = m2.(RepoPickerModel)

	assert.Equal(t, pickerModeList, m.mode)
	assert.Equal(t, "myrepo", m.pendingSelectRepo)
	assert.Equal(t, "", m.urlInput)
	assert.Nil(t, m.cloneErr)
	assert.Equal(t, "", m.cloneRepoName)
	assert.True(t, m.loading)
	assert.NotNil(t, cmd) // Should return loadRepos command
}

func TestRepoPickerCloning_ErrorReturnsToURLInput(t *testing.T) {
	m := newTestRepoPicker(nil)
	m.mode = pickerModeCloning
	m.cloneRepoName = "https://example.com/bad"
	m.urlInput = "https://example.com/bad"

	cloneErr := fmt.Errorf("repository not found")
	m2, cmd := m.Update(repoInitErrorMsg{err: cloneErr})
	m = m2.(RepoPickerModel)

	assert.Equal(t, pickerModeURLInput, m.mode)
	assert.Equal(t, cloneErr, m.cloneErr)
	// URL is preserved so user can edit it
	assert.Equal(t, "https://example.com/bad", m.urlInput)
	assert.Equal(t, "", m.cloneRepoName)
	assert.Nil(t, cmd)
}

func TestRepoPickerCloning_CtrlCQuits(t *testing.T) {
	m := newTestRepoPicker(nil)
	m.mode = pickerModeCloning

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	assert.NotNil(t, cmd) // Should be tea.Quit
}

func TestRepoPickerCloning_OtherKeysIgnored(t *testing.T) {
	m := newTestRepoPicker(nil)
	m.mode = pickerModeCloning
	m.cloneRepoName = "https://example.com/repo"

	// Regular keys are ignored during cloning
	m2, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	m = m2.(RepoPickerModel)

	assert.Equal(t, pickerModeCloning, m.mode)
	assert.Nil(t, cmd)
}

// --- pendingSelectRepo auto-selection ---

func TestRepoPickerPendingSelect_AutoSelectsAfterLoad(t *testing.T) {
	m := newTestRepoPicker(nil)
	m.pendingSelectRepo = "bar"

	// Simulate repos loading with the cloned repo in the list
	m2, _ := m.Update(repoLoadedMsg{repos: []string{"alpha", "bar", "gamma"}})
	m = m2.(RepoPickerModel)

	assert.Equal(t, 1, m.selectedIdx) // "bar" is at index 1
	assert.Equal(t, "", m.pendingSelectRepo)
}

func TestRepoPickerPendingSelect_ClearedEvenIfNotFound(t *testing.T) {
	m := newTestRepoPicker(nil)
	m.pendingSelectRepo = "missing"

	m2, _ := m.Update(repoLoadedMsg{repos: []string{"alpha", "beta"}})
	m = m2.(RepoPickerModel)

	assert.Equal(t, "", m.pendingSelectRepo) // Cleared even if not found
}

// --- Repo load in non-list modes ---

func TestRepoPickerURLInput_RepoLoadedMsgHandled(t *testing.T) {
	// If loadRepos completes while in URL input mode, it should update repos
	// and clear loading so returning to list doesn't show stale "Loading..." state.
	m := newTestRepoPicker(nil)
	m.loading = true
	m.mode = pickerModeURLInput

	m2, _ := m.Update(repoLoadedMsg{repos: []string{"alpha", "beta"}})
	m = m2.(RepoPickerModel)

	assert.False(t, m.loading)
	assert.Len(t, m.repos, 2)
	assert.Equal(t, pickerModeURLInput, m.mode) // stays in URL input mode
}

func TestRepoPickerURLInput_RepoLoadErrorMsgHandled(t *testing.T) {
	m := newTestRepoPicker(nil)
	m.loading = true
	m.mode = pickerModeURLInput

	m2, _ := m.Update(repoLoadErrorMsg{err: fmt.Errorf("load failed")})
	m = m2.(RepoPickerModel)

	assert.False(t, m.loading)
	assert.Equal(t, pickerModeURLInput, m.mode)
}

func TestRepoPickerCloning_RepoLoadedMsgHandled(t *testing.T) {
	m := newTestRepoPicker(nil)
	m.loading = true
	m.mode = pickerModeCloning

	m2, _ := m.Update(repoLoadedMsg{repos: []string{"alpha"}})
	m = m2.(RepoPickerModel)

	assert.False(t, m.loading)
	assert.Len(t, m.repos, 1)
	assert.Equal(t, pickerModeCloning, m.mode)
}

// --- View tests ---

func TestRepoPickerView_EmptyStateShowsAddHint(t *testing.T) {
	m := newTestRepoPicker(nil)
	m.wtRoot = "/tmp/wt"

	view := m.View()
	assert.Contains(t, view, "[a] to add a repository")
	assert.NotContains(t, view, "wt init")
}

func TestRepoPickerView_NormalStateFooterShowsAddRepo(t *testing.T) {
	m := newTestRepoPicker([]string{"foo", "bar"})

	view := m.View()
	assert.Contains(t, view, "[a] add repo")
}

func TestRepoPickerView_URLInputMode(t *testing.T) {
	m := newTestRepoPicker(nil)
	m.mode = pickerModeURLInput
	m.urlInput = "https://github.com/test"

	view := m.View()
	assert.Contains(t, view, "Add repository")
	assert.Contains(t, view, "Enter a git URL")
	assert.Contains(t, view, "https://github.com/test")
	assert.Contains(t, view, "[Enter] clone")
	assert.Contains(t, view, "[Esc] back")
}

func TestRepoPickerView_URLInputModeWithError(t *testing.T) {
	m := newTestRepoPicker(nil)
	m.mode = pickerModeURLInput
	m.urlInput = "https://bad.url"
	m.cloneErr = fmt.Errorf("clone failed")

	view := m.View()
	assert.Contains(t, view, "clone failed")
	assert.Contains(t, view, "https://bad.url")
}

func TestRepoPickerView_CloningMode(t *testing.T) {
	m := newTestRepoPicker(nil)
	m.mode = pickerModeCloning
	m.cloneRepoName = "https://github.com/user/repo"

	view := m.View()
	assert.Contains(t, view, "Cloning")
	assert.Contains(t, view, "https://github.com/user/repo")
	assert.Contains(t, view, "This may take a moment")
}

func TestRepoPickerURLInput_AFromEmptyRepoList(t *testing.T) {
	// When there are zero repos, 'a' should still enter URL mode
	m := newTestRepoPicker(nil)

	m2, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	m = m2.(RepoPickerModel)

	assert.Equal(t, pickerModeURLInput, m.mode)
	assert.Nil(t, cmd)
}

// --- URL validation tests ---

func TestRepoPickerInitRepo_TrailingSlashURLReturnsError(t *testing.T) {
	// A URL with trailing slash yields empty repo name — should error
	m := newTestRepoPicker(nil)
	cmd := m.initRepo(context.Background(), "https://github.com/user/repo/")
	msg := cmd()
	errMsg, ok := msg.(repoInitErrorMsg)
	require.True(t, ok, "expected repoInitErrorMsg, got %T", msg)
	assert.Contains(t, errMsg.err.Error(), "could not determine repository name")
}

func TestRepoPickerInitRepo_BareSlashURLReturnsError(t *testing.T) {
	m := newTestRepoPicker(nil)
	cmd := m.initRepo(context.Background(), "https://github.com/")
	msg := cmd()
	errMsg, ok := msg.(repoInitErrorMsg)
	require.True(t, ok, "expected repoInitErrorMsg, got %T", msg)
	assert.Contains(t, errMsg.err.Error(), "could not determine repository name")
}

func TestRepoPickerInitRepo_DotRepoNameReturnsError(t *testing.T) {
	m := newTestRepoPicker(nil)
	cmd := m.initRepo(context.Background(), "https://example.com/.")
	msg := cmd()
	errMsg, ok := msg.(repoInitErrorMsg)
	require.True(t, ok, "expected repoInitErrorMsg, got %T", msg)
	assert.Contains(t, errMsg.err.Error(), "could not determine repository name")
}

func TestRepoPickerInitRepo_DotDotRepoNameReturnsError(t *testing.T) {
	m := newTestRepoPicker(nil)
	cmd := m.initRepo(context.Background(), "https://example.com/..")
	msg := cmd()
	errMsg, ok := msg.(repoInitErrorMsg)
	require.True(t, ok, "expected repoInitErrorMsg, got %T", msg)
	assert.Contains(t, errMsg.err.Error(), "could not determine repository name")
}

// --- Clone cancellation tests ---

func TestRepoPickerCloning_CtrlCCancelsCloneContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := newTestRepoPicker(nil)
	m.ctx = ctx
	m.mode = pickerModeCloning

	// Simulate having a cloneCancel (set when Enter starts cloning)
	cloneCtx, cloneCancel := context.WithCancel(ctx)
	m.cloneCancel = cloneCancel

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	assert.NotNil(t, cmd) // Should be tea.Quit

	// The clone context should be cancelled
	assert.Error(t, cloneCtx.Err())
}

func TestRepoPickerCloning_SuccessCleansUpCancel(t *testing.T) {
	m := newTestRepoPicker(nil)
	m.mode = pickerModeCloning
	_, cancel := context.WithCancel(context.Background())
	m.cloneCancel = cancel

	m2, _ := m.Update(repoInitSuccessMsg{repoName: "myrepo"})
	m = m2.(RepoPickerModel)

	assert.Nil(t, m.cloneCancel)
}

func TestRepoPickerCloning_ErrorCleansUpCancel(t *testing.T) {
	m := newTestRepoPicker(nil)
	m.mode = pickerModeCloning
	m.urlInput = "https://example.com/bad"
	_, cancel := context.WithCancel(context.Background())
	m.cloneCancel = cancel

	m2, _ := m.Update(repoInitErrorMsg{err: fmt.Errorf("failed")})
	m = m2.(RepoPickerModel)

	assert.Nil(t, m.cloneCancel)
}
