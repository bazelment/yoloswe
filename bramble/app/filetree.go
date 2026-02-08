package app

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/bazelment/yoloswe/wt"
)

var (
	fileTreeHeaderStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("12")).
				BorderBottom(true).
				BorderStyle(lipgloss.NormalBorder()).
				BorderForeground(lipgloss.Color("12"))

	fileTreeHeaderDimStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("242")).
				BorderBottom(true).
				BorderStyle(lipgloss.NormalBorder()).
				BorderForeground(lipgloss.Color("242"))
)

// FileTree displays a navigable tree of changed files from a WorktreeContext.
type FileTree struct {
	root    string
	entries []fileEntry
	files   []fileInfo
	cursor  int
	offset  int
	focused bool
}

// fileInfo holds a file path and its git status indicator.
type fileInfo struct {
	Path   string
	Status string // "M", "A", "D", "?"
}

// fileEntry is a single line in the rendered tree.
type fileEntry struct {
	Display string // rendered display text
	Path    string // full file path (empty for directory headers)
	IsDir   bool
	Depth   int
}

// NewFileTree creates a file tree from a WorktreeContext.
// If wtCtx is nil, an empty tree is created.
func NewFileTree(worktreePath string, wtCtx *wt.WorktreeContext) *FileTree {
	ft := &FileTree{
		root: worktreePath,
	}

	if wtCtx != nil {
		for _, f := range wtCtx.ChangedFiles {
			ft.files = append(ft.files, fileInfo{Path: f, Status: "M"})
		}
		for _, f := range wtCtx.UntrackedFiles {
			ft.files = append(ft.files, fileInfo{Path: f, Status: "?"})
		}
	}

	ft.rebuild()
	return ft
}

// SetContext updates the file tree with new context data.
func (ft *FileTree) SetContext(wtCtx *wt.WorktreeContext) {
	ft.files = nil
	if wtCtx != nil {
		for _, f := range wtCtx.ChangedFiles {
			ft.files = append(ft.files, fileInfo{Path: f, Status: "M"})
		}
		for _, f := range wtCtx.UntrackedFiles {
			ft.files = append(ft.files, fileInfo{Path: f, Status: "?"})
		}
	}
	ft.rebuild()
}

// rebuild flattens the file list into a tree display.
func (ft *FileTree) rebuild() {
	ft.entries = nil

	if len(ft.files) == 0 {
		ft.entries = append(ft.entries, fileEntry{
			Display: dimStyle.Render("  (no changes)"),
		})
		return
	}

	// Sort files by path
	sorted := make([]fileInfo, len(ft.files))
	copy(sorted, ft.files)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Path < sorted[j].Path
	})

	// Group by directory
	dirFiles := make(map[string][]fileInfo)
	var dirs []string
	for _, f := range sorted {
		dir := filepath.Dir(f.Path)
		if _, ok := dirFiles[dir]; !ok {
			dirs = append(dirs, dir)
		}
		dirFiles[dir] = append(dirFiles[dir], f)
	}

	// Check if all files are in a single root directory (skip header if so)
	singleRoot := len(dirs) == 1
	isRootOnly := false
	if singleRoot {
		displayDir := dirs[0]
		if ft.root != "" {
			if rel, err := filepath.Rel(ft.root, displayDir); err == nil {
				displayDir = rel
			}
		}
		isRootOnly = displayDir == "."
	}

	for _, dir := range dirs {
		displayDir := dir
		if ft.root != "" {
			if rel, err := filepath.Rel(ft.root, dir); err == nil {
				displayDir = rel
			}
		}
		if displayDir == "." {
			displayDir = ""
		}

		// Skip directory header if all files are at root level
		if !(isRootOnly && displayDir == "") {
			if displayDir == "" {
				displayDir = "."
			}
			ft.entries = append(ft.entries, fileEntry{
				Display: dimStyle.Render("  " + displayDir + "/"),
				IsDir:   true,
				Depth:   0,
			})
		}

		// Files in directory
		indent := "    "
		if isRootOnly && displayDir == "" {
			indent = "  " // Less indent when no directory header
		}
		for _, f := range dirFiles[dir] {
			name := filepath.Base(f.Path)
			statusColor := statusIndicator(f.Status)
			ft.entries = append(ft.entries, fileEntry{
				Display: fmt.Sprintf("%s%s %s", indent, statusColor, name),
				Path:    f.Path,
				Depth:   1,
			})
		}
	}

	// Reset cursor if out of bounds
	if ft.cursor >= len(ft.entries) {
		ft.cursor = len(ft.entries) - 1
	}
	if ft.cursor < 0 {
		ft.cursor = 0
	}
}

// statusIndicator returns a colored status character.
func statusIndicator(status string) string {
	switch status {
	case "M":
		return runningStyle.Render("M")
	case "A":
		return completedStyle.Render("A")
	case "D":
		return failedStyle.Render("D")
	case "?":
		return pendingStyle.Render("?")
	default:
		return dimStyle.Render(status)
	}
}

// MoveUp moves the cursor up.
func (ft *FileTree) MoveUp() {
	if ft.cursor > 0 {
		ft.cursor--
	}
}

// MoveDown moves the cursor down.
func (ft *FileTree) MoveDown() {
	if ft.cursor < len(ft.entries)-1 {
		ft.cursor++
	}
}

// SelectedPath returns the path of the currently selected entry,
// or empty string if a directory or no selection.
func (ft *FileTree) SelectedPath() string {
	if ft.cursor >= 0 && ft.cursor < len(ft.entries) {
		return ft.entries[ft.cursor].Path
	}
	return ""
}

// SetFocused sets whether the file tree pane has focus.
func (ft *FileTree) SetFocused(focused bool) {
	ft.focused = focused
}

// FileCount returns the number of files in the tree.
func (ft *FileTree) FileCount() int {
	return len(ft.files)
}

// Render draws the file tree within the given dimensions.
func (ft *FileTree) Render(width, height int) string {
	var b strings.Builder

	// Title with file count, rendered with bottom border via lipgloss
	titleText := "Files"
	if len(ft.files) > 0 {
		titleText = fmt.Sprintf("Files (%d)", len(ft.files))
	}
	headerStyle := fileTreeHeaderDimStyle
	if ft.focused {
		headerStyle = fileTreeHeaderStyle
	}
	b.WriteString(headerStyle.Width(width).Render(titleText))
	b.WriteString("\n")

	contentHeight := height - 2 // title (with border) takes 2 lines

	// Adjust scroll offset to keep cursor visible
	if ft.cursor < ft.offset {
		ft.offset = ft.cursor
	}
	if ft.cursor >= ft.offset+contentHeight {
		ft.offset = ft.cursor - contentHeight + 1
	}

	// Render visible entries
	for i := ft.offset; i < ft.offset+contentHeight && i < len(ft.entries); i++ {
		entry := ft.entries[i]
		line := entry.Display

		// Highlight cursor
		if i == ft.cursor {
			line = selectedStyle.Render(stripAnsi(line))
		}

		// Truncate to width
		stripped := stripAnsi(line)
		if len(stripped) > width {
			line = line[:width-3] + "..."
		}

		b.WriteString(line)
		b.WriteString("\n")
	}

	return b.String()
}
