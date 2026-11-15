package chat

import (
	"context"
	"embed"
	"io"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/exp/teatest/v2"
	"github.com/mattn/go-runewidth"

	"github.com/bazelment/yoloswe/orca/clock"
	"github.com/bazelment/yoloswe/orca/controller"
	"github.com/bazelment/yoloswe/orca/model"
	"github.com/bazelment/yoloswe/orca/router"
	"github.com/bazelment/yoloswe/orca/store"
)

//go:embed testdata/*.golden
var goldenFS embed.FS

// fakeTerminal emulates a fixed-size VT100/ANSI terminal screen so tests can
// verify what remains visible on screen (and in scrollback) after Bubble Tea
// cursor movements, line insertions, and erases.
type fakeTerminal struct {
	cells         [][]rune
	scrollback    []string
	w, h          int
	row, col      int
	incompleteEsc bool
}

func newFakeTerminal(w, h int) *fakeTerminal {
	t := &fakeTerminal{
		w:     w,
		h:     h,
		cells: make([][]rune, h),
	}
	for r := range h {
		t.cells[r] = t.blankRow()
	}
	return t
}

func (t *fakeTerminal) blankRow() []rune {
	row := make([]rune, t.w)
	for i := range row {
		row[i] = ' '
	}
	return row
}

func (t *fakeTerminal) scrollUp() {
	t.scrollback = append(t.scrollback, strings.TrimRight(string(t.cells[0]), " "))
	copy(t.cells[:t.h-1], t.cells[1:])
	t.cells[t.h-1] = t.blankRow()
}

func (t *fakeTerminal) write(s string) {
	t.incompleteEsc = false
	runes := []rune(s)
	for i := 0; i < len(runes); {
		r := runes[i]
		if r == '\x1b' {
			if i+1 >= len(runes) {
				t.incompleteEsc = true
				return
			}
			next := runes[i+1]
			if next == 'M' {
				if t.row > 0 {
					t.row--
				}
				i += 2
				continue
			}
			if next == ']' {
				j := i + 2
				terminated := false
				for j < len(runes) {
					if runes[j] == '\x07' || runes[j] == 0x9c {
						j++
						terminated = true
						break
					}
					if runes[j] == '\x1b' {
						if j+1 >= len(runes) {
							t.incompleteEsc = true
							return
						}
						if runes[j+1] == '\\' {
							j += 2
							terminated = true
							break
						}
					}
					j++
				}
				if !terminated {
					t.incompleteEsc = true
					return
				}
				i = j
				continue
			}
			if next == '[' {
				j := i + 2
				for j < len(runes) && (runes[j] < 0x40 || runes[j] > 0x7e) {
					j++
				}
				if j >= len(runes) {
					t.incompleteEsc = true
					return
				}
				params := string(runes[i+2 : j])
				cmd := runes[j]
				t.handleCSI(params, cmd)
				i = j + 1
				continue
			}
			i += 2
			continue
		}
		switch r {
		case '\r':
			t.col = 0
		case '\n':
			t.col = 0
			if t.row+1 >= t.h {
				t.scrollUp()
			} else {
				t.row++
			}
		case '\b':
			if t.col > 0 {
				t.col--
			}
		default:
			if r >= 0x20 {
				rw := max(runewidth.RuneWidth(r), 1)
				if t.col+rw > t.w {
					t.col = 0
					if t.row+1 >= t.h {
						t.scrollUp()
					} else {
						t.row++
					}
				}
				if t.row >= 0 && t.row < t.h && t.col >= 0 && t.col < t.w {
					t.cells[t.row][t.col] = r
					for k := 1; k < rw && t.col+k < t.w; k++ {
						t.cells[t.row][t.col+k] = 0
					}
				}
				t.col += rw
			}
		}
		i++
	}
}

func (t *fakeTerminal) handleCSI(params string, cmd rune) {
	if strings.HasPrefix(params, "?") || strings.HasPrefix(params, ">") {
		return
	}
	parseNum := func(s string, def int) int {
		if s == "" {
			return def
		}
		n, err := strconv.Atoi(s)
		if err != nil || n <= 0 {
			return def
		}
		return n
	}
	switch cmd {
	case 'A':
		t.row = max(t.row-parseNum(params, 1), 0)
	case 'B':
		t.row = min(t.row+parseNum(params, 1), t.h-1)
	case 'C':
		t.col = min(t.col+parseNum(params, 1), t.w-1)
	case 'D':
		t.col = max(t.col-parseNum(params, 1), 0)
	case 'd':
		t.row = min(max(parseNum(params, 1)-1, 0), t.h-1)
	case 'G':
		t.col = min(max(parseNum(params, 1)-1, 0), t.w-1)
	case 'H', 'f':
		parts := strings.Split(params, ";")
		r, c := 1, 1
		if len(parts) >= 1 {
			r = parseNum(parts[0], 1)
		}
		if len(parts) >= 2 {
			c = parseNum(parts[1], 1)
		}
		t.row = min(max(r-1, 0), t.h-1)
		t.col = min(max(c-1, 0), t.w-1)
	case 'J':
		mode, _ := strconv.Atoi(params)
		switch mode {
		case 0:
			for c := t.col; c < t.w; c++ {
				t.cells[t.row][c] = ' '
			}
			for r := t.row + 1; r < t.h; r++ {
				t.cells[r] = t.blankRow()
			}
		case 2:
			for r := 0; r < t.h; r++ {
				t.cells[r] = t.blankRow()
			}
		}
	case 'K':
		mode, _ := strconv.Atoi(params)
		switch mode {
		case 0:
			for c := t.col; c < t.w; c++ {
				t.cells[t.row][c] = ' '
			}
		case 2:
			t.cells[t.row] = t.blankRow()
		}
	case 'L':
		n := parseNum(params, 1)
		for range n {
			if t.row < t.h {
				copy(t.cells[t.row+1:], t.cells[t.row:t.h-1])
				t.cells[t.row] = t.blankRow()
			}
		}
	case 'M':
		n := parseNum(params, 1)
		for range n {
			if t.row < t.h {
				copy(t.cells[t.row:t.h-1], t.cells[t.row+1:])
				t.cells[t.h-1] = t.blankRow()
			}
		}
	}
}

func (t *fakeTerminal) visibleLines() []string {
	out := make([]string, t.h)
	for r := range t.h {
		var b strings.Builder
		for _, ch := range t.cells[r] {
			if ch != 0 {
				b.WriteRune(ch)
			}
		}
		out[r] = strings.TrimRight(b.String(), " ")
	}
	return out
}

