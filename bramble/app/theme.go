package app

import (
	"github.com/charmbracelet/lipgloss"
)

// ColorPalette holds all semantic color values for a theme.
// Each field is a string accepted by lipgloss (ANSI 256 number or hex).
type ColorPalette struct {
	Name string

	Accent   string // primary accent (titles, links)
	Dim      string // muted/secondary text
	Border   string // subtle borders
	BarBg    string // top/status bar background
	BarFg    string // top/status bar foreground
	SelectBg string // highlighted row background
	SelectFg string // highlighted row foreground
	Error    string // errors, failures
	Running  string // running/active sessions
	Idle     string // idle/awaiting sessions
	Pending  string // pending/queued sessions
	Done     string // completed/done sessions

	// Toast notification colors (bg, fg pairs)
	ToastSuccessBg string
	ToastSuccessFg string
	ToastInfoBg    string
	ToastInfoFg    string
	ToastErrorBg   string
	ToastErrorFg   string

	// Glamour markdown style: "dark", "light", or "auto"
	GlamourStyle string
}

// Styles holds all pre-computed lipgloss styles derived from a ColorPalette.
type Styles struct {
	Title     lipgloss.Style
	Selected  lipgloss.Style
	Dim       lipgloss.Style
	Error     lipgloss.Style
	TopBar    lipgloss.Style
	StatusBar lipgloss.Style
	Running   lipgloss.Style
	Idle      lipgloss.Style
	Pending   lipgloss.Style
	Completed lipgloss.Style
	Failed    lipgloss.Style
	Border    lipgloss.Style
	InputBox  lipgloss.Style
	ModalBox  lipgloss.Style

	// Toast styles
	ToastSuccess lipgloss.Style
	ToastInfo    lipgloss.Style
	ToastError   lipgloss.Style

	// Help overlay styles
	HelpSectionTitle lipgloss.Style
	HelpKey          lipgloss.Style
	HelpKeyAlign     lipgloss.Style
	HelpBox          lipgloss.Style

	// Welcome screen styles
	WelcomeTitle lipgloss.Style
	WelcomeKey   lipgloss.Style
	WelcomeDesc  lipgloss.Style

	// File tree styles
	FileTreeHeader    lipgloss.Style
	FileTreeHeaderDim lipgloss.Style

	// All sessions overlay
	AllSessionsBox lipgloss.Style

	// Split pane divider
	Divider lipgloss.Style

	// The palette that produced these styles, for reference.
	Palette ColorPalette
}

// NewStyles builds all styles from a ColorPalette.
func NewStyles(p ColorPalette) *Styles {
	accent := lipgloss.Color(p.Accent)
	dim := lipgloss.Color(p.Dim)
	border := lipgloss.Color(p.Border)
	barBg := lipgloss.Color(p.BarBg)
	barFg := lipgloss.Color(p.BarFg)
	selectBg := lipgloss.Color(p.SelectBg)
	selectFg := lipgloss.Color(p.SelectFg)
	errorC := lipgloss.Color(p.Error)
	running := lipgloss.Color(p.Running)
	idle := lipgloss.Color(p.Idle)
	pending := lipgloss.Color(p.Pending)
	done := lipgloss.Color(p.Done)

	return &Styles{
		Palette: p,

		Title: lipgloss.NewStyle().
			Bold(true).
			Foreground(accent),

		Selected: lipgloss.NewStyle().
			Background(selectBg).
			Foreground(selectFg),

		Dim: lipgloss.NewStyle().
			Foreground(dim),

		Error: lipgloss.NewStyle().
			Foreground(errorC),

		TopBar: lipgloss.NewStyle().
			Background(barBg).
			Foreground(barFg).
			Padding(0, 1),

		StatusBar: lipgloss.NewStyle().
			Background(barBg).
			Foreground(dim),

		Running: lipgloss.NewStyle().
			Foreground(running),

		Idle: lipgloss.NewStyle().
			Foreground(idle),

		Pending: lipgloss.NewStyle().
			Foreground(pending),

		Completed: lipgloss.NewStyle().
			Foreground(done),

		Failed: lipgloss.NewStyle().
			Foreground(errorC),

		Border: lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(border).
			BorderLeft(false).
			BorderRight(false),

		InputBox: lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(accent).
			Padding(0, 1),

		ModalBox: lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(accent).
			Padding(1, 2),

		// Toast styles
		ToastSuccess: lipgloss.NewStyle().
			Background(lipgloss.Color(p.ToastSuccessBg)).
			Foreground(lipgloss.Color(p.ToastSuccessFg)).
			Padding(0, 1),

		ToastInfo: lipgloss.NewStyle().
			Background(lipgloss.Color(p.ToastInfoBg)).
			Foreground(lipgloss.Color(p.ToastInfoFg)).
			Padding(0, 1),

		ToastError: lipgloss.NewStyle().
			Background(lipgloss.Color(p.ToastErrorBg)).
			Foreground(lipgloss.Color(p.ToastErrorFg)).
			Padding(0, 1),

		// Help overlay styles
		HelpSectionTitle: lipgloss.NewStyle().Bold(true).Foreground(idle),
		HelpKey:          lipgloss.NewStyle().Bold(true).Foreground(accent),
		HelpKeyAlign:     lipgloss.NewStyle().Width(12).Align(lipgloss.Right),
		HelpBox: lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(border).
			Padding(1, 2),

		// Welcome screen styles
		WelcomeTitle: lipgloss.NewStyle().
			Bold(true).
			Foreground(accent).
			MarginBottom(1),
		WelcomeKey: lipgloss.NewStyle().
			Bold(true).
			Foreground(idle),
		WelcomeDesc: lipgloss.NewStyle().
			Foreground(barFg),

		// File tree styles
		FileTreeHeader: lipgloss.NewStyle().
			Bold(true).
			Foreground(accent).
			BorderBottom(true).
			BorderStyle(lipgloss.NormalBorder()).
			BorderForeground(accent),
		FileTreeHeaderDim: lipgloss.NewStyle().
			Foreground(dim).
			BorderBottom(true).
			BorderStyle(lipgloss.NormalBorder()).
			BorderForeground(dim),

		// All sessions overlay
		AllSessionsBox: lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(border).
			Padding(1, 2),

		// Split pane divider
		Divider: lipgloss.NewStyle().
			Foreground(border),
	}
}

