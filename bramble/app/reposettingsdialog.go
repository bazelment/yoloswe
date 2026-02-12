package app

import (
	"strings"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// RepoSettingsDialogFocus indicates which control is focused.
type RepoSettingsDialogFocus int

const (
	RepoSettingsFocusTheme RepoSettingsDialogFocus = iota
	RepoSettingsFocusCreate
	RepoSettingsFocusDelete
	RepoSettingsFocusSave
	RepoSettingsFocusCancel
)

// RepoSettingsDialogAction indicates the result of a key update.
type RepoSettingsDialogAction int

const (
	RepoSettingsActionNone RepoSettingsDialogAction = iota
	RepoSettingsActionSave
	RepoSettingsActionCancel
	RepoSettingsActionQuit
)

// RepoSettingsDialog is an overlay for editing per-repo worktree hook commands.
type RepoSettingsDialog struct { //nolint:govet // fieldalignment: readability over packing
	createInput textarea.Model
	deleteInput textarea.Model
	themes      []ColorPalette
	repoName    string
	original    string
	width       int
	height      int
	selectedIdx int
	focus       RepoSettingsDialogFocus
	visible     bool
}

// NewRepoSettingsDialog creates a new repo settings dialog.
func NewRepoSettingsDialog() *RepoSettingsDialog {
	return &RepoSettingsDialog{
		createInput: newRepoSettingsTextArea(),
		deleteInput: newRepoSettingsTextArea(),
		themes:      BuiltinThemes,
		focus:       RepoSettingsFocusTheme,
	}
}

func newRepoSettingsTextArea() textarea.Model {
	ta := textarea.New()
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	ta.Prompt = ""
	ta.SetWidth(60)
	ta.SetHeight(4)
	ta.MaxHeight = 8
	ta.FocusedStyle = textarea.Style{
		Base:        lipgloss.NewStyle(),
		Placeholder: lipgloss.NewStyle().Foreground(lipgloss.Color("245")),
		Text:        lipgloss.NewStyle(),
		CursorLine:  lipgloss.NewStyle(),
	}
	ta.BlurredStyle = ta.FocusedStyle

	// Keep Enter for text input; use Ctrl+Enter to save dialog.
	ta.KeyMap.InsertNewline = key.NewBinding(
		key.WithKeys("enter", "shift+enter"),
		key.WithHelp("enter", "newline"),
	)
	ta.KeyMap.Paste.SetEnabled(false)
	return ta
}

// Show opens the dialog with repo settings.
func (d *RepoSettingsDialog) Show(repoName string, cfg RepoSettings, currentTheme string, w, h int, placeholderColor lipgloss.Color) {
	d.repoName = repoName
	d.width = w
	d.height = h
	d.visible = true
	d.focus = RepoSettingsFocusTheme
	d.original = currentTheme
	d.selectedIdx = 0
	for i := range d.themes {
		if d.themes[i].Name == currentTheme {
			d.selectedIdx = i
			break
		}
	}

	d.createInput.SetValue(strings.Join(cfg.OnWorktreeCreate, "\n"))
	d.deleteInput.SetValue(strings.Join(cfg.OnWorktreeDelete, "\n"))
	d.createInput.Placeholder = "One shell command per line"
	d.deleteInput.Placeholder = "One shell command per line"
	d.createInput.FocusedStyle.Placeholder = lipgloss.NewStyle().Foreground(placeholderColor)
	d.createInput.BlurredStyle.Placeholder = d.createInput.FocusedStyle.Placeholder
	d.deleteInput.FocusedStyle.Placeholder = lipgloss.NewStyle().Foreground(placeholderColor)
	d.deleteInput.BlurredStyle.Placeholder = d.deleteInput.FocusedStyle.Placeholder

	d.createInput.Blur()
	d.deleteInput.Blur()
}

// Hide closes the dialog.
func (d *RepoSettingsDialog) Hide() {
	d.visible = false
}

// IsVisible returns true if the dialog is visible.
func (d *RepoSettingsDialog) IsVisible() bool {
	return d.visible
}

// SetSize updates overlay dimensions.
func (d *RepoSettingsDialog) SetSize(w, h int) {
	d.width = w
	d.height = h
}

// RepoSettings returns the current normalized settings from the dialog.
func (d *RepoSettingsDialog) RepoSettings() RepoSettings {
	return RepoSettings{
		OnWorktreeCreate: parseCommandLines(d.createInput.Value()),
		OnWorktreeDelete: parseCommandLines(d.deleteInput.Value()),
	}
}

// SelectedTheme returns the currently highlighted theme.
func (d *RepoSettingsDialog) SelectedTheme() ColorPalette {
	if d.selectedIdx >= 0 && d.selectedIdx < len(d.themes) {
		return d.themes[d.selectedIdx]
	}
	return Dark
}

// OriginalThemeName returns the theme name active when the dialog opened.
func (d *RepoSettingsDialog) OriginalThemeName() string {
	return d.original
}

// FocusTheme puts keyboard focus on theme selection.
func (d *RepoSettingsDialog) FocusTheme() {
	d.setFocus(RepoSettingsFocusTheme)
}

func parseCommandLines(in string) []string {
	var commands []string
	for _, line := range strings.Split(in, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			commands = append(commands, line)
		}
	}
	return commands
}

