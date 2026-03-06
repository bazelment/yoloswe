// Command tmuxwatch monitors Claude Code tmux windows and prints structured
// status information periodically. Used for developing and validating tmux
// integration features in bramble.
//
// Usage:
//
//	bazel run //bramble/cmd/tmuxwatch 2>/tmp/tmuxwatch.log
//
// Stdout shows a live-updating dashboard. Stderr logs structured state data
// for post-analysis (grep for state changes, token drift, context compaction).
//
// # Claude Code TUI Layout (observed 2026-03)
//
// The bottom of every Claude Code pane has a consistent layout:
//
//	<agent output / tool results>
//	❯ <user input>                              ← idle prompt (or spinner when working)
//	✻ Worked for 36m 36s                        ← completion indicator (optional)
//	─────────────────────────────────────────── ← separator (always present, ─{10,})
//	  ~/path  branch  Model  ctx:XX%  tokens:NNk [Context left until auto-compact: N%]
//	  ⏵⏵ bypass permissions on (...) [· PR #NNN]
//
// # Known Character Variants
//
// Completion indicators (turn just finished, effectively idle):
//
//	✻ ✢ ✽ ✹  — followed by food-themed verb ("Worked", "Baked", "Sautéed", etc.)
//
// Spinner characters (actively working):
//   - · ⠋ ⠙ ⠹ ⠸ ⠼ ⠴ ⠦ ⠧ ⠇ ⠏ — followed by food-themed verb ("Frosting…", "Creating…")
//
// Tool execution (content, not chrome):
//
//	● — tool name follows, e.g. "● Bash(git status)"
//
// # Parsing Caveats
//
//   - The token count field may have trailing text when context is high:
//     "tokens:50k                     Context left until auto-compact: 5%"
//     Regex must not be greedy after the token count.
//
//   - Context compaction causes ctx% to drop sharply (e.g. 79% → 10%).
//     Token count continues to grow through compaction.
//
//   - Working state is transient (sub-second transitions). At 15s polling,
//     you'll almost never catch a spinner — recent output lines are more
//     informative about what happened.
//
//   - The ⏵⏵ prefix is multi-byte UTF-8. The permissions line may have
//     trailing metadata: "· PR #930", "· 4 bashes", "· Replace gcloud CLI..."
//
//   - When a session has multiple panes, capture-pane targets the active pane.
//     Bramble sessions are always single-pane.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/bazelment/yoloswe/bramble/session"
)

// tmuxWindow holds info about a discovered tmux window.
type tmuxWindow struct {
	Index string
	ID    string
	Name  string
}

func listWindows() []tmuxWindow {
	cmd := exec.Command("tmux", "list-windows", "-F", "#{window_index} #{window_id} #{window_name}")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var windows []tmuxWindow
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		parts := strings.SplitN(line, " ", 3)
		if len(parts) < 3 {
			continue
		}
		windows = append(windows, tmuxWindow{Index: parts[0], ID: parts[1], Name: parts[2]})
	}
	return windows
}

// isClaudeWindow heuristically identifies a Claude Code window by checking
// if the pane capture contains the Claude status bar separator + ctx: pattern.
func isClaudeWindow(windowID string) bool {
	lines, err := session.CaptureTmuxPane(windowID, 10)
	if err != nil {
		return false
	}
	for _, line := range lines {
		if strings.Contains(line, "ctx:") && strings.Contains(line, "tokens:") {
			return true
		}
	}
	return false
}