func (t *fakeTerminal) nonEmptyLines() []string {
	var out []string
	for _, line := range t.visibleLines() {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}

func (t *fakeTerminal) hasTurnSpinner() bool {
	for _, line := range t.visibleLines() {
		if strings.HasPrefix(line, ">") {
			break
		}
		for _, g := range SpinnerFrames {
			if strings.HasPrefix(line, "  "+g+" ") {
				return true
			}
		}
	}
	return false
}

func waitScreenModel(t *testing.T, tm *teatest.TestModel, m Model, vt **fakeTerminal, rawAll *strings.Builder, w, h int, substrs ...string) {
	t.Helper()
	teatest.WaitFor(t, tm.Output(), func(p []byte) bool {
		probe := newFakeTerminal(w, h)
		probe.write(rawAll.String() + string(p))
		if probe.incompleteEsc {
			return false
		}
		settled, busy := m.isSettled()
		if !settled {
			return false
		}
		if !busy && probe.hasTurnSpinner() {
			return false
		}
		screen := strings.Join(probe.nonEmptyLines(), "\n")
		for _, sub := range substrs {
			if !strings.Contains(screen, sub) {
				return false
			}
		}
		rawAll.Write(p)
		if vt != nil {
			*vt = probe
		}
		return true
	}, teatest.WithDuration(5*time.Second), teatest.WithCheckInterval(5*time.Millisecond))
}

func TestInnerLoopReply60ColsGolden(t *testing.T) {
	t.Parallel()
	reply := strings.Join([]string{
		"**Goal:** verify whether that flag needs action before the release cut.",
		"",
		"- **Status:** lane `orca-ui` is running; checking whether that flag needs action.",
		"- **Next:** inspect [design doc](https://example.com/orca) and run tests.",
	}, "\n")

	for _, tc := range []struct {
		name       string
		goldenPath string
		wantScreen []string
		width      int
	}{
		{
			name:       "60-cols",
			width:      60,
			goldenPath: "testdata/reply_60.golden",
			wantScreen: []string{
				"orca> Goal: verify whether that flag needs action before",
				"      the release cut.",
				"      • Status: lane orca-ui is running; checking whether",
				"        that flag needs action.",
				"      • Next: inspect design doc (https://example.com/orca)",
				"        and run tests.",
			},
		},
		{
			name:       "188-cols",
			width:      188,
			goldenPath: "testdata/reply_188.golden",
			wantScreen: []string{
				"orca> Goal: verify whether that flag needs action before the release cut.",
				"      • Status: lane orca-ui is running; checking whether that flag needs action.",
				"      • Next: inspect design doc (https://example.com/orca) and run tests.",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotLines := FormatReply(reply, tc.width, true)
			got := strings.Join(gotLines, "\n") + "\n"

			wantBytes, err := goldenFS.ReadFile(tc.goldenPath)
			if err != nil {
				t.Fatalf("read golden %s: %v", tc.goldenPath, err)
			}
			want := string(wantBytes)
			if got != want {
				t.Fatalf("FormatReply(%d, tty=true) mismatch:\ngot:\n%q\nwant:\n%q", tc.width, got, want)
			}

			term := newFakeTerminal(tc.width, 20)
			for _, line := range gotLines {
				term.write(line + "\r\n")
			}
			screen := strings.Join(term.nonEmptyLines(), "\n")
			wantScreen := strings.Join(tc.wantScreen, "\n")
			if screen != wantScreen {
				t.Fatalf("%d-col fake terminal screen mismatch:\ngot:\n%s\nwant:\n%s", tc.width, screen, wantScreen)
			}

			// Verify continuation lines are indented by 6 spaces and words are never broken mid-word.
			for i, line := range term.visibleLines() {
				if strings.TrimSpace(line) == "" {
					continue
				}
				if i == 0 {
					if !strings.HasPrefix(line, "orca> ") {
						t.Fatalf("first line %q missing 'orca> ' prefix", line)
					}
				} else if !strings.HasPrefix(line, "      ") {
					t.Fatalf("continuation line %d %q missing 6-space indent", i, line)
				}
				if strings.HasSuffix(line, "actio") || strings.HasPrefix(strings.TrimSpace(line), "n.") {
					t.Fatalf("mid-word wrap detected on line %d: %q", i, line)
				}
			}
		})
	}
}

func TestWordWrapAt188ColsIndentsContinuationLines(t *testing.T) {
	t.Parallel()
	longPara := "**Summary:** " + strings.Repeat("checking whether that flag needs action before merge ", 5)
	lines := FormatReply(longPara, 188, true)
	if len(lines) < 2 {
		t.Fatalf("expected long paragraph to wrap at 188 cols, got %d lines", len(lines))
	}
	term := newFakeTerminal(188, 10)
	for _, l := range lines {
		term.write(l + "\r\n")
	}
	visible := term.nonEmptyLines()
	if len(visible) < 2 {
		t.Fatalf("expected >= 2 visible lines, got %v", visible)
	}
	if !strings.HasPrefix(visible[0], "orca> Summary: ") {
		t.Fatalf("line 0 = %q, want 'orca> Summary: ' prefix", visible[0])
	}
	for i, l := range visible[1:] {
		if !strings.HasPrefix(l, "      ") {
			t.Fatalf("188-col continuation line %d = %q, missing 6-space indent", i+1, l)
		}
		if runewidth.StringWidth(l) > 187 {
			t.Fatalf("188-col line %d width = %d, exceeds 187", i+1, runewidth.StringWidth(l))
		}
	}
}

func TestMarkdownRenderedOnTTYAndPlainWhenPiped(t *testing.T) {
	t.Parallel()
	md := strings.Join([]string{
		"### Run Summary",
		"**Goal:** verify whether that flag needs action before the release cut.",
		"",
		"- **Status:** lane `orca-ui` is running.",
		"- **Link:** [design doc](https://example.com/orca)",
		"1. First step",
		"2. Second step",
	}, "\n")

	// 1. TTY rendering includes ANSI bold, inline code, bullet •, numbered list, and underlined URL.
	ttyOut := strings.Join(FormatReply(md, 60, true), "\n")
	for _, want := range []string{
		"\x1b[1mRun Summary\x1b[22m",
		"\x1b[1mGoal:\x1b[22m",
		"• \x1b[1mStatus:\x1b[22m lane \x1b[36morca-ui\x1b[39m is running.",
		"• \x1b[1mLink:\x1b[22m design doc (\x1b[4mhttps://example.com/orca\x1b[24m)",
		"      1. First step",
		"      2. Second step",
	} {
		if !strings.Contains(ttyOut, want) {
			t.Fatalf("TTY markdown output missing %q:\n%q", want, ttyOut)
		}
	}
	for _, rawToken := range []string{"**Goal:**", "**Status:**", "`orca-ui`", "### "} {
		if strings.Contains(ttyOut, rawToken) {
			t.Fatalf("TTY markdown output still contains raw token %q:\n%q", rawToken, ttyOut)
		}
	}

	// 2. Piped run via chat.Run produces plain text without ANSI escapes or raw markdown syntax.
	m := New(t.Context(), "front-desk", nil, routeFunc(func(context.Context, string) (router.Decision, error) {
		return router.Decision{Handler: router.Concierge, Confidence: 1}, nil
	}))
	m.width = 60
	m.Dispatch = func(ctx context.Context, _ router.Decision, _ string, emit func(string)) error {
		EmitToolCall(ctx, "status", []byte(`{"run_id":"front-desk"}`), "front-desk")
		emit(md)
		return nil
	}
	var pipedOut strings.Builder
	if err := Run(t.Context(), strings.NewReader("status check\n"), &pipedOut, m); err != nil {
		t.Fatalf("Run piped: %v", err)
	}
	wantBytes, err := goldenFS.ReadFile("testdata/turn_piped.golden")
	if err != nil {
		t.Fatalf("read turn_piped.golden: %v", err)
	}
	if pipedOut.String() != string(wantBytes) {
		t.Fatalf("piped output mismatch:\ngot:\n%q\nwant:\n%q", pipedOut.String(), string(wantBytes))
	}
	if strings.Contains(pipedOut.String(), "\x1b[") {
		t.Fatalf("piped output contains ANSI escape sequence: %q", pipedOut.String())
	}
}

func TestTurnLayoutWithToolCallsAndBlankLineBetweenTurns(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	clk := clock.NewFakeClock(time.Date(2026, 11, 15, 5, 40, 0, 0, time.UTC))
	m := New(ctx, "front-desk", nil, routeFunc(func(context.Context, string) (router.Decision, error) {
		return router.Decision{Handler: router.Concierge, Confidence: 1}, nil
	}))
	m.Clock = clk
	m.width = 80
	m.SpinnerInterval = -1
	m.Dispatch = func(turnCtx context.Context, _ router.Decision, text string, emit func(string)) error {
		if text == "check status" {
			EmitToolCall(turnCtx, "status", []byte(`{"run_id":"front-desk"}`), "front-desk")
			EmitToolCall(turnCtx, "lanes", []byte(`{"run_id":"20261114-trial-m1"}`), "front-desk")
			emit("**Status:** `m2-default` is in review.\n- **Checks:** all green.")
			return nil
		}
		emit("Second turn reply.")
		return nil
	}

	tm := teatest.NewTestModel(t, m, teatest.WithInitialTermSize(80, 24))
	var captured strings.Builder
	waitCapture := func(substrs ...string) {
		t.Helper()
		waitScreenModel(t, tm, m, nil, &captured, 80, 24, substrs...)
	}
	waitCapture("orca chat — run front-desk")

	submit(tm, "check status")
	waitCapture("• Checks: all green.")
	submit(tm, "follow up")
	waitCapture("orca> Second turn reply.")
	finish(t, tm)

	rawBytes, err := io.ReadAll(tm.Output())
	if err != nil {
		t.Fatal(err)
	}
	captured.Write(rawBytes)
	raw := captured.String()

	// Verify styled owner line, colored/emoji routing line, dim tool-call lines, and rendered markdown reply.
	for _, want := range []string{
		"\x1b[1myou> check status\x1b[22m",
		"\x1b[36m  💬 → concierge: check status\x1b[39m",
		"\x1b[2m  → status front-desk\x1b[22m",
		"\x1b[2m  → lanes 20261114-trial-m1\x1b[22m",
		"orca> \x1b[1mStatus:\x1b[22m \x1b[36mm2-default\x1b[39m is in review.",
		"      • \x1b[1mChecks:\x1b[22m all green.",
		"\x1b[1myou> follow up\x1b[22m",
		"orca> Second turn reply.",
	} {
		if !strings.Contains(raw, want) {
			t.Fatalf("TTY output missing %q in:\n%q", want, raw)
		}
	}

	// Feed into 80x24 fake terminal and verify visual turn layout & blank line between turns.
	vt := newFakeTerminal(80, 24)
	vt.write(raw)
	vis := vt.visibleLines()
	var turn1ReplyIdx, turn2OwnerIdx int = -1, -1
	for i, line := range vis {
		if strings.Contains(line, "• Checks: all green.") {
			turn1ReplyIdx = i
		}
		if strings.Contains(line, "you> follow up") {
			turn2OwnerIdx = i
		}
	}
	if turn1ReplyIdx < 0 || turn2OwnerIdx < 0 {
		t.Fatalf("missing turn 1 reply or turn 2 owner line on visible screen:\n%s", strings.Join(vis, "\n"))
	}
	if turn2OwnerIdx != turn1ReplyIdx+2 || vis[turn1ReplyIdx+1] != "" {
		t.Fatalf("expected blank line between turn 1 reply (row %d) and turn 2 owner (row %d), screen:\n%s",
			turn1ReplyIdx, turn2OwnerIdx, strings.Join(vis, "\n"))
	}
}

func TestRestartPrintsRecentTurnsAndRestartLineOnVisibleScreen(t *testing.T) {
	t.Parallel()
	// 1. Verify FormatRestartStatus with no held runs vs held runs.
	noHeld := FormatRestartStatus("c00d67a", "9f8e7d6", nil)
	if len(noHeld) != 1 || noHeld[0] != "orca restarted (version c00d67a -> 9f8e7d6)" {
		t.Fatalf("FormatRestartStatus(no held) = %q, want single 'orca restarted (version c00d67a -> 9f8e7d6)'", noHeld)
	}
	withHeld := FormatRestartStatus("c00d67a", "9f8e7d6", []string{"reattached 20261114-trial-m1 (2 lanes, driving again, pid 1234)"})
	if len(withHeld) != 2 || withHeld[0] != "orca restarted (version c00d67a -> 9f8e7d6)" || !strings.Contains(withHeld[1], "reattached 20261114-trial-m1") {
		t.Fatalf("FormatRestartStatus(with held) = %q", withHeld)
	}

	// 2. Start a chat model after /restart with 5 prior turns and no held runs in an 80x24 fake terminal.
	transcript := []string{
		"you> turn 1 question",
		"orca> turn 1 answer",
		"you> turn 2 question",
		"orca> turn 2 answer",
		"you> turn 3 question",
		"orca> **Turn 3:** answer",
		"you> turn 4 question",
		"orca> **Turn 4:** answer",
		"you> turn 5 question",
		"orca> **Turn 5:** answer with `code`",
	}
	transcript = append(transcript, noHeld...)

	ctx := t.Context()
	clk := clock.NewFakeClock(time.Date(2026, 11, 15, 5, 42, 0, 0, time.UTC))
	st := store.NewMemoryStore(clk)
	defer st.Close()
	if err := st.Runs().Create(ctx, store.Run{ID: "front-desk"}); err != nil {
		t.Fatal(err)
	}
	b := NewStoreBackend(st, "front-desk")
	b.FrontDesk = "front-desk"

	m := New(ctx, "front-desk", nil, nil).WithTranscript(transcript)
	m.Clock = clk
	m.width = 80
	m.Backend = b

	tm := teatest.NewTestModel(t, m, teatest.WithInitialTermSize(80, 24))
	var captured strings.Builder
	var vt *fakeTerminal
	waitScreenModel(t, tm, m, &vt, &captured, 80, 24, "orca restarted (version c00d67a -> 9f8e7d6)", "front desk: no run yet")
	finish(t, tm)

	screen := strings.Join(vt.nonEmptyLines(), "\n")

	for _, want := range []string{
		"orca chat — run front-desk",
		"2 earlier turns; /log to see them",
		"you> turn 3 question",
		"orca> Turn 3: answer",
		"you> turn 4 question",
		"orca> Turn 4: answer",
		"you> turn 5 question",
		"orca> Turn 5: answer with code",
		"orca restarted (version c00d67a -> 9f8e7d6)",
		">",
		"front desk: no run yet",
	} {
		if !strings.Contains(screen, want) {
			t.Fatalf("visible screen after /restart missing %q:\n%s", want, screen)
		}
	}
	for _, unwanted := range []string{
		"turn 1 question",
		"turn 2 question",
		"reattached front-desk",
	} {
		if strings.Contains(screen, unwanted) {
			t.Fatalf("visible screen after /restart unexpectedly contains %q:\n%s", unwanted, screen)
		}
	}
}

func TestHeaderPrintedOnceAcrossTurnsAndStatusRedraws(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	clk := clock.NewFakeClock(time.Date(2026, 11, 15, 5, 44, 0, 0, time.UTC))
	st := store.NewMemoryStore(clk)
	defer st.Close()
	if err := st.Runs().Create(ctx, store.Run{ID: "front-desk", Objective: "test header once"}); err != nil {
		t.Fatal(err)
	}
	b := NewStoreBackend(st, "front-desk")
	b.FrontDesk = "front-desk"

	m := New(ctx, "front-desk", nil, routeFunc(func(context.Context, string) (router.Decision, error) {
		return router.Decision{Handler: router.Concierge, Confidence: 1}, nil
	}))
	m.Clock = clk
	m.PollInterval = 25 * time.Millisecond
	m.Backend = b
	m.Dispatch = func(_ context.Context, _ router.Decision, text string, emit func(string)) error {
		emit("reply to " + text)
		return nil
	}

	tm := teatest.NewTestModel(t, m, teatest.WithInitialTermSize(80, 24))
	var captured strings.Builder
	waitCapture := func(substrs ...string) {
		t.Helper()
		waitScreenModel(t, tm, m, nil, &captured, 80, 24, substrs...)
	}

	waitCapture("orca chat — run front-desk")
	submit(tm, "first turn")
	waitCapture("orca> reply to first turn")
	clk.Advance(25 * time.Millisecond)
	submit(tm, "second turn")
	waitCapture("orca> reply to second turn")
	finish(t, tm)
	rest, _ := io.ReadAll(tm.Output())
	captured.Write(rest)

	if count := strings.Count(captured.String(), "orca chat — run front-desk"); count != 1 {
		t.Fatalf("header 'orca chat — run front-desk' printed %d times, want 1:\n%q", count, captured.String())
	}
}

func TestFencedCodePreservesIndentationAndWrapsByDisplayWidth(t *testing.T) {
	t.Parallel()
	md := strings.Join([]string{
		"Snippet:",
		"```go",
		"func main() {",
		"    fmt.Println(\"a   b\")",
		"}",
		"```",
	}, "\n")

	ttyLines := FormatReply(md, 80, true)
	ttyOut := strings.Join(ttyLines, "\n")
	for _, want := range []string{
		"orca> Snippet:",
		"        \x1b[36mfunc main() {\x1b[39m",
		"        \x1b[36m    fmt.Println(\"a   b\")\x1b[39m",
		"        \x1b[36m}\x1b[39m",
	} {
		if !strings.Contains(ttyOut, want) {
			t.Fatalf("fenced code TTY output missing %q:\n%q", want, ttyOut)
		}
	}

	plainLines := FormatReply(md, 80, false)
	plainOut := strings.Join(plainLines, "\n")
	if !strings.Contains(plainOut, "        func main() {\n            fmt.Println(\"a   b\")\n        }") {
		t.Fatalf("fenced code plain output did not preserve 4-space indentation and internal spaces:\n%q", plainOut)
	}

	// Verify long code lines wrap by display width without stripping spaces.
	longFence := "```\n    01234567890123456789\n```"
	wrappedCode := FormatReply(longFence, 22, false)
	if len(wrappedCode) < 2 || wrappedCode[0] != "orca>       012345678" || !strings.HasPrefix(wrappedCode[1], "        90123456789") {
		t.Fatalf("long fenced code line wrap at width 22 = %q", wrappedCode)
	}
}

func TestOverlongTokenPreservedOnSingleLine(t *testing.T) {
	t.Parallel()
	longURL := "https://example.com/orca/very/long/path/that/exceeds/sixty/columns/without/any/whitespace"
	reply := "See " + longURL + " now."
	lines := FormatReply(reply, 60, false)
	want := []string{
		"orca> See",
		"      " + longURL,
		"      now.",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("FormatReply overlong token:\ngot:\n%s\nwant:\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
}

func TestControlCharactersStrippedFromUntrustedText(t *testing.T) {
	t.Parallel()
	malicious := "normal\x1b[2J\x1b]0;pwned\x07\rcarriage\bback\u009b31mred"

	reply := strings.Join(FormatReply(malicious, 80, false), "\n")
	if reply != "orca> normalcarriagebackred" {
		t.Fatalf("FormatReply did not strip control sequences: %q", reply)
	}
	user := strings.Join(FormatUserLine(malicious, 80, false), "\n")
	if user != "you> normalcarriagebackred" {
		t.Fatalf("FormatUserLine did not strip control sequences: %q", user)
	}
	dim := strings.Join(FormatDimLine("→ status "+malicious, 80, false), "\n")
	if dim != "→ status normalcarriagebackred" {
		t.Fatalf("FormatDimLine did not strip control sequences: %q", dim)
	}
	tool := FormatToolCall("status\x1b[2J", []byte(`{"run_id":"run-1\u001b]0;bad\u0007\r"}`), "front-desk")
	if tool != "→ status run-1" {
		t.Fatalf("FormatToolCall did not strip control sequences: %q", tool)
	}
}

func TestFormatToolCallTargetSelectionBranches(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		tool         string
		input        string
		defaultRunID string
		want         string
	}{
		{"trace-with-run", "log", `{"run_id":"run-1","trace":"tr-9"}`, "front-desk", "→ log run-1 tr-9"},
		{"trace-only", "log", `{"trace":"tr-9"}`, "", "→ log tr-9"},
		{"lane-id", "lane.show", `{"lane_id":"orca-ui"}`, "front-desk", "→ lane.show orca-ui"},
		{"ask-id", "directive.revoke", `{"id":"dir-42"}`, "front-desk", "→ directive.revoke dir-42"},
		{"goal-multiline", "run.start", `{"goal":"first line\nsecond line"}`, "front-desk", "→ run.start first line"},
		{"action-with-run", "run.control", `{"action":"pause"}`, "run-2", "→ run.control run-2 pause"},
		{"action-no-run", "run.control", `{"action":"pause"}`, "", "→ run.control pause"},
		{"kind", "directive.submit", `{"kind":"instruction"}`, "front-desk", "→ directive.submit instruction"},
		{"summary", "memory.submit", `{"summary":"keep footer pinned"}`, "front-desk", "→ memory.submit keep footer pinned"},
		{"default-run", "status", `{}`, "front-desk", "→ status front-desk"},
		{"no-target", "swarms", `{}`, "", "→ swarms"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := FormatToolCall(tc.tool, []byte(tc.input), tc.defaultRunID); got != tc.want {
				t.Fatalf("FormatToolCall(%q, %s, %q) = %q, want %q", tc.tool, tc.input, tc.defaultRunID, got, tc.want)
			}
		})
	}
}

func TestFormatReplyNarrowAndWideCJKPreservesCharacters(t *testing.T) {
	t.Parallel()
	cjk := "**状態:** テスト実行中 確認完了"
	narrow := FormatReply(cjk, 8, false)
	if len(narrow) < 2 || !strings.HasPrefix(narrow[0], "orca> ") {
		t.Fatalf("narrow CJK FormatReply = %q", narrow)
	}
	var stripped []string
	for i, line := range narrow {
		if i == 0 {
			line = strings.TrimPrefix(line, "orca> ")
		} else {
			line = strings.TrimPrefix(line, "      ")
		}
		stripped = append(stripped, line)
	}
	if got, want := strings.Join(stripped, " "), "状態: テスト実行中 確認完了"; got != want {
		t.Fatalf("narrow CJK characters lost:\ngot:  %q\nwant: %q", got, want)
	}
}

func TestFormatTranscriptSummaryAndTurns(t *testing.T) {
	t.Parallel()
	lines := []string{
		"4 earlier turns; /log to see them",
		"you> check lane",
		"→ concierge: check lane",
		"→ lane.show orca-ui",
		"orca> **Lane:** `orca-ui` is green.",
		"orca restarted (version a -> b)",
	}
	got := strings.Join(FormatTranscript(lines, 3, 60, false), "\n")
	for _, want := range []string{
		"4 earlier turns; /log to see them",
		"you> check lane",
		"→ concierge: check lane",
		"→ lane.show orca-ui",
		"orca> Lane: orca-ui is green.",
		"orca restarted (version a -> b)",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("FormatTranscript plain output missing %q:\n%s", want, got)
		}
	}
}