func (d *RepoSettingsDialog) setFocus(f RepoSettingsDialogFocus) {
	d.focus = f
	switch f {
	case RepoSettingsFocusTheme:
		d.createInput.Blur()
		d.deleteInput.Blur()
	case RepoSettingsFocusCreate:
		d.createInput.Focus()
		d.deleteInput.Blur()
	case RepoSettingsFocusDelete:
		d.deleteInput.Focus()
		d.createInput.Blur()
	default:
		d.createInput.Blur()
		d.deleteInput.Blur()
	}
}

func (d *RepoSettingsDialog) moveFocus(delta int) {
	next := int(d.focus) + delta
	if next < int(RepoSettingsFocusCreate) {
		next = int(RepoSettingsFocusCancel)
	}
	if next > int(RepoSettingsFocusCancel) {
		next = int(RepoSettingsFocusCreate)
	}
	d.setFocus(RepoSettingsDialogFocus(next))
}

// Update handles key presses and returns an action + optional cmd.
func (d *RepoSettingsDialog) Update(msg tea.KeyMsg) (RepoSettingsDialogAction, tea.Cmd) {
	switch msg.String() {
	case "esc":
		return RepoSettingsActionCancel, nil
	case "q", "ctrl+c":
		return RepoSettingsActionQuit, nil
	case "ctrl+enter":
		return RepoSettingsActionSave, nil
	case "tab":
		d.moveFocus(1)
		return RepoSettingsActionNone, nil
	case "shift+tab":
		d.moveFocus(-1)
		return RepoSettingsActionNone, nil
	case "enter":
		switch d.focus {
		case RepoSettingsFocusTheme:
			d.moveFocus(1)
			return RepoSettingsActionNone, nil
		case RepoSettingsFocusSave:
			return RepoSettingsActionSave, nil
		case RepoSettingsFocusCancel:
			return RepoSettingsActionCancel, nil
		}
	case "left", "h":
		if d.focus == RepoSettingsFocusTheme {
			d.moveTheme(-1)
			return RepoSettingsActionNone, nil
		}
	case "right", "l":
		if d.focus == RepoSettingsFocusTheme {
			d.moveTheme(1)
			return RepoSettingsActionNone, nil
		}
	case "up":
		if d.focus == RepoSettingsFocusSave || d.focus == RepoSettingsFocusCancel {
			d.moveFocus(-1)
			return RepoSettingsActionNone, nil
		}
	case "down":
		if d.focus == RepoSettingsFocusSave || d.focus == RepoSettingsFocusCancel {
			d.moveFocus(1)
			return RepoSettingsActionNone, nil
		}
	}

	switch d.focus {
	case RepoSettingsFocusTheme:
		return RepoSettingsActionNone, nil
	case RepoSettingsFocusCreate:
		var cmd tea.Cmd
		d.createInput, cmd = d.createInput.Update(msg)
		return RepoSettingsActionNone, cmd
	case RepoSettingsFocusDelete:
		var cmd tea.Cmd
		d.deleteInput, cmd = d.deleteInput.Update(msg)
		return RepoSettingsActionNone, cmd
	default:
		return RepoSettingsActionNone, nil
	}
}

