// Package reviewer provides a multi-backend wrapper for code review using
// agent CLIs (Codex, Cursor).
package reviewer

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude/render"
	"github.com/bazelment/yoloswe/agent-cli-wrapper/codex"
)

// BackendType identifies which agent backend to use.
type BackendType string

const (
	BackendCodex  BackendType = "codex"
	BackendCursor BackendType = "cursor"
)

// Config holds reviewer configuration.
//
// # Sandbox challenges (affects both Codex and Cursor)
//
// Both Codex and Cursor use bubblewrap (bwrap) for sandboxing. On
// Ubuntu 24.04+ (kernel ≥ 6.5) with AppArmor, the sysctl
// kernel.apparmor_restrict_unprivileged_userns=1 is enabled by default.
// When an unprivileged process creates a user namespace, AppArmor
// transitions it into the "unprivileged_userns" profile which denies all
// capabilities—including CAP_NET_ADMIN that bwrap needs for loopback
// network setup. This breaks sandboxing for both backends:
//
//   - Codex: "read-only" and "workspace-write" sandbox modes fail with
//     "bwrap: loopback: Failed RTM_NEWADDR: Operation not permitted".
//     Only "danger-full-access" (which bypasses bwrap) works.
//   - Cursor: --sandbox causes the session to end immediately without
//     producing any result, regardless of whether --force is also set.
//
// Until the host is reconfigured (e.g. adding a per-binary AppArmor
// profile with "userns," or setting the sysctl to 0), sandboxing is
// unavailable for both backends.
//
// # Codex sandbox modes
//
// Codex offers three sandbox modes:
//
//   - "read-only"        – strictest; no file writes, shell runs inside bwrap
//   - "workspace-write"  – writes allowed only under the workspace root
//   - "danger-full-access" – no bwrap; tools run directly on the host
//
// # Codex approval policy and --read-only mitigation
//
// To mitigate the lack of bwrap, the --read-only flag provides a
// software-level guard: it sets the approval policy to "on-failure" and
// wires a ReadOnlyHandler that auto-approves Bash tool calls but denies
// Write tool calls. This prevents file writes through the Codex Write
// tool but cannot block destructive shell commands (rm, git reset, etc.)—
// the review prompt's instructions are the remaining constraint there.
//
// Without --read-only, the default is ApprovalPolicyNever (auto-approve
// everything) which avoids hanging on approval requests in non-interactive
// automation. Any approval policy other than "never" requires a wired
// ApprovalHandler; without one the codex process blocks indefinitely
// waiting for approval responses.
type Config struct {
	Model          string
	WorkDir        string
	Goal           string
	SessionLogPath string
	Effort         string               // Reasoning effort level for codex (low, medium, high)
	Sandbox        string               // Codex sandbox: "read-only", "workspace-write", "danger-full-access"
	ApprovalPolicy codex.ApprovalPolicy // Codex approval policy; see doc above for constraints
	BackendType    BackendType
	ReadOnly       bool // Deny file writes via approval handler (Codex only; CLI entrypoints default this to true)
	Verbose        bool
	NoColor        bool
	JSONOutput     bool
	// SkipTestExecution instructs the reviewer not to run test/build commands
	// (bazel, go test, etc.). Callers that already run tests in a separate step
	// (e.g. /pr-polish quality gates) should enable this to avoid duplicate work.
	SkipTestExecution bool
}

// buildGoalText formats the goal text for review prompts.
func buildGoalText(goal string) string {
	if goal == "" {
		return "Review all changes on this branch. Use commit messages to understand their purpose."
	}
	return "Review all changes on this branch. The main goal of the change on this branch is: " + goal
}

// skipTestExecutionSuffix is appended to the base prompt when the caller has
// opted into SkipTestExecution. The reviewer is still free to read test files;
// only process-spawning test/build commands are discouraged.
const skipTestExecutionSuffix = `

Do NOT run tests or build commands. The caller runs tests separately. Read test files to assess coverage, but do not invoke bazel, go test, go build, npm test, pytest, etc.`

// buildBasePrompt creates the common review prompt content.
func buildBasePrompt(goal string, skipTestExecution bool) string {
	base := fmt.Sprintf(`You are experienced software engineer, with bias toward code quality and correctness.
%s

Focus on these areas:
- Is the implementation correct? Is there any gap that should be addressed.
- Does it provide sufficient test coverage about the code path it touched.
- maintainability. also look at code around it, is there any code duplication that can be avoided.
- developer experience.
- performance.
- security.

When you flag an issue, provide a short, direct explanation and cite the affected file and line range.
Prioritize severe issues and avoid nit-level comments unless they block understanding of the diff.
Ensure that file citations and line numbers are exactly correct using the tools available; if they are incorrect your comments will be rejected.`, buildGoalText(goal))
	if skipTestExecution {
		base += skipTestExecutionSuffix
	}
	return base
}

