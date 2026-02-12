package app

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/bazelment/yoloswe/multiagent/agent"
)

// RepoSettingsDialogFocus indicates which control is focused.
type RepoSettingsDialogFocus int

const (
	RepoSettingsFocusTheme     RepoSettingsDialogFocus = iota
	RepoSettingsFocusProviders                         // Provider toggle section
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
	createInput      textarea.Model
	deleteInput      textarea.Model
	themes           []ColorPalette
	providerStatuses []agent.ProviderStatus
	enabledProviders map[string]bool
	repoName         string
	original         string
	width            int
	height           int
	selectedIdx      int
	providerCursor   int // which provider row is highlighted
	focus            RepoSettingsDialogFocus
	visible          bool
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
func (d *RepoSettingsDialog) Show(repoName string, cfg RepoSettings, currentTheme string, w, h int, placeholderColor lipgloss.Color, providerStatuses []agent.ProviderStatus, enabledProviders []string) {
	d.repoName = repoName
	d.width = w
	d.height = h
	d.visible = true
	d.focus = RepoSettingsFocusTheme
	d.original = currentTheme
	d.selectedIdx = 0
	d.providerCursor = 0
	for i := range d.themes {
		if d.themes[i].Name == currentTheme {
			d.selectedIdx = i
			break
		}
	}

	// Initialize provider statuses and enabled map
	d.providerStatuses = providerStatuses
	d.enabledProviders = make(map[string]bool, len(agent.AllProviders))
	if len(enabledProviders) == 0 {
		// nil/empty = all enabled
		for _, s := range providerStatuses {
			d.enabledProviders[s.Provider] = true
		}
	} else {
		for _, p := range enabledProviders {
			d.enabledProviders[p] = true
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

// EnabledProviders returns the list of enabled provider names from the dialog.
// Returns nil only when no provider status data is available (backward compat).
// Returns a non-nil empty slice when all providers are explicitly disabled.
func (d *RepoSettingsDialog) EnabledProviders() []string {
	if len(d.providerStatuses) == 0 {
		return nil // no availability data → treat as all enabled
	}
	result := make([]string, 0, len(d.providerStatuses))
	for _, s := range d.providerStatuses {
		if d.enabledProviders[s.Provider] {
			result = append(result, s.Provider)
		}
	}
	// If all are enabled, return nil to mean "all" (backward compat)
	if len(result) == len(d.providerStatuses) {
		return nil
	}
	return result
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
	case RepoSettingsFocusTheme, RepoSettingsFocusProviders:
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
	if next < int(RepoSettingsFocusTheme) {
		next = int(RepoSettingsFocusCancel)
	}
	if next > int(RepoSettingsFocusCancel) {
		next = int(RepoSettingsFocusTheme)
	}
	d.setFocus(RepoSettingsDialogFocus(next))
}

// Update handles key presses and returns an action + optional cmd.
func (d *RepoSettingsDialog) Update(msg tea.KeyMsg) (RepoSettingsDialogAction, tea.Cmd) {
	switch msg.String() {
	case "esc":
		return RepoSettingsActionCancel, nil
	case "ctrl+c":
		return RepoSettingsActionQuit, nil
	case "q":
		// Only quit if not focused on text inputs or providers
		if d.focus != RepoSettingsFocusCreate && d.focus != RepoSettingsFocusDelete && d.focus != RepoSettingsFocusProviders {
			return RepoSettingsActionQuit, nil
		}
	case "ctrl+enter":
		return RepoSettingsActionSave, nil
	case "tab":
		d.moveFocus(1)
		return RepoSettingsActionNone, nil
	case "shift+tab":
		d.moveFocus(-1)
		return RepoSettingsActionNone, nil
	case " ":
		// Space toggles provider enabled state
		if d.focus == RepoSettingsFocusProviders && len(d.providerStatuses) > 0 {
			ps := d.providerStatuses[d.providerCursor]
			if ps.Installed {
				d.enabledProviders[ps.Provider] = !d.enabledProviders[ps.Provider]
			}
			return RepoSettingsActionNone, nil
		}
	case "enter":
		switch d.focus {
		case RepoSettingsFocusTheme:
			d.moveFocus(1)
			return RepoSettingsActionNone, nil
		case RepoSettingsFocusProviders:
			// Enter toggles the selected provider (same as space)
			if len(d.providerStatuses) > 0 {
				ps := d.providerStatuses[d.providerCursor]
				if ps.Installed {
					d.enabledProviders[ps.Provider] = !d.enabledProviders[ps.Provider]
				}
			}
			return RepoSettingsActionNone, nil
		case RepoSettingsFocusSave:
			return RepoSettingsActionSave, nil
		case RepoSettingsFocusCancel:
			return RepoSettingsActionCancel, nil
		}
	case "left", "h":
		if d.focus == RepoSettingsFocusTheme {
			d.moveThemeGrid(0, -1)
			return RepoSettingsActionNone, nil
		}
	case "right", "l":
		if d.focus == RepoSettingsFocusTheme {
			d.moveThemeGrid(0, 1)
			return RepoSettingsActionNone, nil
		}
	case "up":
		if d.focus == RepoSettingsFocusTheme {
			d.moveThemeGrid(-1, 0)
			return RepoSettingsActionNone, nil
		}
		if d.focus == RepoSettingsFocusProviders {
			if d.providerCursor > 0 {
				d.providerCursor--
			} else {
				d.moveFocus(-1) // Move to theme section
			}
			return RepoSettingsActionNone, nil
		}
		if d.focus == RepoSettingsFocusSave || d.focus == RepoSettingsFocusCancel {
			d.moveFocus(-1)
			return RepoSettingsActionNone, nil
		}
	case "down":
		if d.focus == RepoSettingsFocusTheme {
			d.moveThemeGrid(1, 0)
			return RepoSettingsActionNone, nil
		}
		if d.focus == RepoSettingsFocusProviders {
			if d.providerCursor < len(d.providerStatuses)-1 {
				d.providerCursor++
			} else {
				d.moveFocus(1) // Move to create section
			}
			return RepoSettingsActionNone, nil
		}
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

// themeGridCols returns how many columns the theme grid should have based on
// the current dialog width.
func (d *RepoSettingsDialog) themeGridCols() int {
	boxWidth := 84
	if d.width > 0 && d.width < 96 {
		boxWidth = d.width - 8
	}
	if boxWidth < 52 {
		boxWidth = 52
	}
	innerWidth := boxWidth - 6 // ModalBox padding (2*2) + border (2)
	cols := innerWidth / 25
	if cols < 1 {
		cols = 1
	}
	if cols > 3 {
		cols = 3
	}
	return cols
}

// moveThemeGrid navigates the theme selection in a 2D grid layout.
func (d *RepoSettingsDialog) moveThemeGrid(dRow, dCol int) {
	n := len(d.themes)
	if n == 0 {
		return
	}
	cols := d.themeGridCols()
	row := d.selectedIdx / cols
	col := d.selectedIdx % cols
	rows := (n + cols - 1) / cols

	col += dCol
	if col < 0 {
		col = cols - 1
	}
	if col >= cols {
		col = 0
	}

	row += dRow
	if row < 0 {
		row = rows - 1
	}
	if row >= rows {
		row = 0
	}

	idx := row*cols + col
	// Clamp to valid range (last row may be short)
	if idx >= n {
		idx = n - 1
	}
	d.selectedIdx = idx
}

// renderThemeSwatch renders a single theme swatch showing the theme name and
// colored preview dots inside a rounded border box.
func renderThemeSwatch(palette ColorPalette, width int, selected, current bool, appStyles *Styles) string {
	borderColor := lipgloss.Color(palette.Border)
	if selected {
		borderColor = lipgloss.Color(appStyles.Palette.Accent)
	}

	nameStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(palette.Accent)).Bold(true)
	name := palette.Name
	if current {
		name += "*"
	}

	dot := func(c string) string {
		return lipgloss.NewStyle().Foreground(lipgloss.Color(c)).Render("\u25cf")
	}
	dots := strings.Join([]string{
		dot(palette.Running),
		dot(palette.Error),
		dot(palette.Idle),
		dot(palette.Pending),
		dot(palette.Dim),
	}, " ")

	contentWidth := width - 4 // border (2) + padding (2)
	if contentWidth < 8 {
		contentWidth = 8
	}

	renderedName := nameStyle.Render(name)
	content := renderedName + "\n" + dots

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(borderColor).
		Padding(0, 1).
		Width(contentWidth).
		Render(content)

	return box
}

// renderThemeGrid builds the full grid of theme swatches.
func (d *RepoSettingsDialog) renderThemeGrid(boxWidth int, styles *Styles) string {
	cols := d.themeGridCols()
	n := len(d.themes)
	rows := (n + cols - 1) / cols

	swatchWidth := (boxWidth - 6) / cols // distribute inner width evenly
	if swatchWidth < 12 {
		swatchWidth = 12
	}

	var rowStrings []string
	for r := 0; r < rows; r++ {
		var swatches []string
		for c := 0; c < cols; c++ {
			idx := r*cols + c
			if idx >= n {
				// Pad with empty space to keep grid aligned
				swatches = append(swatches, strings.Repeat(" ", swatchWidth))
				continue
			}
			selected := idx == d.selectedIdx
			current := d.themes[idx].Name == d.original
			swatches = append(swatches, renderThemeSwatch(d.themes[idx], swatchWidth, selected, current, styles))
		}
		rowStrings = append(rowStrings, lipgloss.JoinHorizontal(lipgloss.Top, swatches...))
	}
	return lipgloss.JoinVertical(lipgloss.Left, rowStrings...)
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
		maxInputHeight := (d.height - 26) / 2
		if maxInputHeight > inputHeight {
			inputHeight = maxInputHeight
		}
	}
	if inputHeight < 2 {
		inputHeight = 2
	}
	if inputHeight > 8 {
		inputHeight = 8
	}

	d.createInput.SetWidth(inputWidth)
	d.deleteInput.SetWidth(inputWidth)
	d.createInput.SetHeight(inputHeight)
	d.deleteInput.SetHeight(inputHeight)

	themeLabel := "Color Theme"
	if d.focus == RepoSettingsFocusTheme {
		themeLabel = styles.Selected.Render(" " + themeLabel + " ")
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
	b.WriteString(d.renderThemeGrid(boxWidth, styles))
	if d.focus == RepoSettingsFocusTheme {
		b.WriteString("\n")
		b.WriteString(styles.Dim.Render("[Arrow Keys] Navigate"))
	}
	b.WriteString("\n\n")

	// Providers section
	providerLabel := "Providers"
	if d.focus == RepoSettingsFocusProviders {
		providerLabel = styles.Selected.Render(" " + providerLabel + " ")
	}
	b.WriteString(providerLabel)
	b.WriteString("\n")
	for i, ps := range d.providerStatuses {
		checkbox := "[ ]"
		if d.enabledProviders[ps.Provider] {
			checkbox = "[x]"
		}
		status := styles.Failed.Render("not found")
		if ps.Installed {
			ver := ps.Version
			if ver == "" {
				ver = "installed"
			}
			status = styles.Completed.Render(ver)
		}
		line := fmt.Sprintf("  %s %-8s %s", checkbox, ps.Provider, status)
		if d.focus == RepoSettingsFocusProviders && i == d.providerCursor {
			line = styles.Selected.Render(line)
		} else if !ps.Installed {
			line = styles.Dim.Render(line)
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	if d.focus == RepoSettingsFocusProviders {
		b.WriteString(styles.Dim.Render("  [Space/Enter] toggle  [Up/Down] navigate"))
		b.WriteString("\n")
	}
	b.WriteString("\n")
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