func renderStatusBox(w tmuxWindow, lines []string, ps *session.PaneStatus, width int) string {
	var b strings.Builder

	sep := strings.Repeat("─", width)
	b.WriteString(fmt.Sprintf("╭%s╮\n", sep))

	// Header line
	header := fmt.Sprintf(" Window %s [%s] %s ", w.Index, w.ID, w.Name)
	if len(header) > width {
		header = header[:width]
	}
	b.WriteString(fmt.Sprintf("│%-*s│\n", width, header))

	// Status bar info
	if ps != nil {
		info := fmt.Sprintf(" Model: %-12s  Branch: %-30s  ctx:%s  tokens:%s",
			ps.Model, ps.Branch, ps.ContextPct, ps.TokenCount)
		if len(info) > width {
			info = info[:width]
		}
		b.WriteString(fmt.Sprintf("│%-*s│\n", width, info))

		stateStr := "unknown"
		if ps.IsIdle {
			stateStr = "IDLE (awaiting input)"
		} else if ps.IsWorking {
			stateStr = "WORKING"
		}
		state := fmt.Sprintf(" State: %-20s", stateStr)
		if ps.PRNumber != "" {
			state += fmt.Sprintf("  PR: #%s", ps.PRNumber)
		}
		if ps.Permissions != "" {
			state += fmt.Sprintf("  Perms: %s", ps.Permissions)
		}
		if len(state) > width {
			state = state[:width]
		}
		b.WriteString(fmt.Sprintf("│%-*s│\n", width, state))

		if ps.StatusLine != "" {
			sl := fmt.Sprintf(" Status: %s", ps.StatusLine)
			if len(sl) > width {
				sl = sl[:width]
			}
			b.WriteString(fmt.Sprintf("│%-*s│\n", width, sl))
		}
	} else {
		b.WriteString(fmt.Sprintf("│%-*s│\n", width, " (no status bar parsed)"))
	}

	// Separator
	b.WriteString(fmt.Sprintf("│%s│\n", sep))

	// Recent output lines — use ContentLines to strip all TUI chrome.
	displayLines := session.ContentLines(lines, ps)
	if len(displayLines) > 6 {
		displayLines = displayLines[len(displayLines)-6:]
	}
	for _, line := range displayLines {
		truncated := line
		if len(truncated) > width {
			truncated = truncated[:width-1] + "…"
		}
		b.WriteString(fmt.Sprintf("│%-*s│\n", width, " "+truncated))
	}
	// Pad to 6 lines
	for i := len(displayLines); i < 6; i++ {
		b.WriteString(fmt.Sprintf("│%-*s│\n", width, ""))
	}

	b.WriteString(fmt.Sprintf("╰%s╯", sep))
	return b.String()
}

func main() {
	duration := 10 * time.Minute
	interval := 15 * time.Second

	fmt.Printf("tmuxwatch: monitoring Claude Code windows for %v (every %v)\n", duration, interval)
	fmt.Printf("Started at: %s\n\n", time.Now().Format("15:04:05"))

	deadline := time.Now().Add(duration)
	iteration := 0

	for time.Now().Before(deadline) {
		iteration++
		now := time.Now()
		remaining := time.Until(deadline).Round(time.Second)

		// Discover Claude windows
		allWindows := listWindows()
		var claudeWindows []tmuxWindow
		for _, w := range allWindows {
			if isClaudeWindow(w.ID) {
				claudeWindows = append(claudeWindows, w)
			}
		}

		// Clear screen with ANSI
		fmt.Print("\033[2J\033[H")
		fmt.Printf("tmuxwatch — iteration %d — %s — remaining: %v — %d Claude windows\n",
			iteration, now.Format("15:04:05"), remaining, len(claudeWindows))
		fmt.Println(strings.Repeat("═", 120))

		if len(claudeWindows) == 0 {
			fmt.Println("No Claude Code windows found.")
		}

		for _, w := range claudeWindows {
			lines, cursorY, err := session.CaptureTmuxPaneFull(w.ID)
			if err != nil {
				fmt.Printf("  Error capturing %s: %v\n", w.ID, err)
				continue
			}

			ps := session.ParseClaudeStatusBarWithCursor(lines, cursorY)
			if ps == nil {
				// Fallback to legacy separator scanning.
				ps = session.ParseClaudeStatusBar(lines)
			}
			box := renderStatusBox(w, lines, ps, 118)
			fmt.Println(box)
		}

		// Log state changes to stderr for post-analysis
		for _, w := range claudeWindows {
			lines, cursorY, _ := session.CaptureTmuxPaneFull(w.ID)
			ps := session.ParseClaudeStatusBarWithCursor(lines, cursorY)
			if ps == nil {
				ps = session.ParseClaudeStatusBar(lines)
			}
			if ps != nil {
				state := "unknown"
				if ps.IsIdle {
					state = "idle"
				} else if ps.IsWorking {
					state = "working"
				}
				fmt.Fprintf(os.Stderr, "[%s] %s (%s) state=%s model=%s ctx=%s tokens=%s status=%q\n",
					now.Format("15:04:05"), w.Name, w.ID,
					state, ps.Model, ps.ContextPct, ps.TokenCount, ps.StatusLine)
			}
		}

		time.Sleep(interval)
	}

	fmt.Printf("\n\ntmuxwatch: completed %d iterations over %v\n", iteration, duration)
}
