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
// The four RGB palettes (Dark, Light, DarkDaltonized, LightDaltonized) use
// "#RRGGBB" hex strings so lipgloss renders exact 24-bit colour.
// The two ANSI palettes (DarkAnsi, LightAnsi) use basic-16 colour numbers
// so they adapt to the user's terminal colour scheme.
var (
	// Dark is the default dark palette, based on Claude Code's dark theme.
	Dark = ColorPalette{
		Name:           "dark",
		Accent:         "#D77757", // terracotta orange (claude)
		Dim:            "#999999", // medium gray (inactive)
		Border:         "#505050", // dark gray (subtle)
		BarBg:          "#373737", // dark gray (userMessageBackground)
		BarFg:          "#FFFFFF", // white (text)
		SelectBg:       "#606060", // slightly brighter than subtle
		SelectFg:       "#FFFFFF", // white (text)
		Error:          "#FF6B80", // coral red (error)
		Running:        "#4EBA65", // green (success)
		Idle:           "#B1B9F9", // periwinkle (permission)
		Pending:        "#FFC107", // amber (warning)
		Done:           "#999999", // medium gray (inactive)
		ToastSuccessBg: "#1A3D1F", // darker green
		ToastSuccessFg: "#4EBA65", // green (success)
		ToastInfoBg:    "#2A2D4A", // darker periwinkle
		ToastInfoFg:    "#B1B9F9", // periwinkle (permission)
		ToastErrorBg:   "#4A1A22", // darker red
		ToastErrorFg:   "#FF6B80", // coral red (error)
		GlamourStyle:   "dark",
	}

	// Light is the light palette, based on Claude Code's light theme.
	Light = ColorPalette{
		Name:           "light",
		Accent:         "#D77757", // terracotta orange (claude)
		Dim:            "#666666", // dark gray (inactive)
		Border:         "#AFAFAF", // medium gray (subtle)
		BarBg:          "#F0F0F0", // light gray (userMessageBackground)
		BarFg:          "#000000", // black (text)
		SelectBg:       "#C0C0C0", // slightly brighter than subtle
		SelectFg:       "#000000", // black (text)
		Error:          "#AB2B3F", // dark red (error)
		Running:        "#2C7A39", // dark green (success)
		Idle:           "#5769F7", // blue-violet (permission)
		Pending:        "#966C1E", // dark amber (warning)
		Done:           "#666666", // dark gray (inactive)
		ToastSuccessBg: "#D4EDDA", // pale green
		ToastSuccessFg: "#2C7A39", // dark green (success)
		ToastInfoBg:    "#D6DAFE", // pale periwinkle
		ToastInfoFg:    "#5769F7", // blue-violet (permission)
		ToastErrorBg:   "#F5D0D6", // pale red
		ToastErrorFg:   "#AB2B3F", // dark red (error)
		GlamourStyle:   "light",
	}

	// DarkDaltonized is a colorblind-friendly dark palette.
	// Key difference: success uses blue instead of green.
	DarkDaltonized = ColorPalette{
		Name:           "dark-daltonized",
		Accent:         "#FF9933", // bright orange
		Dim:            "#999999", // medium gray
		Border:         "#505050", // dark gray
		BarBg:          "#373737", // dark gray
		BarFg:          "#FFFFFF", // white
		SelectBg:       "#606060", // slightly brighter than subtle
		SelectFg:       "#FFFFFF", // white
		Error:          "#FF6666", // bright red
		Running:        "#3399FF", // blue (NOT green)
		Idle:           "#99CCFF", // light sky blue
		Pending:        "#FFCC00", // bright yellow
		Done:           "#999999", // medium gray
		ToastSuccessBg: "#1A2D4A", // darker blue
		ToastSuccessFg: "#3399FF", // blue (success)
		ToastInfoBg:    "#2A3D5A", // darker sky blue
		ToastInfoFg:    "#99CCFF", // light sky blue
		ToastErrorBg:   "#4A1A1A", // darker red
		ToastErrorFg:   "#FF6666", // bright red
		GlamourStyle:   "dark",
	}

	// LightDaltonized is a colorblind-friendly light palette.
	// Key difference: success uses teal-blue instead of green.
	LightDaltonized = ColorPalette{
		Name:           "light-daltonized",
		Accent:         "#FF9933", // bright orange
		Dim:            "#666666", // dark gray
		Border:         "#AFAFAF", // medium gray
		BarBg:          "#F0F0F0", // light gray
		BarFg:          "#000000", // black
		SelectBg:       "#C0C0C0", // slightly brighter than subtle
		SelectFg:       "#000000", // black
		Error:          "#CC0000", // pure red
		Running:        "#006699", // dark teal-blue
		Idle:           "#3366FF", // blue
		Pending:        "#FF9900", // orange
		Done:           "#666666", // dark gray
		ToastSuccessBg: "#CCE5F0", // pale teal
		ToastSuccessFg: "#006699", // dark teal-blue
		ToastInfoBg:    "#D6DEFF", // pale blue
		ToastInfoFg:    "#3366FF", // blue
		ToastErrorBg:   "#F5CCCC", // pale red
		ToastErrorFg:   "#CC0000", // pure red
		GlamourStyle:   "light",
	}

	// DarkAnsi is an ANSI-only dark palette that adapts to the terminal's
	// colour scheme. Uses basic-16 colour numbers (0-15).
	DarkAnsi = ColorPalette{
		Name:           "dark-ansi",
		Accent:         "9",   // red bright (ansi)
		Dim:            "7",   // white / silver (ansi)
		Border:         "7",   // white / silver (ansi)
		BarBg:          "236", // dark gray (extended, safe on dark terms)
		BarFg:          "15",  // bright white (ansi)
		SelectBg:       "240", // medium gray (extended)
		SelectFg:       "15",  // bright white (ansi)
		Error:          "9",   // red bright (ansi)
		Running:        "10",  // green bright (ansi)
		Idle:           "12",  // blue bright (ansi)
		Pending:        "11",  // yellow bright (ansi)
		Done:           "7",   // white / silver (ansi)
		ToastSuccessBg: "22",  // dark green (extended)
		ToastSuccessFg: "10",  // green bright (ansi)
		ToastInfoBg:    "17",  // dark blue (extended)
		ToastInfoFg:    "12",  // blue bright (ansi)
		ToastErrorBg:   "52",  // dark red (extended)
		ToastErrorFg:   "9",   // red bright (ansi)
		GlamourStyle:   "dark",
	}

	// LightAnsi is an ANSI-only light palette that adapts to the terminal's
	// colour scheme. Uses basic-16 colour numbers (0-15).
	LightAnsi = ColorPalette{
		Name:           "light-ansi",
		Accent:         "9",   // red bright (ansi)
		Dim:            "8",   // black bright / dark gray (ansi)
		Border:         "8",   // black bright / dark gray (ansi)
		BarBg:          "254", // light gray (extended, safe on light terms)
		BarFg:          "0",   // black (ansi)
		SelectBg:       "195", // light cyan (extended)
		SelectFg:       "0",   // black (ansi)
		Error:          "1",   // red (ansi)
		Running:        "2",   // green (ansi)
		Idle:           "4",   // blue (ansi)
		Pending:        "3",   // yellow (ansi)
		Done:           "8",   // black bright / dark gray (ansi)
		ToastSuccessBg: "194", // pale green (extended)
		ToastSuccessFg: "2",   // green (ansi)
		ToastInfoBg:    "153", // pale blue (extended)
		ToastInfoFg:    "4",   // blue (ansi)
		ToastErrorBg:   "217", // pale red (extended)
		ToastErrorFg:   "1",   // red (ansi)
		GlamourStyle:   "light",
	}

	// BuiltinThemes lists all available built-in themes.
	BuiltinThemes = []ColorPalette{Dark, Light, DarkDaltonized, LightDaltonized, DarkAnsi, LightAnsi}
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
