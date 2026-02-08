package app

import (
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
)

// ToastLevel determines the notification style and auto-dismiss duration.
type ToastLevel int

const (
	ToastSuccess ToastLevel = iota
	ToastInfo
	ToastError
)

// Toast represents a single transient notification.
type Toast struct {
	CreatedAt time.Time
	Message   string
	Duration  time.Duration // auto-dismiss after this duration
	ID        int           // monotonic ID for dismissal targeting
	Level     ToastLevel
}

// IsExpired returns true if the toast has exceeded its duration.
func (t Toast) IsExpired(now time.Time) bool {
	return now.After(t.CreatedAt.Add(t.Duration))
}

// maxToasts is the maximum number of visible toasts.
const maxToasts = 3

// ToastManager manages the notification stack.
type ToastManager struct {
	toasts []Toast
	nextID int
	width  int
}

// NewToastManager creates a new toast manager.
func NewToastManager() *ToastManager {
	return &ToastManager{}
}

// SetWidth sets the rendering width.
func (tm *ToastManager) SetWidth(w int) {
	tm.width = w
}

// Add adds a new toast notification. If the stack exceeds maxToasts,
// the oldest toast is evicted.
func (tm *ToastManager) Add(message string, level ToastLevel) {
	var duration time.Duration
	switch level {
	case ToastSuccess:
		duration = 3 * time.Second
	case ToastInfo:
		duration = 4 * time.Second
	case ToastError:
		duration = 5 * time.Second
	}

	toast := Toast{
		Message:   message,
		Level:     level,
		CreatedAt: time.Now(),
		Duration:  duration,
		ID:        tm.nextID,
	}
	tm.nextID++
	tm.toasts = append(tm.toasts, toast)

	// Evict oldest if over max
	if len(tm.toasts) > maxToasts {
		tm.toasts = tm.toasts[len(tm.toasts)-maxToasts:]
	}
}

// Tick removes expired toasts. Returns true if any were removed
// (caller should schedule next tick if toasts remain).
func (tm *ToastManager) Tick(now time.Time) bool {
	var remaining []Toast
	changed := false
	for _, t := range tm.toasts {
		if t.IsExpired(now) {
			changed = true
		} else {
			remaining = append(remaining, t)
		}
	}
	tm.toasts = remaining
	return changed
}

// HasToasts returns true if there are active toasts.
func (tm *ToastManager) HasToasts() bool {
	return len(tm.toasts) > 0
}

// Count returns the number of active toasts.
func (tm *ToastManager) Count() int {
	return len(tm.toasts)
}

// Height returns the number of lines the toast area will consume.
// Returns 0 if no toasts are active.
func (tm *ToastManager) Height() int {
	if len(tm.toasts) == 0 {
		return 0
	}
	return len(tm.toasts) // Each toast is one line
}

// View renders all active toasts stacked vertically.
// Returns empty string if no toasts are active.
func (tm *ToastManager) View() string {
	if len(tm.toasts) == 0 {
		return ""
	}

	var lines []string
	for _, t := range tm.toasts {
		var style lipgloss.Style
		var icon string
		switch t.Level {
		case ToastSuccess:
			style = toastSuccessStyle
			icon = " ✓ "
		case ToastInfo:
			style = toastInfoStyle
			icon = " i "
		case ToastError:
			style = toastErrorStyle
			icon = " ! "
		}
		content := icon + t.Message
		// Truncate to width (guard against small widths to avoid negative slice)
		if tm.width > 7 && runewidth.StringWidth(content) > tm.width-4 {
			content = truncateVisual(content, tm.width-4)
		}
		lines = append(lines, style.Width(tm.width).Render(content))
	}
	return strings.Join(lines, "\n")
}

var (
	toastSuccessStyle = lipgloss.NewStyle().
				Background(lipgloss.Color("22")). // dark green background
				Foreground(lipgloss.Color("10")). // bright green text
				Padding(0, 1)

	toastInfoStyle = lipgloss.NewStyle().
			Background(lipgloss.Color("17")). // dark blue background
			Foreground(lipgloss.Color("14")). // cyan text
			Padding(0, 1)

	toastErrorStyle = lipgloss.NewStyle().
			Background(lipgloss.Color("52")). // dark red background
			Foreground(lipgloss.Color("9")).  // bright red text
			Padding(0, 1)
)
