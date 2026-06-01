package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/bazelment/yoloswe/jiradozer"
	"github.com/bazelment/yoloswe/wt"
)

// bootstrapArgs holds bootstrap-only flags.
type bootstrapArgs struct {
	output string
	repo   string
	force  bool
}

type bootstrapRepoWorktreeResult struct {
	workDir    string
	baseBranch string
}

func newBootstrapCommand(args *bootstrapArgs, configPath *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "bootstrap",
		Short: "Generate a starter config file",
		Long: `Write a starter config file seeded with the canonical step prompts and
comment templates. By default bootstrap writes to --config; use --output
to write somewhere else. With --repo, the default config path is next to
the wt-managed checkout unless --config or --output is explicit. Edit the
prompts to taste; the generated file is the source of truth for what the
agent says.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			configExplicit := cmd.Flags().Changed("config") || cmd.InheritedFlags().Changed("config")
			path, err := resolveBootstrapOutputPath(args, *configPath, configExplicit)
			if err != nil {
				return err
			}
			if _, err := os.Stat(path); err == nil && !args.force {
				if args.repo != "" && args.output == "" && !configExplicit {
					if _, err := recoverExistingRepoWorktree(cmd.Context(), args.repo, cmd.OutOrStdout()); err != nil {
						return err
					}
				}
				return fmt.Errorf("%s already exists (use --force to overwrite)", path)
			} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("stat %s: %w", path, err)
			}
			repoResult := bootstrapRepoWorktreeResult{}
			if args.repo != "" {
				repoResult, err = bootstrapRepoWorktree(cmd.Context(), args.repo, cmd.OutOrStdout())
				if err != nil {
					return err
				}
			}
			content, err := bootstrapYAML(repoResult.workDir, repoResult.baseBranch)
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
	cmd.Flags().StringVar(&args.repo, "repo", "", "Optional GitHub repo URL or owner/repo shorthand. Bootstraps a wt-managed worktree under $WT_ROOT (or ~/worktrees), reusing it if present, and points work_dir at that checkout. Without explicit --config or --output, writes config beside that checkout.")
	cmd.Flags().BoolVarP(&args.force, "force", "f", false, "Overwrite the output file if it already exists")
	return cmd
}

func resolveBootstrapOutputPath(args *bootstrapArgs, configPath string, configExplicit bool) (string, error) {
	path := args.output
	if path == "" {
		path = configPath
	}
	if path == "" {
		path = "jiradozer.yaml"
	}
	if args.repo == "" || args.output != "" || configExplicit {
		return path, nil
	}
	wtRoot, err := resolveWTRoot()
	if err != nil {
		return "", err
	}
	repoURL := normalizeRepoURL(args.repo)
	return filepath.Join(wtRoot, wt.GetRepoNameFromURL(repoURL), "jiradozer.yaml"), nil
}

func recoverExistingRepoWorktree(ctx context.Context, repoArg string, out io.Writer) (bootstrapRepoWorktreeResult, error) {
	repoURL := normalizeRepoURL(repoArg)
	wtRoot, err := resolveWTRoot()
	if err != nil {
		return bootstrapRepoWorktreeResult{}, err
	}
	mgr := newBootstrapWTManager(wtRoot, repoURL, out)
	if _, err := os.Stat(mgr.BareDir()); errors.Is(err, fs.ErrNotExist) {
		return bootstrapRepoWorktreeResult{}, nil
	} else if err != nil {
		return bootstrapRepoWorktreeResult{}, fmt.Errorf("stat %s: %w", mgr.BareDir(), err)
	}
	return bootstrapRepoWorktreeInternal(ctx, repoArg, out, false)
}

func bootstrapRepoWorktree(ctx context.Context, repoArg string, out io.Writer) (bootstrapRepoWorktreeResult, error) {
	return bootstrapRepoWorktreeInternal(ctx, repoArg, out, true)
}

func bootstrapRepoWorktreeInternal(ctx context.Context, repoArg string, out io.Writer, announceReuse bool) (bootstrapRepoWorktreeResult, error) {
	repoURL := normalizeRepoURL(repoArg)
	wtRoot, err := resolveWTRoot()
	if err != nil {
		return bootstrapRepoWorktreeResult{}, err
	}
	mgr := newBootstrapWTManager(wtRoot, repoURL, out)
	if _, err := os.Stat(mgr.BareDir()); err == nil {
		if err := verifyExistingRepoRemote(ctx, mgr, repoURL); err != nil {
			return bootstrapRepoWorktreeResult{}, err
		}
		defaultBranch, err := wt.GetDefaultBranch(ctx, mgr.GitRunner(), mgr.BareDir())
		if err != nil {
			return bootstrapRepoWorktreeResult{}, fmt.Errorf("detect default branch for existing repo: %w", err)
		}
		mainPath, err := mgr.GetWorktreePath(defaultBranch)
		if errors.Is(err, wt.ErrWorktreeNotFound) {
			mainPath, err = recreateDefaultWorktree(ctx, mgr, defaultBranch)
		}
		if err != nil {
			return bootstrapRepoWorktreeResult{}, fmt.Errorf("existing repo worktree for %s: %w", defaultBranch, err)
		}
		if err := verifyGitWorktree(mainPath); err != nil {
			return bootstrapRepoWorktreeResult{}, fmt.Errorf("existing repo worktree for %s: %w", defaultBranch, err)
		}
		if announceReuse {
			fmt.Fprintf(out, "reusing existing repo at %s\n", mgr.RepoDir())
		}
		return bootstrapRepoWorktreeResult{workDir: mainPath, baseBranch: defaultBranch}, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return bootstrapRepoWorktreeResult{}, fmt.Errorf("stat %s: %w", mgr.BareDir(), err)
	}
	mainPath, err := mgr.Init(ctx, repoURL)
	if err != nil {
		return bootstrapRepoWorktreeResult{}, err
	}
	baseBranch, err := branchFromWorktreePath(mgr.RepoDir(), mainPath)
	if err != nil {
		return bootstrapRepoWorktreeResult{}, err
	}
	return bootstrapRepoWorktreeResult{workDir: mainPath, baseBranch: baseBranch}, nil
}

func branchFromWorktreePath(repoDir string, workDir string) (string, error) {
	rel, err := filepath.Rel(repoDir, workDir)
	if err != nil {
		return "", fmt.Errorf("resolve default branch from %s: %w", workDir, err)
	}
	return filepath.ToSlash(rel), nil
}

func verifyGitWorktree(path string) error {
	if _, err := os.Stat(filepath.Join(path, ".git")); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%s is not a git worktree", path)
		}
		return fmt.Errorf("stat %s: %w", filepath.Join(path, ".git"), err)
	}
	return nil
}

func newBootstrapWTManager(wtRoot, repoURL string, out io.Writer) *wt.Manager {
	return wt.NewManager(wtRoot, wt.GetRepoNameFromURL(repoURL), wt.WithOutput(wt.NewOutput(out, false)))
}

func verifyExistingRepoRemote(ctx context.Context, mgr *wt.Manager, repoURL string) error {
	result, err := mgr.GitRunner().Run(ctx, []string{"config", "--get", "remote.origin.url"}, mgr.BareDir())
	if err != nil {
		return fmt.Errorf("read existing repo remote: %w", err)
	}
	got := strings.TrimSpace(result.Stdout)
	if sameRepoRemote(got, repoURL) {
		return nil
	}
	return fmt.Errorf("existing repo at %s uses remote %q, not %q", mgr.RepoDir(), got, repoURL)
}

func sameRepoRemote(a, b string) bool {
	return canonicalRepoRemote(a) == canonicalRepoRemote(b)
}

func canonicalRepoRemote(remote string) string {
	remote = strings.TrimSuffix(strings.TrimSpace(remote), ".git")
	if host, path, ok := splitSCPRemote(remote); ok {
		return strings.ToLower(host + "/" + strings.Trim(path, "/"))
	}
	if parsed, err := url.Parse(remote); err == nil && parsed.Host != "" {
		return strings.ToLower(parsed.Host + "/" + strings.Trim(parsed.Path, "/"))
	}
	return remote
}

func splitSCPRemote(remote string) (string, string, bool) {
	if !strings.Contains(remote, "://") && strings.Contains(remote, "@") && strings.Contains(remote, ":") {
		parts := strings.SplitN(remote, "@", 2)
		hostPath := strings.SplitN(parts[1], ":", 2)
		if len(hostPath) == 2 && hostPath[0] != "" && hostPath[1] != "" {
			return hostPath[0], hostPath[1], true
		}
	}
	return "", "", false
}

func recreateDefaultWorktree(ctx context.Context, mgr *wt.Manager, defaultBranch string) (string, error) {
	mainPath := filepath.Join(mgr.RepoDir(), defaultBranch)
	if _, err := mgr.GitRunner().Run(ctx, []string{"worktree", "prune"}, mgr.BareDir()); err != nil {
		return "", fmt.Errorf("prune stale worktree metadata: %w", err)
	}
	result, err := mgr.GitRunner().Run(ctx, []string{"worktree", "add", mainPath, defaultBranch}, mgr.BareDir())
	if err != nil {
		if result != nil {
			if stderr := strings.TrimSpace(result.Stderr); stderr != "" {
				return "", fmt.Errorf("recreate default worktree: %s: %w", stderr, err)
			}
		}
		return "", fmt.Errorf("recreate default worktree: %w", err)
	}
	return mainPath, nil
}

var ownerRepoPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

func normalizeRepoURL(repoArg string) string {
	if ownerRepoPattern.MatchString(repoArg) {
		return "https://github.com/" + strings.TrimSuffix(repoArg, ".git") + ".git"
	}
	return repoArg
}

// bootstrapYAML returns the bytes of a starter jiradozer.yaml. The output is
// hand-laid-out (not produced by yaml.Marshal) so each field carries the
// doc comment from its struct tag and optional fields can be emitted as
// commented-out lines that hint at usage without forcing a value.
func bootstrapYAML(workDir string, baseBranchOverride ...string) ([]byte, error) {
	baseBranch := ""
	if len(baseBranchOverride) > 0 {
		baseBranch = baseBranchOverride[0]
	}
	var b strings.Builder
	b.WriteString(bootstrapHeader)
	b.WriteString(bootstrapTrackerBlock)
	b.WriteString(bootstrapSourceBlock)
	b.WriteString(bootstrapStatesBlock)
	b.WriteString(bootstrapAgentBlock)
	b.WriteString(renderWorkDirBlock(workDir, baseBranch))

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
		IdleTimeout:          5 * time.Minute,
		IdleTimeoutComment:   "Tracks gap between log lines, not wall-clock-since-start, so a slow-but-progressing agent is not interrupted. 0 disables the watchdog.",
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
		IdleTimeout:          20 * time.Minute,
		IdleTimeoutComment:   "Build runs the longest because tests can take many minutes; the gap-based watchdog only trips when output truly stops.",
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
		IdleTimeout:          5 * time.Minute,
		IdleTimeoutComment:   "create_pr is short — gh + git push only — so a tight timeout catches gh hangs quickly.",
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
		IdleTimeout:          20 * time.Minute,
		IdleTimeoutComment:   "Validate runs tests + linters (potentially long); the gap-based watchdog only trips when output truly stops.",
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
		IdleTimeout:          5 * time.Minute,
		IdleTimeoutComment:   "Ship is short — gh PR-update only — so a tight timeout catches gh hangs quickly.",
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
    # Optional: route inference through a third-party LLM API endpoint
    # (Baseten, OpenRouter, LiteLLM, etc.). The claude backend requires an
    # Anthropic-shaped endpoint; codex/gemini accept OpenAI-compatible.
    #llm_endpoint:
    #    base_url: https://inference.baseten.co/v1
    #    api_key_env: BASETEN_API_KEY    # prefer over api_key
    #    provider_name: baseten
    #    wire_api: chat                  # "chat" (OpenAI-compat) or "responses"
    #    headers:                        # optional extra HTTP headers (codex only)
    #        X-Custom-Header: value

`

// renderWorkDirBlock renders required scalar config values.
func renderWorkDirBlock(workDir string, baseBranch string) string {
	if workDir == "" {
		workDir = "."
	} else {
		workDir = strconv.Quote(workDir)
	}
	if baseBranch == "" {
		baseBranch = "main"
	}
	return fmt.Sprintf(bootstrapWorkDirBlock, workDir, baseBranch)
}

const bootstrapWorkDirBlock = `# Working directory for the agent (where it runs commands and edits files).
work_dir: %s

# Default base branch for PRs.
base_branch: %s

# Optional phases to skip for every run. Skipped phases advance in-memory
# workflow state and clear stale -inprogress labels, but do not write -done
# labels; use tracker labels like jiradozer-plan-done for durable completion.
# Tracker labels like jiradozer-skip-plan are also honored; label editors are
# trusted to bypass the matching phase.
#skip_phases: [plan]

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
	Key                  string        // YAML key, e.g. "plan"
	Heading              string        // human heading shown in the section comment
	Description          string        // one-paragraph blurb explaining what this step does
	Prompt               string        // canonical prompt (required)
	CommentTemplate      string        // canonical comment template (required for single-shot steps)
	PermissionMode       string        // step default permission mode
	RoundCommentTemplate string        // canonical round comment template (only used when RoundsCapable)
	IdleTimeoutComment   string        // step-specific blurb describing why this timeout (rendered in YAML comment); only emitted when IdleTimeout > 0
	MaxTurns             int           // step default max turns
	IdleTimeout          time.Duration // step default idle timeout; 0 = omit from rendered YAML
	RoundsCapable        bool          // whether bootstrap should seed round_comment_template uncommented
}

// renderStepBlock emits one step's YAML with field-level comments. Optional
// fields are written as commented-out lines so users see the field exists
// without it taking effect.
func renderStepBlock(s stepBlock) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# ---- %s ----\n", s.Heading)
	fmt.Fprintf(&b, "# %s\n", wrapComment(s.Description, 76, "# "))
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

	if s.IdleTimeout > 0 {
		b.WriteString("    # Parent kills the subprocess if it emits no log line for this long while\n")
		// firstLinePrefix is 22 visible chars; subtract from the 76-char
		// target so wrapped continuation lines stay inside the right margin.
		const firstLinePrefix = "    # inside this step. "
		const contPrefix = "    # "
		fmt.Fprintf(&b, "%s%s\n",
			firstLinePrefix,
			wrapComment(s.IdleTimeoutComment, 76-len(firstLinePrefix), contPrefix))
		fmt.Fprintf(&b, "    idle_timeout: %s\n", formatDurationShort(s.IdleTimeout))
	}

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
	b.WriteString("    # Grace period a turn waits, after completion, for outstanding background\n")
	b.WriteString("    # work (e.g. bramble reviewers a skill backgrounds) to finish before the\n")
	b.WriteString("    # turn is force-completed; 0 = provider default (10m). Raise it for steps\n")
	b.WriteString("    # that launch long-running background tools.\n")
	b.WriteString("    #stream_turn_grace_period: 0\n")
	b.WriteString("    # Skip the human review gate after this step.\n")
	b.WriteString("    #auto_approve: false\n")

	b.WriteString("\n")
	return b.String()
}

// formatDurationShort trims trailing "0s" / "0m" so 5m0s renders as "5m"
// and 1h0m as "1h" — the form a human would write into YAML.
func formatDurationShort(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
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

// wrapComment soft-wraps s at width characters and joins continuation lines
// with linePrefix. The first line has no prefix (the caller already wrote
// the leading "# " itself).
func wrapComment(s string, width int, linePrefix string) string {
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
	return strings.Join(lines, "\n"+linePrefix)
}
