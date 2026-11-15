package chat

import (
	"context"
	"embed"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/exp/teatest/v2"
	"github.com/mattn/go-runewidth"

	"github.com/bazelment/yoloswe/orca/router"
	"github.com/bazelment/yoloswe/orca/store"
)

//go:embed testdata/*.golden
var goldenFS embed.FS

// fakeTerminal emulates a fixed-size VT100/ANSI terminal screen so tests can
// verify what remains visible on screen (and in scrollback) after Bubble Tea
// cursor movements, line insertions, and erases.
type fakeTerminal struct {
	cells      [][]rune
	scrollback []string
	w, h       int
	row, col   int
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
	runes := []rune(s)
	for i := 0; i < len(runes); {
		r := runes[i]
		if r == '\x1b' {
			if i+1 >= len(runes) {
				i++
				continue
			}
			next := runes[i+1]
			if next == 'M' {
				if t.row > 0 {
					t.row--
				}
				i += 2
				continue
			}
			if next == '[' {
				j := i + 2
				for j < len(runes) && (runes[j] < 0x40 || runes[j] > 0x7e) {
					j++
				}
				if j < len(runes) {
					params := string(runes[i+2 : j])
					cmd := runes[j]
					t.handleCSI(params, cmd)
					i = j + 1
					continue
				}
			}
			i += 2
			continue
		}
		switch r {
		case '\r':
			t.col = 0
		case '\n':
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
	m := New(ctx, "front-desk", nil, routeFunc(func(context.Context, string) (router.Decision, error) {
		return router.Decision{Handler: router.Concierge, Confidence: 1}, nil
	}))
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
	waitCapture := func(substr string) {
		t.Helper()
		teatest.WaitFor(t, tm.Output(), func(p []byte) bool {
			captured.Write(p)
			return strings.Contains(captured.String(), substr)
		}, teatest.WithDuration(5*time.Second))
	}
	waitCapture("orca chat — run front-desk")

	submit(tm, "check status")
	waitCapture("Checks:")
	submit(tm, "follow up")
	waitCapture("Second turn reply.")
	finish(t, tm)

	rawBytes, err := io.ReadAll(tm.Output())
	if err != nil {
		t.Fatal(err)
	}
	captured.Write(rawBytes)
	raw := captured.String()

	// Verify styled owner line, dim routing line, dim tool-call lines, and rendered markdown reply.
	for _, want := range []string{
		"\x1b[1myou> check status\x1b[22m",
		"\x1b[2m  → concierge: check status\x1b[22m",
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
	st := store.NewMemoryStore(nil)
	defer st.Close()
	if err := st.Runs().Create(ctx, store.Run{ID: "front-desk"}); err != nil {
		t.Fatal(err)
	}
	b := NewStoreBackend(st, "front-desk")
	b.FrontDesk = "front-desk"

	m := New(ctx, "front-desk", nil, nil).WithTranscript(transcript)
	m.Backend = b

	tm := teatest.NewTestModel(t, m, teatest.WithInitialTermSize(80, 24))
	var captured strings.Builder
	teatest.WaitFor(t, tm.Output(), func(p []byte) bool {
		captured.Write(p)
		s := captured.String()
		idx := strings.Index(s, "orca restarted (version c00d67a -> 9f8e7d6)")
		return idx >= 0 && strings.Contains(s[idx:], "front desk: no run yet")
	}, teatest.WithDuration(5*time.Second))
	liveScreenBytes := captured.String()
	finish(t, tm)

	vt := newFakeTerminal(80, 24)
	vt.write(liveScreenBytes)
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
	st := store.NewMemoryStore(nil)
	defer st.Close()
	if err := st.Runs().Create(ctx, store.Run{ID: "front-desk", Objective: "test header once"}); err != nil {
		t.Fatal(err)
	}
	b := NewStoreBackend(st, "front-desk")
	b.FrontDesk = "front-desk"

	m := New(ctx, "front-desk", nil, routeFunc(func(context.Context, string) (router.Decision, error) {
		return router.Decision{Handler: router.Concierge, Confidence: 1}, nil
	}))
	m.PollInterval = 25 * time.Millisecond
	m.Backend = b
	m.Dispatch = func(_ context.Context, _ router.Decision, text string, emit func(string)) error {
		emit("reply to " + text)
		return nil
	}

	tm := teatest.NewTestModel(t, m, teatest.WithInitialTermSize(80, 24))
	var captured strings.Builder
	waitCapture := func(substr string) {
		t.Helper()
		teatest.WaitFor(t, tm.Output(), func(p []byte) bool {
			captured.Write(p)
			return strings.Contains(captured.String(), substr)
		}, teatest.WithDuration(5*time.Second))
	}

	waitCapture("orca chat — run front-desk")
	submit(tm, "first turn")
	waitCapture("orca> reply to first turn")
	submit(tm, "second turn")
	waitCapture("orca> reply to second turn")
	finish(t, tm)
	rest, _ := io.ReadAll(tm.Output())
	captured.Write(rest)

	if count := strings.Count(captured.String(), "orca chat — run front-desk"); count != 1 {
		t.Fatalf("header 'orca chat — run front-desk' printed %d times, want 1:\n%q", count, captured.String())
	}
}