// BuildPrompt creates the review prompt with free-form text output.
func BuildPrompt(goal string) string {
	return BuildPromptWithOptions(goal, false)
}

// BuildPromptWithOptions is BuildPrompt with the skip-test-execution toggle.
func BuildPromptWithOptions(goal string, skipTestExecution bool) string {
	return buildBasePrompt(goal, skipTestExecution) + `

After listing findings, produce an overall correctness verdict ("patch is correct" or "patch is incorrect") with a concise justification and a confidence score between 0 and 1.`
}

// BuildJSONPrompt creates a review prompt that requests JSON output format.
func BuildJSONPrompt(goal string) string {
	return BuildJSONPromptWithOptions(goal, false)
}

// BuildJSONPromptWithOptions is BuildJSONPrompt with the skip-test-execution toggle.
func BuildJSONPromptWithOptions(goal string, skipTestExecution bool) string {
	return buildBasePrompt(goal, skipTestExecution) + `

## Output Format
You MUST respond with valid JSON in this exact format:
{
  "verdict": "accepted" or "rejected",
  "summary": "Brief overall assessment of the changes",
  "issues": [
    {
      "severity": "critical|high|medium|low",
      "file": "path/to/file.go",
      "line": 42,
      "message": "Description of the issue",
      "suggestion": "How to fix it"
    }
  ]
}

## Severity Levels
- critical: Bugs, security vulnerabilities, broken functionality, data loss risks
- high: Missing error handling, incorrect logic that could cause failures
- medium: Code style issues, minor inefficiencies, missing edge cases
- low: Naming preferences, formatting, optional improvements

## Rules
- verdict MUST be exactly "accepted" or "rejected"
- If there are any critical or high severity issues, verdict MUST be "rejected"
- issues array can be empty if verdict is "accepted"
- Output ONLY the JSON object, no other text`
}

// ReviewResult contains the result of a review turn.
type ReviewResult struct {
	ResponseText string // Full response text
	ErrorMessage string // Error from the agent backend (empty on success)
	Success      bool
	DurationMs   int64
	InputTokens  int64
	OutputTokens int64
}

// Reviewer wraps an agent backend for code review operations.
type Reviewer struct {
	output         io.Writer
	backend        Backend
	renderer       *render.Renderer
	lastSessionID  string
	effectiveModel string // updated from backend session info when available
	config         Config
}

// New creates a new Reviewer with the given config.
func New(config Config) *Reviewer {
	if config.BackendType == "" {
		config.BackendType = BackendCodex
	}
	// Apply codex-specific defaults only for codex backend.
	// See Config doc for why danger-full-access is the default sandbox.
	if config.BackendType == BackendCodex {
		if config.Model == "" {
			config.Model = "gpt-5.2-codex"
		}
		if config.ApprovalPolicy == "" {
			if config.ReadOnly {
				// on-failure triggers the approval handler so
				// ReadOnlyHandler can deny Write tool calls.
				config.ApprovalPolicy = codex.ApprovalPolicyOnFailure
			} else {
				// never = auto-approve; avoids hanging without a handler.
				config.ApprovalPolicy = codex.ApprovalPolicyNever
			}
		}
		if config.Sandbox == "" {
			// bwrap-based modes (read-only, workspace-write) fail on
			// systems with AppArmor unprivileged userns restrictions.
			config.Sandbox = "danger-full-access"
		}
	}

	var backend Backend
	switch config.BackendType {
	case BackendCursor:
		backend = newCursorBackend(config)
	default:
		backend = newCodexBackend(config)
	}

	return &Reviewer{
		config:         config,
		output:         os.Stderr,
		renderer:       render.NewRendererWithOptions(os.Stderr, config.Verbose, config.NoColor),
		backend:        backend,
		effectiveModel: config.Model,
	}
}

// SetOutput sets the output writer for streaming responses.
// This also recreates the renderer to use the new writer.
func (r *Reviewer) SetOutput(w io.Writer) {
	r.output = w
	r.renderer = render.NewRendererWithOptions(w, r.config.Verbose, r.config.NoColor)
}

// Start initializes the backend.
func (r *Reviewer) Start(ctx context.Context) error {
	return r.backend.Start(ctx)
}

// Stop shuts down the backend.
func (r *Reviewer) Stop() error {
	return r.backend.Stop()
}