func (d *RepoSettingsDialog) moveTheme(delta int) {
	if len(d.themes) == 0 {
		return
	}
	d.selectedIdx += delta
	if d.selectedIdx < 0 {
		d.selectedIdx = len(d.themes) - 1
	}
	if d.selectedIdx >= len(d.themes) {
		d.selectedIdx = 0
	}
}

// View renders the dialog.
func (d *RepoSettingsDialog) View(styles *Styles) string {
	title := styles.Title.Render("Repo Settings")
	subtitle := styles.Dim.Render("Repository: " + d.repoName)

	boxWidth := 84
	if d.width > 0 && d.width < 96 {
		boxWidth = d.width - 8
	}
	if boxWidth < 52 {
		boxWidth = 52
	}
	inputWidth := boxWidth - 10
	if inputWidth < 20 {
		inputWidth = 20
	}

	inputHeight := 4
	if d.height > 0 {
		maxInputHeight := (d.height - 18) / 2
		if maxInputHeight > inputHeight {
			inputHeight = maxInputHeight
		}
	}
	if inputHeight < 3 {
		inputHeight = 3
	}
	if inputHeight > 8 {
		inputHeight = 8
	}

	d.createInput.SetWidth(inputWidth)
	d.deleteInput.SetWidth(inputWidth)
	d.createInput.SetHeight(inputHeight)
	d.deleteInput.SetHeight(inputHeight)

	themeLabel := "Color Theme"
	themeValue := d.SelectedTheme().Name
	if d.focus == RepoSettingsFocusTheme {
		themeLabel = styles.Selected.Render(" " + themeLabel + " ")
		themeValue = styles.Selected.Render(" < " + themeValue + " > ")
	} else {
		themeValue = styles.Dim.Render("< " + themeValue + " >")
	}

	createLabel := "On Worktree Create (run after create)"
	deleteLabel := "On Worktree Delete (run before delete)"
	if d.focus == RepoSettingsFocusCreate {
		createLabel = styles.Selected.Render(" " + createLabel + " ")
	}
	if d.focus == RepoSettingsFocusDelete {
		deleteLabel = styles.Selected.Render(" " + deleteLabel + " ")
	}

	saveBtn := styles.Dim.Render("[ Save ]")
	cancelBtn := styles.Dim.Render("[ Cancel ]")
	if d.focus == RepoSettingsFocusSave {
		saveBtn = styles.Selected.Render("[ Save ]")
	}
	if d.focus == RepoSettingsFocusCancel {
		cancelBtn = styles.Selected.Render("[ Cancel ]")
	}

	var b strings.Builder
	b.WriteString(title)
	b.WriteString("\n")
	b.WriteString(subtitle)
	b.WriteString("\n\n")
	b.WriteString(themeLabel)
	b.WriteString("\n")
	b.WriteString(themeValue)
	b.WriteString(" ")
	b.WriteString(styles.Dim.Render("[Left/Right]"))
	b.WriteString("\n\n")
	b.WriteString(createLabel)
	b.WriteString("\n")
	b.WriteString(styles.InputBox.Width(inputWidth + 2).Render(d.createInput.View()))
	b.WriteString("\n\n")
	b.WriteString(deleteLabel)
	b.WriteString("\n")
	b.WriteString(styles.InputBox.Width(inputWidth + 2).Render(d.deleteInput.View()))
	b.WriteString("\n\n")
	b.WriteString(saveBtn + "  " + cancelBtn)
	b.WriteString("\n")
	b.WriteString(styles.Dim.Render("[Tab] Next  [Shift+Tab] Prev  [Ctrl+Enter] Save  [Esc] Cancel"))

	box := styles.ModalBox.Width(boxWidth).Render(b.String())
	if d.width > 0 && d.height > 0 {
		return lipgloss.Place(
			d.width, d.height,
			lipgloss.Center, lipgloss.Center,
			box,
		)
	}
	return box
}