// Built-in palettes.
var (
	// DefaultDark is the default dark-terminal palette with improved readability.
	// Dim uses "245" (was 242) and Done uses "245" (was 8) for better contrast.
	DefaultDark = ColorPalette{
		Name:           "default-dark",
		Accent:         "12",  // bright blue
		Dim:            "245", // improved: was 242, now brighter for readability
		Border:         "240", // subtle lines
		BarBg:          "236", // bar fill
		BarFg:          "252", // bar primary text
		SelectBg:       "240", // highlight row
		SelectFg:       "15",  // white
		Error:          "9",   // bright red
		Running:        "10",  // bright green
		Idle:           "14",  // bright cyan
		Pending:        "11",  // bright yellow
		Done:           "245", // improved: was 8 (near-invisible), now readable gray
		ToastSuccessBg: "22",
		ToastSuccessFg: "10",
		ToastInfoBg:    "17",
		ToastInfoFg:    "14",
		ToastErrorBg:   "52",
		ToastErrorFg:   "9",
		GlamourStyle:   "dark",
	}

	// DefaultLight is the default light-terminal palette.
	DefaultLight = ColorPalette{
		Name:           "default-light",
		Accent:         "4",   // navy
		Dim:            "238", // muted text
		Border:         "248", // subtle lines
		BarBg:          "254", // bar fill
		BarFg:          "236", // bar primary text
		SelectBg:       "195", // light cyan highlight
		SelectFg:       "0",   // black
		Error:          "1",   // dark red
		Running:        "2",   // dark green
		Idle:           "6",   // dark cyan
		Pending:        "3",   // dark yellow
		Done:           "244", // gray
		ToastSuccessBg: "194",
		ToastSuccessFg: "22",
		ToastInfoBg:    "153",
		ToastInfoFg:    "17",
		ToastErrorBg:   "217",
		ToastErrorFg:   "52",
		GlamourStyle:   "light",
	}

	// HighContrast is a high-contrast palette for maximum readability.
	HighContrast = ColorPalette{
		Name:           "high-contrast",
		Accent:         "87",  // bright cyan
		Dim:            "252", // very bright gray
		Border:         "250", // bright border
		BarBg:          "234", // dark bar
		BarFg:          "15",  // white text
		SelectBg:       "21",  // bright blue background
		SelectFg:       "15",  // white
		Error:          "196", // vivid red
		Running:        "46",  // vivid green
		Idle:           "51",  // vivid cyan
		Pending:        "226", // vivid yellow
		Done:           "250", // bright gray
		ToastSuccessBg: "22",
		ToastSuccessFg: "46",
		ToastInfoBg:    "17",
		ToastInfoFg:    "51",
		ToastErrorBg:   "52",
		ToastErrorFg:   "196",
		GlamourStyle:   "dark",
	}

	// BuiltinThemes lists all available built-in themes.
	BuiltinThemes = []ColorPalette{DefaultDark, DefaultLight, HighContrast}
)

// ThemeByName looks up a built-in theme by name.
func ThemeByName(name string) (ColorPalette, bool) {
	for i := range BuiltinThemes {
		if BuiltinThemes[i].Name == name {
			return BuiltinThemes[i], true
		}
	}
	return ColorPalette{}, false
}