// Review sends a review prompt and streams the response to output.
func (r *Reviewer) Review(ctx context.Context, prompt string) error {
	_, err := r.ReviewWithResult(ctx, prompt)
	return err
}

// ReviewWithResult sends a review prompt and returns the result with response text.
func (r *Reviewer) ReviewWithResult(ctx context.Context, prompt string) (*ReviewResult, error) {
	model := r.config.Model
	if model == "" {
		model = "default"
	}
	status := fmt.Sprintf("Running review with %s (model: %s", r.config.BackendType, model)
	if r.config.Effort != "" {
		status += fmt.Sprintf(", effort: %s", r.config.Effort)
	}
	status += ")..."
	r.renderer.Status(status)
	handler := r.newEventHandler()
	result, err := r.backend.RunPrompt(ctx, prompt, handler)
	if err != nil {
		return nil, err
	}
	r.renderer.TurnCompleteWithTokens(result.Success, result.DurationMs, result.InputTokens, result.OutputTokens)
	return result, nil
}

// FollowUp sends a follow-up message to the existing backend session.
func (r *Reviewer) FollowUp(ctx context.Context, prompt string) (*ReviewResult, error) {
	handler := r.newEventHandler()
	result, err := r.backend.RunPrompt(ctx, prompt, handler)
	if err != nil {
		return nil, err
	}
	r.renderer.TurnCompleteWithTokens(result.Success, result.DurationMs, result.InputTokens, result.OutputTokens)
	return result, nil
}

// EffectiveModel returns the model actually used by the backend. For Codex
// this is the post-default config value; for Cursor this is updated from the
// backend's ReadyEvent once the session starts (the CLI picks its own default
// when --model is empty). Callers should prefer this over the raw --model
// flag, which may be empty or differ from what the backend actually ran.
func (r *Reviewer) EffectiveModel() string { return r.effectiveModel }

// LastSessionID returns the session/thread ID from the most recent backend
// session, or the empty string if no OnSessionInfo event has been observed.
func (r *Reviewer) LastSessionID() string { return r.lastSessionID }

// ValidateBackend returns an error if the given backend string is not supported.
func ValidateBackend(backend string) error {
	switch BackendType(backend) {
	case BackendCursor, BackendCodex:
		return nil
	default:
		return fmt.Errorf("unknown backend %q (supported: cursor, codex)", backend)
	}
}

// ResolveWorkDir returns the working directory from the WORK_DIR env var,
// falling back to os.Getwd().
func ResolveWorkDir() (string, error) {
	if dir := os.Getenv("WORK_DIR"); dir != "" {
		return dir, nil
	}
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("failed to determine working directory: %w", err)
	}
	return dir, nil
}

// ResolveProtocolLogPath resolves the session log path from a flag value and
// the BRAMBLE_PROTOCOL_LOG_DIR env var fallback. It creates the directory if
// needed and returns a unique filename including a timestamp to avoid
// collisions between concurrent runs. Returns "" if no log dir is configured.
//
// Note: protocol session logging is currently only supported by the Codex
// backend; the Cursor backend silently ignores SessionLogPath.
func ResolveProtocolLogPath(flagValue string) (string, error) {
	dir := flagValue
	if dir == "" {
		dir = os.Getenv("BRAMBLE_PROTOCOL_LOG_DIR")
	}
	if dir == "" {
		return "", nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("failed to create protocol log dir: %w", err)
	}
	filename := fmt.Sprintf("reviewer-session-%s.jsonl", time.Now().Format("20060102-150405"))
	return filepath.Join(dir, filename), nil
}

// PrintResultSummary writes the review metadata to stderr and prints the full
// response text to stdout.
//
// Output contract: the streaming render path already prints the response on
// stderr during the turn. Stdout always receives the final response text
// exactly once so pipeline consumers have a stable sink; interactive users
// will see the text twice (streaming on stderr, final on stdout), which is
// a reasonable cost for a predictable machine-readable contract.
func PrintResultSummary(result *ReviewResult) {
	fmt.Fprintf(os.Stderr, "\n=== Review Result ===\n")
	fmt.Fprintf(os.Stderr, "Success: %v\n", result.Success)
	if result.ErrorMessage != "" {
		fmt.Fprintf(os.Stderr, "Error: %s\n", result.ErrorMessage)
	}
	fmt.Fprintf(os.Stderr, "Duration: %dms\n", result.DurationMs)
	fmt.Fprintf(os.Stderr, "Response length: %d chars\n", len(result.ResponseText))
	fmt.Println(result.ResponseText)
}
