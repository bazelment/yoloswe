package main

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/bazelment/yoloswe/bramble/session"
)

func TestRunesTruncate(t *testing.T) {
	t.Parallel()

	if got := runesTruncate("ab😀cd", 4); got != "ab😀c" {
		t.Fatalf("runesTruncate() = %q, want %q", got, "ab😀c")
	}
	if got := runesTruncate("short", 10); got != "short" {
		t.Fatalf("runesTruncate() changed short string to %q", got)
	}
	if got := runesTruncate("😀😀", 1); !utf8.ValidString(got) || got != "😀" {
		t.Fatalf("runesTruncate() = %q, want one valid emoji", got)
	}
}

func TestRunesPad(t *testing.T) {
	t.Parallel()

	got := runesPad("é", 3)
	if got != "é  " {
		t.Fatalf("runesPad() = %q, want %q", got, "é  ")
	}
	if utf8.RuneCountInString(got) != 3 {
		t.Fatalf("runesPad() rune count = %d, want 3", utf8.RuneCountInString(got))
	}
	if got := runesPad("already-long", 4); got != "already-long" {
		t.Fatalf("runesPad() truncated string to %q", got)
	}
}

func TestRenderStatusBox(t *testing.T) {
	t.Parallel()

	box := renderStatusBox(
		tmuxWindow{Index: "1", ID: "@7", Name: "claude"},
		[]string{"planning", "running tests"},
		&session.PaneStatus{
			Model:       "Opus 4.6",
			Branch:      "feature/refactor",
			ContextPct:  "42%",
			TokenCount:  "20k",
			PRNumber:    "123",
			Permissions: "bypass permissions on",
			StatusLine:  "✻ Worked for 1m",
			IsIdle:      true,
			SepIdx:      -1,
		},
		90,
	)

	for _, want := range []string{
		"Window 1 [@7] claude",
		"Model: Opus 4.6",
		"IDLE (awaiting input)",
		"PR: #123",
		"Perms: bypass permissions on",
		"Status: ✻ Worked for 1m",
		"planning",
		"running tests",
	} {
		if !strings.Contains(box, want) {
			t.Fatalf("rendered box missing %q:\n%s", want, box)
		}
	}
}

func TestRenderStatusBoxWithoutStatus(t *testing.T) {
	t.Parallel()

	box := renderStatusBox(tmuxWindow{Index: "2", ID: "@8", Name: "other"}, nil, nil, 30)
	if !strings.Contains(box, "(no status bar parsed)") {
		t.Fatalf("rendered box missing no-status message:\n%s", box)
	}
}
