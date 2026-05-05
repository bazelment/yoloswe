package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/bazelment/yoloswe/jiradozer"
)

// bootstrapArgs holds bootstrap-only flags.
type bootstrapArgs struct {
	output string
	force  bool
}

func newBootstrapCommand(args *bootstrapArgs, configPath *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "bootstrap",
		Short: "Generate a starter config file",
		Long: `Write a starter config file seeded with the canonical step prompts and
comment templates. By default bootstrap writes to --config; use --output
to write somewhere else. Edit the prompts to taste; the generated file is
the source of truth for what the agent says.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			path := args.output
			if path == "" {
				path = *configPath
			}
			if path == "" {
				path = "jiradozer.yaml"
			}
			if _, err := os.Stat(path); err == nil && !args.force {
				return fmt.Errorf("%s already exists (use --force to overwrite)", path)
			} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("stat %s: %w", path, err)
			}
			content, err := bootstrapYAML()
			if err != nil {
				return err
			}
			if err := os.WriteFile(path, content, 0o644); err != nil {
				return fmt.Errorf("write %s: %w", path, err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "wrote %s\n", path)
			return nil
		},
	}
	cmd.SilenceUsage = true
	cmd.Flags().StringVarP(&args.output, "output", "o", "", "Path to write the starter config; overrides --config")
	cmd.Flags().BoolVarP(&args.force, "force", "f", false, "Overwrite the output file if it already exists")
	return cmd
}

// bootstrapYAML returns the bytes of a starter jiradozer.yaml. The output is
// hand-laid-out (not produced by yaml.Marshal) so each field carries the
// doc comment from its struct tag and optional fields can be emitted as
// commented-out lines that hint at usage without forcing a value.
func bootstrapYAML() ([]byte, error) {
	var b strings.Builder
	b.WriteString(bootstrapHeader)
	b.WriteString(bootstrapTrackerBlock)
	b.WriteString(bootstrapSourceBlock)
	b.WriteString(bootstrapStatesBlock)
	b.WriteString(bootstrapAgentBlock)
	b.WriteString(bootstrapWorkDirBlock)

	b.WriteString(renderStepBlock(stepBlock{
		Key:                  "plan",
		Heading:              "plan — write an implementation plan",
		Description:          "Agent reads the issue and writes an approach. permission_mode is `plan` so it cannot edit files. Output is posted to the issue and gated on a human reply.",
		Prompt:               jiradozer.BootstrapPlanPrompt,
		CommentTemplate:      jiradozer.BootstrapCompleteCommentTemplate,
		PermissionMode:       "plan",
		MaxTurns:             10,
		RoundsCapable:        false,
		RoundCommentTemplate: "",
	}))
	b.WriteString(renderStepBlock(stepBlock{
		Key:                  "build",
		Heading:              "build — implement the approved plan",
		Description:          "Agent edits files in work_dir to satisfy the plan. permission_mode is `bypass` so it can run tools without per-call prompts.",
		Prompt:               jiradozer.BootstrapBuildPrompt,
		CommentTemplate:      jiradozer.BootstrapCompleteCommentTemplate,
		PermissionMode:       "bypass",
		MaxTurns:             30,
		RoundsCapable:        true,
		RoundCommentTemplate: jiradozer.BootstrapRoundCommentTemplate,
	}))
	b.WriteString(renderStepBlock(stepBlock{
		Key:                  "create_pr",
		Heading:              "create_pr — commit, push, open the PR",
		Description:          "Agent stages/commits any pending changes, pushes the branch, and opens (or updates) a PR against base_branch. This step does not support `rounds`.",
		Prompt:               jiradozer.BootstrapCreatePRPrompt,
		CommentTemplate:      jiradozer.BootstrapCompleteCommentTemplate,
		PermissionMode:       "bypass",
		MaxTurns:             5,
		RoundsCapable:        false,
		RoundCommentTemplate: "",
	}))
	b.WriteString(renderStepBlock(stepBlock{
		Key:                  "validate",
		Heading:              "validate — run tests/linters and fix failures",
		Description:          "Agent runs the project's tests and linters in the PR branch and fixes anything it broke before handing back to the reviewer.",
		Prompt:               jiradozer.BootstrapValidatePrompt,
		CommentTemplate:      jiradozer.BootstrapCompleteCommentTemplate,
		PermissionMode:       "bypass",
		MaxTurns:             10,
		RoundsCapable:        true,
		RoundCommentTemplate: jiradozer.BootstrapRoundCommentTemplate,
	}))
	b.WriteString(renderStepBlock(stepBlock{
		Key:                  "ship",
		Heading:              "ship — final PR readiness pass",
		Description:          "Agent makes sure the PR exists, has a good title/body, and is ready for review. Jiradozer does not merge for you — it stops at the ShipReview gate.",
		Prompt:               jiradozer.BootstrapShipPrompt,
		CommentTemplate:      jiradozer.BootstrapCompleteCommentTemplate,
		PermissionMode:       "bypass",
		MaxTurns:             10,
		RoundsCapable:        true,
		RoundCommentTemplate: jiradozer.BootstrapRoundCommentTemplate,
	}))

	b.WriteString(bootstrapTopLevelTail)
	return []byte(b.String()), nil
}

// bootstrapHeader is the documentation block prepended to the rest of the
// file. It explains what each step does and how the review gates fit
// together so a new user can read jiradozer.yaml top-to-bottom.
const bootstrapHeader = "# jiradozer.yaml — generated by `jiradozer bootstrap`. Edit prompts to taste.\n" + `#
# How jiradozer works
# ===================
# Jiradozer drives a tracker issue (Linear, GitHub, or local) through five
# agent-run steps:
#
#   plan → build → create_pr → validate → ship
#
#   1. plan      — agent reads the issue and writes an implementation plan
#                  (permission_mode: plan, no code changes).
#   2. build     — agent implements the approved plan in your work_dir.
#   3. create_pr — agent commits, pushes, and opens a PR against base_branch.
#   4. validate  — agent runs tests/linters in the PR branch and fixes failures.
#   5. ship      — agent makes sure the PR is up to date and ready for review.
#
# After plan, build, validate, and ship, jiradozer posts the agent's output
# as a comment on the issue and waits at a review gate. Reply on the issue:
#   - "approve" / "lgtm"  → next step
#   - "approve all" / "approve_all" / "yolo" → approve this and all remaining gates in the current run
#   - "redo"              → re-run the current step
#   - any other text      → fed back to the agent as feedback
#
# Each step block below configures one of those agent sessions: the prompt
# rendered with issue data, the comment template posted afterward, the
# permission mode, and turn/budget caps. Optional fields are commented out
# — uncomment to override defaults.
#
# Multi-round steps (advanced)
# ----------------------------
# Any step except create_pr can replace its single ` + "`prompt`" + ` with a list of
# ` + "`rounds`" + ` that run sequentially inside one review gate. Each round is either
# an agent run (` + "`prompt:`" + `) or a shell command (` + "`command:`" + ` via ` + "`sh -c`" + `).
# Per-round comments use ` + "`round_comment_template`" + `; the step's review gate
# fires once after the last round. Round-level fields override the step's
# zero-valued ones; feedback from a redo is injected into the first agent
# round only. See the commented ` + "`rounds`" + ` example under each step.
#
# Template variables in step prompts and round prompts:
#   {{.Identifier}} {{.Title}} {{.Description}} {{.URL}} {{.Labels}}
#   {{.BaseBranch}} {{.Plan}} {{.BuildOutput}}
#
# Template variables in comment_template / round_comment_template:
#   {{.Step}} {{.Heading}} {{.Output}} {{.Round}} {{.TotalRounds}}

`

// bootstrapTrackerBlock — issue tracker backend. Required.
const bootstrapTrackerBlock = `# Issue tracker backend.
tracker:
    # Backend kind: "linear", "github", or "local".
    kind: linear
    # API key; supports "$ENV_VAR" expansion. Required for linear.
    # github uses the gh CLI for auth, local needs no key.
    api_key: $LINEAR_API_KEY

`

// bootstrapSourceBlock — multi-issue discovery. Optional. Single-issue
// runs (--issue ENG-123) ignore this block, so it is commented out by
// default; uncomment when you want jiradozer to poll a tracker query.
const bootstrapSourceBlock = `# Multi-issue discovery (used when --filter is set or filters is non-empty).
# Single-issue runs (--issue ENG-123) ignore this block.
#source:
#    # Generic key-value filters; see tracker.IssueFilter for keys.
#    filters:
#        state: Todo
#        team: ENG
#    # Worktree branch prefix (default: jiradozer).
#    branch_prefix: jiradozer
#    # Max parallel workflows (default: 3).
#    max_concurrent: 3
#    # Print equivalent bramble new-session command instead of launching a workflow.
#    dry_run: false

`

// bootstrapStatesBlock — tracker state names. Required (defaults shown).
const bootstrapStatesBlock = `# Logical workflow states → tracker state names.
states:
    in_progress: In Progress
    in_review: In Review
    done: Done

`

// bootstrapAgentBlock — top-level agent defaults. Required model, optional
// effort.
const bootstrapAgentBlock = `# Default agent backend; per-step overrides win when set.
agent:
    # Model ID from agent.AllModels (e.g. "sonnet", "opus", "gpt-5.3-codex",
    # "gemini-3.1-pro-preview", "cursor-default").
    model: sonnet
    # Reasoning effort: low, medium, high, max, auto. Empty = provider default.
    #effort: ""

`

// bootstrapWorkDirBlock — required scalars at the top level.
const bootstrapWorkDirBlock = `# Working directory for the agent (where it runs commands and edits files).
work_dir: .

# Default base branch for PRs.
base_branch: main

`

// bootstrapTopLevelTail — top-level scalars that come after the step blocks.
const bootstrapTopLevelTail = `# Total budget cap across all steps in a single workflow run.
max_budget_usd: 50

# How often to poll the tracker for new comments at review gates.
poll_interval: 15s
`

// roundsExampleBlock is the commented-out multi-round example emitted under
// every rounds-capable step. Mutually exclusive with the step's `prompt`
// — uncomment and delete `prompt:` above to switch the step into rounds
// mode. Indented 4 spaces to match the step's child-key indent.
//
// Layout invariant: prose lives in a comment header *above* `#rounds:`;
// every line at-or-below `#rounds:` is YAML-shaped so it parses cleanly
// after the user (or TestRoundsExampleUncommentsCleanly) strips the
// leading `    #` from each line.
const roundsExampleBlock = `    # Multi-round execution. Replaces the single ` + "`prompt`" + ` above with a
    # sequence of rounds run inside one review gate. Each round is either
    # an agent run (` + "`prompt:`" + `) or a shell command (` + "`command:`" + ` via ` + "`sh -c`" + `).
    # Per-round comments use ` + "`round_comment_template`" + `; the step's review
    # gate fires once after the last round. Round-level fields override
    # the step's zero-valued ones; redo feedback is injected into the
    # first agent round only. Mutually exclusive with ` + "`prompt`" + ` above —
    # delete the prompt field when you enable rounds.
    #
    # Per-round override fields (all optional, 0/empty = inherit from step):
    #   model, system_prompt, max_turns, max_budget_usd, max_tool_error_retries
    #
    # WARNING for command rounds: {{.Title}} / {{.Description}} come from
    # the tracker and are user-controlled. Only interpolate them when the
    # issue source is fully trusted. Feedback is not injected into commands.
    #rounds:
    #    - prompt: |-
    #          Round 1 work for {{.Identifier}} — {{.Title}}.
    #      max_turns: 15
    #    - command: "go test ./..."
`

// stepBlock is the data we need to lay out one step in the YAML.
type stepBlock struct {
	Key                  string // YAML key, e.g. "plan"
	Heading              string // human heading shown in the section comment
	Description          string // one-paragraph blurb explaining what this step does
	Prompt               string // canonical prompt (required)
	CommentTemplate      string // canonical comment template (required for single-shot steps)
	PermissionMode       string // step default permission mode
	RoundCommentTemplate string // canonical round comment template (only used when RoundsCapable)
	MaxTurns             int    // step default max turns
	RoundsCapable        bool   // whether bootstrap should seed round_comment_template uncommented
}

// renderStepBlock emits one step's YAML with field-level comments. Optional
// fields are written as commented-out lines so users see the field exists
// without it taking effect.
func renderStepBlock(s stepBlock) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# ---- %s ----\n", s.Heading)
	fmt.Fprintf(&b, "# %s\n", wrapComment(s.Description, 76))
	fmt.Fprintf(&b, "%s:\n", s.Key)

	b.WriteString("    # Initial prompt; Go text/template rendered with issue data (PromptData).\n")
	b.WriteString("    # Required unless `rounds` is set.\n")
	b.WriteString("    prompt: |-\n")
	b.WriteString(indentBlock(s.Prompt, "        "))
	b.WriteString("\n")

	b.WriteString("    # Comment posted to the issue when this step finishes (single-shot mode).\n")
	b.WriteString("    # Required unless this step uses `rounds` only.\n")
	b.WriteString("    comment_template: |-\n")
	b.WriteString(indentBlock(s.CommentTemplate, "        "))
	b.WriteString("\n")

	fmt.Fprintf(&b, "    # Agent permission mode: plan, bypass, default, acceptEdits.\n")
	fmt.Fprintf(&b, "    permission_mode: %s\n", s.PermissionMode)

	fmt.Fprintf(&b, "    # Max agent turns before giving up (Claude provider only).\n")
	fmt.Fprintf(&b, "    max_turns: %d\n", s.MaxTurns)

	if s.RoundsCapable {
		b.WriteString("    # Per-round comment posted when this step runs as multi-round.\n")
		b.WriteString("    # Validated even when `rounds` is empty so a typo here fails fast.\n")
		b.WriteString("    round_comment_template: |-\n")
		b.WriteString(indentBlock(s.RoundCommentTemplate, "        "))
		b.WriteString("\n")
	}

	b.WriteString("    # ----- optional overrides (uncomment to use) -----\n")
	b.WriteString("    # Optional system prompt prepended to the agent session.\n")
	b.WriteString("    #system_prompt: \"\"\n")
	b.WriteString("    # Override agent.model for this step; empty = inherit.\n")
	b.WriteString("    #model: \"\"\n")
	b.WriteString("    # Override agent.effort for this step; empty = inherit.\n")
	b.WriteString("    #effort: \"\"\n")
	if !s.RoundsCapable && s.Key != "create_pr" {
		// plan is single-shot in the bootstrap shape; round_comment_template is
		// still allowed but won't be exercised. create_pr cannot have rounds at
		// all (validate() rejects it), so we don't even hint at the field there.
		b.WriteString("    # Per-round comment template; required if you set `rounds` below.\n")
		b.WriteString("    #round_comment_template: \"\"\n")
	}
	if s.Key != "create_pr" {
		b.WriteString(roundsExampleBlock)
	}
	b.WriteString("    # Step-level budget cap in USD; 0 = inherit max_budget_usd.\n")
	b.WriteString("    #max_budget_usd: 0\n")
	b.WriteString("    # Auto-retry when a turn ends with an unresolved tool error.\n")
	b.WriteString("    #max_tool_error_retries: 0\n")
	b.WriteString("    # Skip the human review gate after this step.\n")
	b.WriteString("    #auto_approve: false\n")

	b.WriteString("\n")
	return b.String()
}

// indentBlock prefixes every non-empty line of s with prefix. Empty lines
// stay truly empty so the rendered YAML doesn't carry trailing-whitespace
// noise in `|-` block scalars.
func indentBlock(s, prefix string) string {
	if s == "" {
		return prefix
	}
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		if line == "" {
			continue
		}
		lines[i] = prefix + line
	}
	return strings.Join(lines, "\n")
}

// wrapComment soft-wraps s at width characters, joining lines with
// "\n# " so the result can be inlined into a comment block. The first
// line is returned without a leading "# " (the caller already wrote one).
func wrapComment(s string, width int) string {
	words := strings.Fields(s)
	if len(words) == 0 {
		return ""
	}
	var lines []string
	current := words[0]
	for _, w := range words[1:] {
		if len(current)+1+len(w) > width {
			lines = append(lines, current)
			current = w
			continue
		}
		current += " " + w
	}
	lines = append(lines, current)
	return strings.Join(lines, "\n# ")
}