func TestStreamPreviewMultiLineAndChunkedAppends(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, want string
		chunks     []string
	}{
		{"single-line-chunks", "orca> first second", []string{"first ", "second"}},
		{"multi-line-shows-last", "orca> line two", []string{"line one\n", "line two"}},
		{"trailing-newline", "orca> line two", []string{"line one\nline two\n"}},
		{"internal-blank-line-whitespace-tail", "orca> line three", []string{"line one\n   \nline three"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := New(t.Context(), "front-desk", nil, nil)
			m.streaming = true
			ch := make(chan streamMsg, 1)
			for _, chunk := range tc.chunks {
				next, _ := m.Update(streamMsg{text: chunk, ch: ch})
				m = next.(Model)
			}
			if got := m.streamPreview(); got != tc.want {
				t.Fatalf("streamPreview() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestInnerLoopSpinnerUnderSlowTurnReplacedByReplyGolden(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	clk := clock.NewFakeClock(time.Date(2026, 11, 15, 5, 45, 0, 0, time.UTC))

	step1Gate := make(chan struct{})
	step2Gate := make(chan struct{})
	replyGate := make(chan struct{})

	m := New(ctx, "front-desk", nil, routeFunc(func(context.Context, string) (router.Decision, error) {
		return router.Decision{Handler: router.Concierge, Confidence: 1}, nil
	}))
	m.Clock = clk
	m.Now = clk.Now
	m.width = 80
	m.SpinnerInterval = time.Second
	m.Dispatch = func(turnCtx context.Context, _ router.Decision, _ string, emit func(string)) error {
		<-step1Gate
		EmitToolCall(turnCtx, "status", []byte(`{"run_id":"20261114-trial-m1"}`), "front-desk")
		<-step2Gate
		EmitToolCall(turnCtx, "read.file", []byte(`{"path":"OBJECTIVE.md"}`), "front-desk")
		<-replyGate
		emit("All 8 checks passed on PR #5899.")
		return nil
	}

	tm := teatest.NewTestModel(t, m, teatest.WithInitialTermSize(80, 24))
	vt := newFakeTerminal(80, 24)
	var rawAll strings.Builder

	waitScreen := func(substrs ...string) {
		t.Helper()
		waitScreenModel(t, tm, m, &vt, &rawAll, 80, 24, substrs...)
	}

	waitScreen("orca chat — run front-desk")

	var frames []string
	recordFrame := func(label string) {
		frames = append(frames, "=== "+label+" ===\n"+strings.Join(vt.nonEmptyLines(), "\n"))
	}

	submit(tm, "check 20261114-trial-m1")
	waitScreen("💬 → concierge: check 20261114-trial-m1", "⠋ 💬 concierge · thinking (0s)")
	recordFrame("frame 0: thinking")

	close(step1Gate)
	waitScreen("→ status 20261114-trial-m1")
	clk.Advance(time.Second)
	waitScreen("⠙ 💬 concierge · → status 20261114-trial-m1 (1s)")
	recordFrame("frame 1: tool status")

	close(step2Gate)
	waitScreen("→ read.file OBJECTIVE.md")
	clk.Advance(time.Second)
	waitScreen("⠹ 💬 concierge · reading OBJECTIVE.md (2s)")
	recordFrame("frame 2: reading OBJECTIVE.md")

	close(replyGate)
	waitScreen("orca> All 8 checks passed on PR #5899.")
	recordFrame("frame 3: reply replaces spinner")

	finish(t, tm)

	gotGolden := strings.Join(frames, "\n") + "\n"
	wantBytes, err := goldenFS.ReadFile("testdata/spinner_turn.golden")
	if err != nil {
		t.Fatalf("read spinner_turn.golden: %v", err)
	}
	if gotGolden != string(wantBytes) {
		t.Fatalf("spinner golden frames mismatch (raw=%q):\ngot:\n%s\nwant:\n%s", rawAll.String(), gotGolden, string(wantBytes))
	}
	if len(vt.scrollback) != 0 {
		t.Fatalf("spinner redraws leaked into scrollback: %q", vt.scrollback)
	}
}

func TestRoutedHandlersDistinctEmojiAndColor(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		handler   string
		wantEmoji string
		wantSGR   string
	}{
		{"concierge", "💬", "\x1b[36m"},
		{"dispatcher", "⚡", "\x1b[33m"},
		{"planner", "🧭", "\x1b[35m"},
	} {
		t.Run(tc.handler, func(t *testing.T) {
			t.Parallel()
			routeLines := FormatRouteLine(tc.handler, "act on run", false, 80, true)
			if len(routeLines) != 1 {
				t.Fatalf("FormatRouteLine(%q) = %v, want 1 line", tc.handler, routeLines)
			}
			if !strings.Contains(routeLines[0], tc.wantEmoji) || !strings.Contains(routeLines[0], tc.wantSGR) {
				t.Fatalf("FormatRouteLine(%q) = %q, want emoji %q and SGR %q", tc.handler, routeLines[0], tc.wantEmoji, tc.wantSGR)
			}
			spinLine := FormatSpinnerLine(0, tc.handler, "thinking", 3*time.Second, 80, true)
			if !strings.Contains(spinLine, tc.wantEmoji) || !strings.Contains(spinLine, "3s") {
				t.Fatalf("FormatSpinnerLine(%q) = %q, want emoji %q and elapsed 3s", tc.handler, spinLine, tc.wantEmoji)
			}
		})
	}
}

func TestFooterColoredSegmentsOpenPRFailingCheckAndCostGuard(t *testing.T) {
	t.Parallel()
	status := strings.Join([]string{
		"adopted: 20261114-trial-m1 | m5-finish review r1 PR #5899 (open, checks: 7 passed, 1 failing, 1 pending) 10m, m2-clean clean r1 PR #5900 (merged) 4m, m3-int integrate r2 PR #5901 (closed) 2m, m4-swe swe r1 1m",
		"lanes: 9 (done:7, failed:1, running:1; review:1) | open questions: 2 | cost today: $4.50",
	}, "\n")

	lines := FormatStatusFooter(status, FooterOptions{
		SpinnerFrame:     1,
		BackgroundActive: true,
		BackgroundRole:   "dispatcher",
		CostState:        ClassifyCostGuard(4.50, 5.00), // $4.50/hr is 90% of $5.00/hr -> near limit (amber/yellow)
	}, 240, true)
	if len(lines) != 2 {
		t.Fatalf("FormatStatusFooter lines = %d, want 2", len(lines))
	}
	joined := strings.Join(lines, "\n")

	for _, want := range []string{
		"\x1b[1;33m⠙ ⚡ dispatcher\x1b[22;39m",                                  // background activity indicator
		"\x1b[1;36m🐋 adopted: 20261114-trial-m1\x1b[22;39m",                    // held run name colored/bold
		"\x1b[33m🔍 \x1b[39m\x1b[1mm5-finish\x1b[22m\x1b[33m review r1\x1b[39m", // review lane with 🔍
		"\x1b[1m PR #5899\x1b[22m",                                             // PR number bold
		"\x1b[32mopen\x1b[39m",                                                 // open PR green
		"\x1b[32m7 passed\x1b[39m",                                             // passed checks green
		"\x1b[1;31m1 failing\x1b[22;39m",                                       // failing check bold red
		"\x1b[33m1 pending\x1b[39m",                                            // pending check yellow
		"\x1b[2m) 10m, \x1b[22m",                                               // elapsed in phase dimmed
		"\x1b[34m🧹 \x1b[39m\x1b[1mm2-clean\x1b[22m\x1b[34m clean r1\x1b[39m",   // clean lane with 🧹
		"\x1b[1;35mmerged\x1b[22;39m",                                          // merged PR bold magenta
		"\x1b[35m🚀 \x1b[39m\x1b[1mm3-int\x1b[22m\x1b[35m integrate r2\x1b[39m", // integrate lane with 🚀
		"\x1b[31mclosed\x1b[39m",                                               // closed PR red
		"\x1b[36m🔨 \x1b[39m\x1b[1mm4-swe\x1b[22m\x1b[36m swe r1\x1b[39m",       // swe lane with 🔨
		"\x1b[1;33m❓ open questions: 2\x1b[22;39m",                             // open questions > 0 highlighted
		"\x1b[1;33m🔶 cost today: $4.50\x1b[22;39m",                             // cost near limit amber
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("colored footer missing %q in:\n%q", want, joined)
		}
	}

	// Normal cost renders green; over-limit cost renders bold red with 🛑; open questions: 0 renders dimmed.
	normalLines := FormatStatusFooter(
		"adopted: 20261114-trial-m1\nlanes: 1 (running:1; swe:1) | open questions: 0 | cost today: $1.20",
		FooterOptions{CostState: CostStateNormal},
		120,
		true,
	)
	if normalJoined := strings.Join(normalLines, "\n"); !strings.Contains(normalJoined, "\x1b[32mcost today: $1.20\x1b[39m") {
		t.Fatalf("normal cost not colored green:\n%q", normalJoined)
	}

	overLines := FormatStatusFooter(
		"adopted: 20261114-trial-m1\nlanes: 1 (running:1; swe:1) | open questions: 0 | cost today: $6.10",
		FooterOptions{CostState: ClassifyCostGuard(6.10, 5.00)},
		120,
		true,
	)
	overJoined := strings.Join(overLines, "\n")
	if !strings.Contains(overJoined, "\x1b[1;31m🛑 cost today: $6.10\x1b[22;39m") {
		t.Fatalf("over-limit cost not colored red:\n%q", overJoined)
	}
	if !strings.Contains(overJoined, "open questions: 0") || strings.Contains(overJoined, "❓") {
		t.Fatalf("zero open questions should be dimmed without ❓:\n%q", overJoined)
	}
}

func TestNoColorAndPipedOutputProducePlainText(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	status := "adopted: 20261114-trial-m1 | m5-finish review r1 PR #5899 (open, checks: 7 passed, 1 failing) 10m\nlanes: 9 (done:7, failed:1, running:1; review:1) | open questions: 1 | cost today: $7.49"

	m := New(t.Context(), "front-desk", nil, routeFunc(func(context.Context, string) (router.Decision, error) {
		return router.Decision{Handler: router.Concierge, Confidence: 1}, nil
	}))
	m.width = 120
	m.status = status
	m.busy = true
	m.turnHandler = "concierge"
	m.turnStep = "thinking"
	m.bgActive = true
	m.bgRole = "dispatcher"
	m.costState = CostStateOverLimit

	if m.needsSpinner() {
		t.Fatal("needsSpinner() = true under NO_COLOR, want false")
	}
	if cmd := m.ensureSpinner(); cmd != nil {
		t.Fatal("ensureSpinner() returned non-nil cmd under NO_COLOR")
	}
	viewContent := m.View().Content
	if strings.Contains(viewContent, "\x1b[") {
		t.Fatalf("NO_COLOR View() contains ANSI escapes: %q", viewContent)
	}
	for _, emoji := range []string{"🐋", "🔨", "🧹", "🔍", "🚀", "❓", "🔶", "🛑", "⠋", "⠙", "⚡", "💬", "🧭"} {
		if strings.Contains(viewContent, emoji) {
			t.Fatalf("NO_COLOR View() contains emoji/spinner glyph %q: %q", emoji, viewContent)
		}
	}
	m.busy = false

	// Verify piped Run under NO_COLOR emits no spinner ticks, no ANSI, and no emoji.
	m.Dispatch = func(turnCtx context.Context, _ router.Decision, _ string, emit func(string)) error {
		EmitToolCall(turnCtx, "status", []byte(`{"run_id":"20261114-trial-m1"}`), "front-desk")
		emit("**Status:** `m5-finish` is green.")
		return nil
	}
	var pipedOut strings.Builder
	if err := Run(t.Context(), strings.NewReader("status\n"), &pipedOut, m); err != nil {
		t.Fatalf("Run piped under NO_COLOR: %v", err)
	}
	if strings.Contains(pipedOut.String(), "\x1b[") {
		t.Fatalf("piped output contains ANSI escapes: %q", pipedOut.String())
	}
	for _, emoji := range []string{"💬", "⚡", "🧭", "🐋", "🔍", "⠋"} {
		if strings.Contains(pipedOut.String(), emoji) {
			t.Fatalf("piped output contains emoji/spinner %q: %q", emoji, pipedOut.String())
		}
	}
}

func TestHalfTypedInputSurvives50SpinnerFrames(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	clk := clock.NewFakeClock(time.Date(2026, 11, 15, 5, 50, 0, 0, time.UTC))

	turnStarted := make(chan struct{})
	releaseTurn := make(chan struct{})
	dispatched := make(chan string, 4)

	m := New(ctx, "front-desk", nil, routeFunc(func(context.Context, string) (router.Decision, error) {
		return router.Decision{Handler: router.Dispatcher, Confidence: 1}, nil
	}))
	m.Clock = clk
	m.Now = clk.Now
	m.width = 80
	m.SpinnerInterval = time.Second
	m.status = "adopted: 20261114-trial-m1 | m5-finish review r1 PR #5899 (open, checks: 8 passed) 10m\nlanes: 5 (done:4, running:1; review:1) | open questions: 0 | cost today: $2.10"
	m.Dispatch = func(_ context.Context, _ router.Decision, text string, emit func(string)) error {
		if text == "first command" {
			close(turnStarted)
			<-releaseTurn
		}
		emit("completed: " + text)
		dispatched <- text
		return nil
	}

	tm := teatest.NewTestModel(t, m, teatest.WithInitialTermSize(80, 24))
	vt := newFakeTerminal(80, 24)
	var rawAll strings.Builder

	waitScreen := func(substr string) {
		t.Helper()
		waitScreenModel(t, tm, m, &vt, &rawAll, 80, 24, substr)
	}

	waitScreen("orca chat — run front-desk")
	submit(tm, "first command")
	select {
	case <-turnStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("first turn did not start")
	}
	waitScreen("⠋ ⚡ dispatcher · thinking (0s)")

	// Type half an input line while the spinner is active.
	tm.Type("hold m5-finish ")
	waitScreen("> hold m5-finish")

	// Advance 50 spinner frames across 50 seconds via the injected clock and verify
	// the half-typed input stays intact and zero lines leak into scrollback.
	for i := 1; i <= 50; i++ {
		clk.Advance(time.Second)
		if !m.sync.wait(ctx, func(r, _ frameState) bool { return r.spinnerFrame >= i }) {
			t.Fatalf("spinner did not advance to frame %d", i)
		}
	}
	wantGlyph := SpinnerFrames[50%len(SpinnerFrames)]
	waitScreen(wantGlyph + " ⚡ dispatcher · thinking (50s)")

	screenDuring50th := strings.Join(vt.nonEmptyLines(), "\n")
	if !strings.Contains(screenDuring50th, "> hold m5-finish") {
		t.Fatalf("half-typed input disturbed after 50 spinner frames:\n%s", screenDuring50th)
	}
	if len(vt.scrollback) != 0 {
		t.Fatalf("50 spinner frames leaked %d lines into scrollback: %q (raw=%q)", len(vt.scrollback), vt.scrollback, rawAll.String())
	}

	// Finish the first turn, complete the half-typed input, and submit.
	close(releaseTurn)
	waitScreen("orca> completed: first command")
	if got := <-dispatched; got != "first command" {
		t.Fatalf("first dispatched = %q, want first command", got)
	}

	submit(tm, "until CI passes")
	select {
	case got := <-dispatched:
		if got != "hold m5-finish until CI passes" {
			t.Fatalf("second dispatched after 50 spinner frames = %q, want %q", got, "hold m5-finish until CI passes")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("follow-up input after 50 spinner frames was not dispatched")
	}
	waitScreen("orca> completed: hold m5-finish until CI passes")
	finish(t, tm)
}

func TestFooter60ColAnd188ColTruncation(t *testing.T) {
	t.Parallel()
	status := strings.Join([]string{
		"adopted: 20261114-trial-m1 | m5-finish review r1 PR #5899 (open, checks: 8 passed) 10m",
		"lanes: 9 (done:7, failed:1, running:1; review:1) | open questions: 1 | cost today: $7.49",
	}, "\n")

	for _, width := range []int{60, 188} {
		t.Run(strconv.Itoa(width)+"-cols", func(t *testing.T) {
			t.Parallel()
			lines := FormatStatusFooter(status, FooterOptions{
				SpinnerFrame:     0,
				BackgroundActive: true,
				BackgroundRole:   "dispatcher",
				CostState:        CostStateNearLimit,
			}, width, true)
			if len(lines) != 2 {
				t.Fatalf("FormatStatusFooter(%d) returned %d lines, want 2", width, len(lines))
			}
			vt := newFakeTerminal(width, 6)
			for i, l := range lines {
				plain := SanitizeText(l)
				w := runewidth.StringWidth(plain)
				if w > effectiveWidth(width) {
					t.Fatalf("width %d line %d plain=%q display width=%d exceeds effectiveWidth=%d", width, i, plain, w, effectiveWidth(width))
				}
				if width == 60 && !strings.HasSuffix(plain, "…") {
					t.Fatalf("width 60 line %d plain=%q expected ellipsis truncation", i, plain)
				}
				if width == 188 && strings.HasSuffix(plain, "…") {
					t.Fatalf("width 188 line %d plain=%q unexpectedly truncated", i, plain)
				}
				vt.write(l + "\r\n")
			}
			if gotRows := len(vt.nonEmptyLines()); gotRows != 2 {
				t.Fatalf("width %d footer wrapped onto %d rows on fakeTerminal:\n%s", width, gotRows, strings.Join(vt.nonEmptyLines(), "\n"))
			}
		})
	}
}

func TestBackgroundEventsPrintedAboveInputColored(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	clk := clock.NewFakeClock(time.Date(2026, 11, 15, 5, 55, 0, 0, time.UTC))
	st := store.NewMemoryStore(clk)
	defer st.Close()

	if err := st.Runs().Create(ctx, store.Run{ID: "run-1", Objective: "live ui"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Lanes().Upsert(ctx, store.Lane{
		RunID: "run-1", ID: "m5-finish", Phase: "review", Status: "running", Round: 1, PR: 5899,
	}); err != nil {
		t.Fatal(err)
	}
	b := NewStoreBackend(st, "run-1")
	b.Now = clk.Now

	m := New(ctx, "run-1", nil, nil)
	m.Clock = clk
	m.width = 100
	m.PollInterval = 30 * time.Millisecond
	m.SpinnerInterval = -1
	m.Backend = b

	tm := teatest.NewTestModel(t, m, teatest.WithInitialTermSize(100, 24))
	vt := newFakeTerminal(100, 24)
	var rawAll strings.Builder

	waitScreen := func(substr string) {
		t.Helper()
		waitScreenModel(t, tm, m, &vt, &rawAll, 100, 24, substr)
	}

	waitScreen("orca chat — run run-1")
	tm.Type("drafting reply")
	waitScreen("> drafting reply")

	// Emit the 4 notable background events: lane advanced (mentioning integrate),
	// PR merged (mentioning failed check retry), lane question (mentioning review),
	// and an end-to-end EnforceCostGuard pause (writing both decision and owner_ask directive).
	if _, err := st.Decisions().Append(ctx, store.Decision{
		RunID: "run-1", LaneID: "m5-finish", Actor: "controller", Category: store.DecisionAuto,
		Tool: controller.ToolPhaseAdvance, Result: "advanced m5-finish from review to integrate",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Decisions().Append(ctx, store.Decision{
		RunID: "run-1", LaneID: "m5-finish", Actor: "controller", Category: store.DecisionAuto,
		Tool: controller.ToolLaneComplete, Reason: "PR #5899 merged after failed check retry", Result: "lane done (pr merged)",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Directives().Add(ctx, store.Directive{
		ID: "ask-bg-1", RunID: "run-1", Ref: "owner_ask:q-bg-1", Scope: "m5-finish",
		Text: `{"id":"q-bg-1","LaneID":"m5-finish","question":"Cut release tag after review?"}`,
		Kind: store.DirectiveInstruction, Source: store.DirectiveSourceDispatcher, Status: store.DirectiveActive,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Decisions().Append(ctx, store.Decision{
		RunID: "run-1", Actor: "dispatcher", Category: store.DecisionJudgment,
		Tool: "lane.spawn", CostUSD: 6.20, CreatedAt: clk.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if paused, _, _, _, err := store.EnforceCostGuard(ctx, st, "run-1", clk.Now(), 5.00); err != nil || !paused {
		t.Fatalf("EnforceCostGuard = (%v, %v), want (true, nil)", paused, err)
	}

	clk.Advance(30 * time.Millisecond)
	waitScreen("Cost guard paused turns for run run-1: spent $6.20 in the last hour (limit $5.00/hr).")
	finish(t, tm)

	screen := strings.Join(vt.nonEmptyLines(), "\n")
	promptIdx := strings.Index(screen, "> drafting reply")
	if promptIdx < 0 {
		t.Fatalf("half-typed input lost after background events:\n%s", screen)
	}
	for _, wantAbove := range []string{
		"🔄 lane advanced: advanced m5-finish from review to integrate",
		"🎉 PR merged (m5-finish): PR #5899 merged after failed check retry",
		"❓ lane question (m5-finish): Cut release tag after review?",
		"🛑 Cost guard paused turns for run run-1: spent $6.20 in the last hour (limit $5.00/hr).",
	} {
		idx := strings.Index(screen, wantAbove)
		if idx < 0 || idx > promptIdx {
			t.Fatalf("background event %q missing or not printed above input prompt:\n%s", wantAbove, screen)
		}
	}
	if strings.Contains(screen, "lane question (run-1): Cost guard paused") {
		t.Fatalf("cost-guard owner_ask directive was reported twice as a lane question:\n%s", screen)
	}

	// Verify long background outcomes wrap across physical lines at 60 cols without ellipsis truncation.
	longOutcome := "dispatcher failed: failed to launch lane m5-finish in worktree because git rebase reported conflicting changes on render.go"
	wrappedRows := FormatBackgroundEvent(longOutcome, 60, true)
	if len(wrappedRows) < 2 {
		t.Fatalf("expected long background event to wrap at 60 cols, got %d lines: %v", len(wrappedRows), wrappedRows)
	}
	joinedPlain := strings.Join(strings.Fields(SanitizeText(strings.Join(wrappedRows, " "))), " ")
	if strings.Contains(joinedPlain, "…") || !strings.Contains(joinedPlain, "conflicting changes on render.go") {
		t.Fatalf("60-col wrapped background event truncated text: %q", joinedPlain)
	}
}

func TestStoreBackendSnapshotBackgroundActivityAndCostGuardState(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	clk := clock.NewFakeClock(time.Date(2026, 11, 15, 6, 0, 0, 0, time.UTC))
	st := store.NewMemoryStore(clk)
	defer st.Close()

	if err := st.Runs().Create(ctx, store.Run{ID: "run-cg", Objective: "verify cost guard and bg"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Runs().UpdateSettings(ctx, "run-cg", store.RunSettings{MaxCostPerHourUSD: 5.00}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Runs().AcquireLease(ctx, "run-cg", "controller", 24*time.Hour); err != nil {
		t.Fatal(err)
	}
	b := NewStoreBackend(st, "run-cg")
	b.Now = clk.Now

	// Spend $6.00 two hours ago (still same UTC day, so daily v.cost = $6.00 > $5.00/hr limit,
	// while trailing-hour spend is $0.00 -> CostState must stay CostStateNormal).
	if _, err := st.Decisions().Append(ctx, store.Decision{
		RunID: "run-cg", Actor: "dispatcher", Category: store.DecisionJudgment,
		Tool: "lane.spawn", CostUSD: 6.00, CreatedAt: clk.Now().Add(-2 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	snapDailyOnly, err := b.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snapDailyOnly.CostState != CostStateNormal {
		t.Fatalf("snapDailyOnly.CostState = %v, want CostStateNormal when daily spend ($6.00) exceeds hourly limit ($5.00/hr) but trailing-hour spend is $0", snapDailyOnly.CostState)
	}

	// Queue a dispatcher instruction that yields no immediate outcome line while no thread is running:
	// BackgroundActive must remain false (not stuck spinning on pendingOutcomeRuns).
	if err := b.Dispatch(ctx, router.Decision{Handler: router.Dispatcher}, "check lanes", func(string) {}); err != nil {
		t.Fatal(err)
	}
	snapQueuedIdle, err := b.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snapQueuedIdle.BackgroundActive {
		t.Fatalf("snapQueuedIdle.BackgroundActive = true when no dispatcher thread/turn is running")
	}

	// Start a running dispatcher turn ($0 in the trailing hour so far).
	th, err := st.Threads().StartThread(ctx, store.Thread{
		ID: "dispatcher-run-cg", RunID: "run-cg", Role: store.ThreadRoleDispatcher, Status: store.ThreadStatusRunning,
	})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := st.Threads().StartTurn(ctx, store.Turn{
		ID: "turn-bg-1", ThreadID: th.ID, RunID: "run-cg", Input: "batch", StartedAt: clk.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}

	snap1, err := b.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !snap1.BackgroundActive || snap1.BackgroundRole != "dispatcher" || snap1.CostState != CostStateNormal {
		t.Fatalf("snap1 = %+v, want BackgroundActive=true role=dispatcher CostState=Normal", snap1)
	}

	// Complete the turn with $4.25 (85% of $5.00/hr limit -> CostStateNearLimit)
	// and verify the queued dispatcher outcome is still delivered even though snapQueuedIdle
	// polled before the dispatcher thread existed.
	if _, err := st.Threads().AppendItem(ctx, store.Item{
		ThreadID: th.ID, TurnID: turn.ID, Kind: store.ItemKindAssistantMessage,
		Content: "Checked all lanes; m5-finish is healthy.",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Threads().CompleteTurn(ctx, th.ID, turn.ID, store.TurnStatusCompleted, model.Usage{CostUSD: 4.25}, ""); err != nil {
		t.Fatal(err)
	}
	if err := st.Threads().UpdateThreadStatus(ctx, th.ID, store.ThreadStatusIdle); err != nil {
		t.Fatal(err)
	}

	snap2, err := b.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snap2.BackgroundActive || snap2.CostState != CostStateNearLimit {
		t.Fatalf("snap2 = %+v, want BackgroundActive=false CostState=CostStateNearLimit", snap2)
	}
	if !slices.Contains(snap2.Outcomes, "dispatcher: Checked all lanes; m5-finish is healthy.") {
		t.Fatalf("snap2.Outcomes = %v, want queued dispatcher outcome delivered after idle poll", snap2.Outcomes)
	}

	// Add $1.20 decision cost in the current hour ($5.45/hr -> CostStateOverLimit).
	if _, err := st.Decisions().Append(ctx, store.Decision{
		RunID: "run-cg", TurnID: "turn-bg-2", Actor: "dispatcher", Category: store.DecisionJudgment,
		Tool: "lane.spawn", CostUSD: 1.20, CreatedAt: clk.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	snap3, err := b.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snap3.CostState != CostStateOverLimit {
		t.Fatalf("snap3 = %+v, want CostState=CostStateOverLimit", snap3)
	}
}

func TestAutoSpinnerTicksAdvanceFramesDuringSlowTurn(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	clk := clock.NewFakeClock(time.Date(2026, 11, 15, 6, 5, 0, 0, time.UTC))
	release := make(chan struct{})
	m := New(ctx, "front-desk", nil, routeFunc(func(context.Context, string) (router.Decision, error) {
		return router.Decision{Handler: router.Planner, Confidence: 1}, nil
	}))
	m.Clock = clk
	m.width = 80
	m.SpinnerInterval = 20 * time.Millisecond
	m.Dispatch = func(_ context.Context, _ router.Decision, _ string, emit func(string)) error {
		<-release
		emit("Plan updated.")
		return nil
	}

	tm := teatest.NewTestModel(t, m, teatest.WithInitialTermSize(80, 24))
	vt := newFakeTerminal(80, 24)
	var rawAll strings.Builder
	waitScreen := func(substr string) {
		t.Helper()
		waitScreenModel(t, tm, m, &vt, &rawAll, 80, 24, substr)
	}

	waitScreen("orca chat — run front-desk")
	submit(tm, "replan m5")
	waitScreen("⠋ 🧭 planner")
	clk.Advance(20 * time.Millisecond)
	waitScreen("⠙ 🧭 planner")
	clk.Advance(20 * time.Millisecond)
	waitScreen("⠹ 🧭 planner")
	close(release)
	waitScreen("orca> Plan updated.")
	finish(t, tm)
}

func TestFormatBackgroundEventClassification(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		line     string
		wantIcon string
		wantSGR  string
	}{
		{
			name:     "cost guard pause",
			line:     "cost guard: paused turns for run-1 after $6.20/hr",
			wantIcon: "🛑",
			wantSGR:  "\x1b[1;31m",
		},
		{
			name:     "PR merged",
			line:     "PR merged (m5-finish): PR #5899 merged after failed check retry",
			wantIcon: "🎉",
			wantSGR:  "\x1b[1;35m",
		},
		{
			name:     "lane question",
			line:     "lane question (m5-finish): Cut release tag after review?",
			wantIcon: "❓",
			wantSGR:  "\x1b[1;33m",
		},
		{
			name:     "lane advanced with lane ID",
			line:     "lane m5-finish advanced: swe -> review",
			wantIcon: "🔄",
			wantSGR:  "\x1b[36m",
		},
		{
			name:     "lane completed without lane ID",
			line:     "lane completed: m5-finish done",
			wantIcon: "🔄",
			wantSGR:  "\x1b[36m",
		},
		{
			name:     "dispatcher failed",
			line:     "dispatcher failed: turn timed out after 30s",
			wantIcon: "✗",
			wantSGR:  "\x1b[31m",
		},
		{
			name:     "dispatcher outcome",
			line:     "dispatcher: lane.spawn (spawned m5-finish)",
			wantIcon: "⚡",
			wantSGR:  "\x1b[33m",
		},
		{
			name:     "mail summary mentioning merged question advanced failed uses neutral bullet",
			line:     "adopted mail summary: PR was merged after question about failed advanced check",
			wantIcon: "•",
			wantSGR:  "\x1b[36m",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rows := FormatBackgroundEvent(tc.line, 120, true)
			if len(rows) != 1 {
				t.Fatalf("FormatBackgroundEvent(%q) rows = %d, want 1", tc.line, len(rows))
			}
			plain := SanitizeText(rows[0])
			if !strings.HasPrefix(plain, "  "+tc.wantIcon+" ") {
				t.Fatalf("FormatBackgroundEvent(%q) plain = %q, want prefix %q", tc.line, plain, "  "+tc.wantIcon+" ")
			}
			if !strings.Contains(rows[0], tc.wantSGR) {
				t.Fatalf("FormatBackgroundEvent(%q) = %q, missing SGR %q", tc.line, rows[0], tc.wantSGR)
			}
		})
	}
}
