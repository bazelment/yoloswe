package integration

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/bramble/session"
	"github.com/bazelment/yoloswe/bramble/tmuxctl"
)

// TestLiveProof_StrandedNudgeInAResidentPaneIsSubmitted is the live proof for
// issue #346: a real claude CLI, running in a real tmux pane on an isolated
// tmux server, with the pane already holding the exact text a previous nudge
// attempt would have stranded there (pasted but never submitted, e.g. by a
// killed process). The fixed Notifier.NotifyParent must recognize that text as
// its own and submit it, rather than reading it as a human's draft and
// yielding forever.
//
// Run manually (this target is tagged "manual" and excluded from
// `bazel test //...`):
//
//	bazel test //bramble/session/integration:integration_test \
//	  --test_filter=TestLiveProof_StrandedNudgeInAResidentPaneIsSubmitted \
//	  --test_output=all --test_arg=-test.v --test_timeout=120 \
//	  --test_env=BRAMBLE_CLAUDE_TRUSTED_WORKDIR=/path/already/trusted/by/claude
func TestLiveProof_StrandedNudgeInAResidentPaneIsSubmitted(t *testing.T) {
	trustedWorkdir := strings.TrimSpace(os.Getenv("BRAMBLE_CLAUDE_TRUSTED_WORKDIR"))
	if trustedWorkdir == "" {
		t.Skip("set BRAMBLE_CLAUDE_TRUSTED_WORKDIR to a directory already trusted by claude")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not available")
	}
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude CLI not available")
	}

	socketPath := filepath.Join(t.TempDir(), "tmux.sock")
	out, err := exec.Command("tmux", "-S", socketPath, "new-session", "-d",
		"-x", "220", "-y", "50", "-c", trustedWorkdir,
		"-P", "-F", "#{window_id}", "claude").Output()
	require.NoError(t, err, "start claude in an isolated tmux session")
	target := strings.TrimSpace(string(out))
	require.NotEmpty(t, target)
	t.Cleanup(func() {
		_ = exec.Command("tmux", "-S", socketPath, "kill-server").Run()
	})

	ctl := tmuxctl.NewWithSocketPath(socketPath)
	ctx := context.Background()

	// Wait for claude's own chrome (composer + status separators) to render.
	// The empty composer shows dimmed placeholder text ("Try ...") which also
	// starts with the prompt glyph, so waiting on the status rule rather than
	// the glyph itself is what actually pins "ready", not merely "painted".
	require.Eventually(t, func() bool {
		lines, err := ctl.Capture(ctx, target, 40)
		if err != nil {
			return false
		}
		return strings.Contains(strings.Join(lines, "\n"), "⏵⏵")
	}, 30*time.Second, 200*time.Millisecond, "claude did not render its composer in time")

	// Simulate the wedge: a previous nudge attempt pasted bramble's hint text
	// into this pane and then never got to send Enter (a killed process, a
	// dropped tmux write). The text is now stranded in the composer exactly as
	// a resident claude CLI would leave it for the next session.
	const strandedText = "[bramble] subagent activity — check your run directory"
	require.NoError(t, tmuxctl.Paste(ctx, ctl, target, strandedText))

	require.Eventually(t, func() bool {
		lines, err := ctl.Capture(ctx, target, 40)
		if err != nil {
			return false
		}
		return strings.Contains(strings.Join(lines, "\n"), strandedText)
	}, 10*time.Second, 200*time.Millisecond, "pasted text never reached the composer")

	before, err := ctl.Capture(ctx, target, 40)
	require.NoError(t, err)
	t.Logf("pane BEFORE nudge (stranded, unsubmitted composer content):\n%s", strings.Join(before, "\n"))

	// Build the exact Notifier a live bramble uses, wired to this one real
	// pane through a minimal DeliveryTarget instead of a full Manager/Registry.
	fake := &liveProofTarget{
		child: session.SessionInfo{
			ID:              "child",
			Status:          session.StatusIdle,
			RunnerType:      session.RunnerTypeTmux,
			ParentSessionID: "parent",
		},
		parent: session.SessionInfo{
			ID:         "parent",
			Status:     session.StatusIdle,
			RunnerType: session.RunnerTypeTmux,
			Backend:    "claude",
			Model:      "opus",
		},
		target: target,
		ctl:    ctl,
	}
	panes := tmuxctl.NewPaneWriter(ctl)
	notifier, err := session.NewNotifier(fake, panes, session.NotifierConfig{LegacyDeliveryDir: t.TempDir()})
	require.NoError(t, err)

	notifier.NotifyParent(ctx, fake.child)

	// The stranded line must be submitted (composer clears and the turn
	// starts, which claude shows as its "thinking" chrome replacing the empty
	// composer's placeholder), not left sitting there as an unsent draft
	// forever. Checking that the stranded text is gone from the *live*
	// composer line specifically — not merely absent from the whole pane —
	// matters because the same text reappears above, in the transcript, once
	// claude echoes the submitted prompt.
	require.Eventually(t, func() bool {
		lines, err := ctl.Capture(ctx, target, 40)
		if err != nil {
			return false
		}
		return !composerLineHolds(lines, strandedText)
	}, 30*time.Second, 200*time.Millisecond,
		"the stranded nudge was never submitted — the composer still holds it")

	after, err := ctl.Capture(ctx, target, 40)
	require.NoError(t, err)
	t.Logf("pane AFTER nudge (submitted, turn running/ran):\n%s", strings.Join(after, "\n"))
	require.True(t, fake.markedRunningAtLeastOnce, "the submit must have started a turn")

	// Cross-check with the issue's own detection command: grep -P '^\x{276f}'
	// over the captured pane finds every "❯"-prefixed line: both the
	// transcript's echo of the now-submitted prompt (rendered with a normal
	// space — claude only uses U+00A0 for the *live* composer) and, before
	// submission, the live composer itself. The gotcha is that a plain-space
	// pattern ('^❯ ') cannot tell these apart from the one line that actually
	// matters: an unsubmitted composer stays invisible to it exactly like a
	// submitted one, which is what makes the wedge silent in the first place.
	joined := strings.Join(after, "\n")
	fancyMatches := regexp.MustCompile(`(?m)^\x{276f}.*$`).FindAllString(joined, -1)
	require.NotEmpty(t, fancyMatches, "the real glyph pattern must find the transcript's echoed prompt")

	// The decisive check either way is the live composer specifically, which
	// composerLineHolds already isolated above (by position, not by regex) and
	// found empty. Confirmed again here for the record: the empty live
	// composer's own prompt line has nothing after its glyph to grep for at
	// all, plain-space or otherwise.
	require.False(t, composerLineHolds(after, strandedText),
		"the live composer must not hold the stranded text once submitted")
}

// composerLineHolds reports whether the pane's live composer line — the last
// line starting with claude's prompt glyph within the tail window before the
// bottom status chrome — contains want. Used to tell "submitted" (the
// composer went back to its empty placeholder) from "still staged".
func composerLineHolds(lines []string, want string) bool {
	for i := len(lines) - 1; i >= 0; i-- {
		trimmed := strings.TrimSpace(lines[i])
		if strings.HasPrefix(trimmed, "❯") {
			return strings.Contains(trimmed, want)
		}
	}
	return false
}

// liveProofTarget is a minimal session.DeliveryTarget wired to one real tmux
// pane, standing in for a full Manager/SessionRegistry so this proof exercises
// the real Notifier and real tmuxctl against a real claude CLI without
// spinning up bramble's whole session machinery.
type liveProofTarget struct { //nolint:govet // fieldalignment: readability over packing
	child, parent            session.SessionInfo
	target                   string
	ctl                      tmuxctl.Controller
	markedRunningAtLeastOnce bool
}

func (f *liveProofTarget) SessionInfo(id session.SessionID) (session.SessionInfo, bool) {
	switch id {
	case f.child.ID:
		return f.child, true
	case f.parent.ID:
		return f.parent, true
	default:
		return session.SessionInfo{}, false
	}
}

func (f *liveProofTarget) ResolveTmuxTarget(session.SessionID) (string, error) {
	return f.target, nil
}

func (f *liveProofTarget) CapturePaneText(_ session.SessionID, n int) ([]string, error) {
	return f.ctl.Capture(context.Background(), f.target, n)
}

func (f *liveProofTarget) MarkRunning(id session.SessionID) bool {
	if id != f.parent.ID || f.parent.Status != session.StatusIdle {
		return false
	}
	f.parent.Status = session.StatusRunning
	f.markedRunningAtLeastOnce = true
	return true
}

func (f *liveProofTarget) MarkIdle(id session.SessionID) {
	if id == f.parent.ID {
		f.parent.Status = session.StatusIdle
	}
}
