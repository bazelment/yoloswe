// Package reviewer provides a multi-backend wrapper for code review using
// agent CLIs (Codex, Cursor, Gemini).
package reviewer

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude/render"
	"github.com/bazelment/yoloswe/agent-cli-wrapper/codex"
)

// BackendType identifies which agent backend to use.
type BackendType string

const (
	BackendCodex  BackendType = "codex"
	BackendCursor BackendType = "cursor"
	BackendGemini BackendType = "gemini"

	// DefaultGeminiModel is the model used when BackendGemini is selected and
	// no --model flag is provided.
	DefaultGeminiModel = "gemini-3.1-flash-lite-preview"

	// DefaultCodexModel is the model used when BackendCodex is selected and
	// no --model flag is provided.
	DefaultCodexModel = "gpt-5.4-mini"

	// DefaultCursorModel is the model used when BackendCursor is selected and
	// no --model flag is provided.
	DefaultCursorModel = "composer-2"
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

// PromptOptions configures the optional clauses appended to a review prompt.
//
// Each clause is gated by data presence rather than a separate boolean: an
// empty TestScopeHints means "no test-quality clause", and fewer than two
// CrossServicePackages means "no cross-service clause". This collapses the
// (boolean, list) cartesian product into a single source of truth — the
// caller controls everything by what they put in the lists.
type PromptOptions struct {
	// TestScopeHints lists co-located test paths the agent should read. When
	// non-empty, the test-quality clause is appended to the prompt and the
	// paths are inlined under it (capped at testScopeHintsCap entries).
	TestScopeHints []string

	// CrossServicePackages names all top-level packages this PR touches.
	// When it has at least two entries and ChangedPackages is empty, the
	// generic flat-list cross-service contract-sweep clause is appended.
	CrossServicePackages []string

	// ChangedPackages names the top-level packages directly modified by
	// this diff. When non-empty, the cross-service clause uses explicit
	// caller/callee framing — naming the changed packages and the
	// callers/dependencies separately — instead of the flat list.
	ChangedPackages []string

	// DependencyPackages names packages that import or are imported by the
	// changed packages. Inlined as the "callers or dependencies" side in
	// the caller/callee framing.
	DependencyPackages []string

	// SkipTestExecution discourages the agent from spawning bazel/go test/etc.
	SkipTestExecution bool
}

// testScopeHintsCap bounds the number of test paths inlined into the prompt
// so token spend stays predictable on very large multi-package PRs.
const testScopeHintsCap = 50

// crossServicePackagesCap bounds the number of cross-service packages
// inlined into the prompt. The realistic shape is small (a typical
// monorepo has under a dozen top-level service buckets) but the upstream
// LoadScopeHints accepts files up to 1 MiB, so a hostile or buggy
// producer could pack thousands of short package strings and inflate
// tokens without ever hitting that cap. Symmetrical with
// testScopeHintsCap.
const crossServicePackagesCap = 50

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

// SanitizePromptHint returns true when s is safe to inline verbatim into a
// prompt clause. It rejects entries that would distort the surrounding
// Markdown structure or look like injected instructions:
//
//   - Bullet-list / blockquote / heading / setext block-level prefixes at
//     the very start: # - * + > =.
//   - Ordered-list markers at the very start: a digit run followed by
//     "." or ")" (e.g. 1. or 42)).
//   - Newlines or carriage returns anywhere in the entry.
//   - Leading/trailing whitespace, which renders awkwardly in joined lines.
//   - Empty strings (would produce blank lines in the prompt).
//
// We deliberately do NOT reject leading underscore. TS/JS conventions
// produce __tests__/foo.test.ts and Python writes _helper.py — both
// legitimate scope-hint inputs the producer (scope_gate.py) emits
// without prompting. Underscore at the start of a line in Markdown only
// renders as emphasis when paired with a matching closing underscore on
// the same line; a leading-underscore path inlined as one of many
// path-per-line entries is safe.
//
// LoadScopeHints calls this at the file-load boundary so a producer bug
// fails loudly with a CLI warning. The prompt builders also filter via
// this function as defense-in-depth: any direct caller of
// BuildJSONPromptWithScope (an exported entry point) gets the same
// guarantee even if they bypass LoadScopeHints. The realistic threat
// here is a buggy producer, not a malicious one — scope_gate.py walks
// the filesystem of the worktree under review — but bramble owns the
// prompt structure, so it shouldn't trust callers to have validated.
func SanitizePromptHint(s string) bool {
	if s == "" {
		return false
	}
	if strings.ContainsAny(s, "\r\n") {
		return false
	}
	if s != strings.TrimSpace(s) {
		return false
	}
	// Block-level Markdown control prefixes at the very first byte. We
	// don't strip them — that would silently rewrite producer output —
	// we just skip the entry. Realistic filesystem paths don't start
	// with these.
	switch s[0] {
	case '#', '-', '*', '+', '>', '=':
		return false
	}
	// Ordered-list markers: ``1.`` / ``12)`` / etc. CommonMark accepts
	// up to nine digits followed by ``.`` or ``)``. We're conservative
	// and reject any leading digit run terminated by either character.
	if s[0] >= '0' && s[0] <= '9' {
		i := 0
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		if i < len(s) && (s[i] == '.' || s[i] == ')') {
			return false
		}
	}
	return true
}

// filterPromptHints drops entries that fail SanitizePromptHint. The
// contract is "best-effort safe inlining": callers that produced clean
// data get all their entries, and a buggy/hostile entry is silently
// elided rather than corrupting the surrounding Markdown structure.
// LoadScopeHints already errors on the same shapes earlier in the
// pipeline, so this filter usually has nothing to do — its purpose is
// to harden the prompt-builder boundary itself.
func filterPromptHints(items []string) []string {
	out := make([]string, 0, len(items))
	for _, s := range items {
		if SanitizePromptHint(s) {
			out = append(out, s)
		}
	}
	return out
}

// capAndJoin caps items at max entries and returns a comma-separated string
// with " (and N more)" appended when items were dropped. Returns "" when
// items is empty. Caller is responsible for any sanitization.
func capAndJoin(items []string, max int) string {
	if len(items) == 0 {
		return ""
	}
	suffix := ""
	if len(items) > max {
		extra := len(items) - max
		items = items[:max]
		suffix = fmt.Sprintf(" (and %d more)", extra)
	}
	return strings.Join(items, ", ") + suffix
}

// testQualityClause returns the test-quality scrutiny clause with the given
// co-located test paths inlined.
//
// Deliberately stated as a principle, not an enumerated checklist. An earlier
// draft listed six concrete anti-patterns derived directly from the kernel
// evidence corpus (tautological asserts, broad Exception catches, missing
// kwargs, etc.); see plans/issue-175-widen-review-scope.md and the
// conversation around #179. The risk of overfitting to those specific shapes
// — anchoring the reviewer on a small list and crowding out the long tail of
// real test-quality issues — outweighed the recall benefit on the eval set.
// Tunings here should add principles, not specific bug examples; let
// measurement (Phase 3, #178) drive any expansion.
//
// The caller must guarantee len(paths) > 0 — buildScopeSuffix is the only
// caller and gates on that.
func testQualityClause(paths []string) string {
	paths = filterPromptHints(paths)
	if len(paths) == 0 {
		// All entries were filtered out by sanitization. The caller's
		// gating (len > 0) was based on raw input; if everything got
		// dropped here, emit no clause rather than an empty bullet list.
		return ""
	}
	displayed := paths
	suffix := ""
	if len(displayed) > testScopeHintsCap {
		extra := len(displayed) - testScopeHintsCap
		displayed = displayed[:testScopeHintsCap]
		suffix = fmt.Sprintf("\n(... and %d more — read tests/ directories under the changed package roots)", extra)
	}
	return `

## Test quality
Read the co-located test files listed below alongside the diff. For each,
assess whether the tests would actually catch a regression of the change
under review — not just whether they pass. Flag tests that exercise mocks
or harness side-effects rather than the behavior under review, and tests
whose assertions would still hold if the new behavior were silently removed.

Continue to avoid nit-level comments unless they block understanding of
the diff or weaken a stated regression signal.

Co-located test files to read (in addition to anything in the diff):
` + strings.Join(displayed, "\n") + suffix
}

// crossServiceContractItems is the trailing checklist shared by both
// framings of the cross-service contract-sweep clause.
//
// The four numbered items track the issue body's draft (#175). An earlier
// version added a fifth item naming specific shapes (FastAPI path-parameter
// ordering, ORM mixins, OpenAPI tag collisions) drawn from kernel-2998; it
// was removed for the same anti-overfitting reason as testQualityClause.
// New items should describe failure modes, not specific framework bugs.
const crossServiceContractItems = `
1. Signature or shape changes that don't match consumer expectations
   (request/response field names, types, optionality, enum values).
2. Async state updates that desync between producer and consumer
   (optimistic UI updates that diverge from refetched server state,
   stale-while-revalidate paths returning prior values).
3. Error or loading paths whose handling differs across packages
   (one side throws, the other silently falls back; one side surfaces a
   typed error, the other treats it as success).
4. Silent fallbacks that swallow values from another service (default
   values masking missing fields, empty arrays masking failed lookups).

When citing an issue, name both sides (file:line) and explain the desync
explicitly. If both sides agree, do not flag the surface.`

// crossServiceClauseGeneric is a flat list of touched packages with no
// caller/callee distinction, used when only CrossServicePackages is set.
// The caller must guarantee len(packages) >= 2.
func crossServiceClauseGeneric(packages []string) string {
	// Sanitization may have filtered the list below the >=2 threshold the
	// caller's gate assumed. Drop the clause rather than emit one with a
	// single (or zero) package — the prompt would read nonsensically.
	filtered := filterPromptHints(packages)
	if len(filtered) < 2 {
		return ""
	}
	return `

## Cross-service contract sweep
This PR touches multiple top-level packages: ` + capAndJoin(filtered, crossServicePackagesCap) + `.
Trace every public API/handler/exported symbol modified in one package to its
consumers in the others. Read both sides of each surface and flag:` + crossServiceContractItems
}

// crossServiceClauseCallerCallee names which packages were changed and which
// are the callers/dependencies to check. Used when ChangedPackages is set.
func crossServiceClauseCallerCallee(changedPkgs, depPkgs []string) string {
	changed := filterPromptHints(changedPkgs)
	if len(changed) == 0 {
		return ""
	}
	depSection := ""
	if deps := filterPromptHints(depPkgs); len(deps) > 0 {
		depSection = `
The following packages are callers or dependencies — check whether the interface
contract (HTTP request shape, event schema, error codes, async message format)
changed on the modified side without a matching update on these sides: ` + capAndJoin(deps, crossServicePackagesCap) + `.`
	}
	return `

## Cross-service contract sweep
The diff primarily modifies: ` + capAndJoin(changed, crossServicePackagesCap) + `.` + depSection + `
Trace every public API/handler/exported symbol modified in the changed packages
to its consumers. Read both sides of each surface and flag:` + crossServiceContractItems
}

// buildScopeSuffix concatenates the optional clauses dictated by opts. Each
// clause is gated purely by data presence so callers without scope info pay
// no cost — the returned string is empty when neither clause applies, which
// keeps legacy callers byte-equal to today's prompt.
func buildScopeSuffix(opts PromptOptions) string {
	var s string
	if len(opts.TestScopeHints) > 0 {
		s += testQualityClause(opts.TestScopeHints)
	}
	if len(opts.ChangedPackages) > 0 {
		s += crossServiceClauseCallerCallee(opts.ChangedPackages, opts.DependencyPackages)
	} else if len(opts.CrossServicePackages) >= 2 {
		s += crossServiceClauseGeneric(opts.CrossServicePackages)
	}
	return s
}

// BuildPrompt creates the review prompt with free-form text output.
func BuildPrompt(goal string) string {
	return BuildPromptWithOptions(goal, false)
}

// BuildPromptWithOptions is BuildPrompt with the skip-test-execution toggle.
// Kept as a shim around BuildPromptWithScope so existing callers compile
// unchanged; passes empty TestScopeHints/CrossServicePackages so the legacy
// output is byte-equal to today's.
func BuildPromptWithOptions(goal string, skipTestExecution bool) string {
	return BuildPromptWithScope(goal, PromptOptions{SkipTestExecution: skipTestExecution})
}

// BuildPromptWithScope creates the free-form review prompt with scope clauses
// gated by opts. Empty PromptOptions{} produces today's legacy prompt.
func BuildPromptWithScope(goal string, opts PromptOptions) string {
	return buildBasePrompt(goal, opts.SkipTestExecution) + buildScopeSuffix(opts) + `

After listing findings, produce an overall correctness verdict ("patch is correct" or "patch is incorrect") with a concise justification and a confidence score between 0 and 1.`
}

// BuildJSONPrompt creates a review prompt that requests JSON output format.
func BuildJSONPrompt(goal string) string {
	return BuildJSONPromptWithOptions(goal, false)
}

// BuildJSONPromptWithOptions is BuildJSONPrompt with the skip-test-execution
// toggle. Kept as a shim around BuildJSONPromptWithScope.
func BuildJSONPromptWithOptions(goal string, skipTestExecution bool) string {
	return BuildJSONPromptWithScope(goal, PromptOptions{SkipTestExecution: skipTestExecution})
}

// BuildJSONPromptWithScope creates the JSON-output review prompt with scope
// clauses gated by opts. Empty PromptOptions{} produces today's legacy
// prompt; the scope clauses (test-quality, cross-service) are inserted
// between the base prompt and the JSON output rules.
func BuildJSONPromptWithScope(goal string, opts PromptOptions) string {
	return buildBasePrompt(goal, opts.SkipTestExecution) + buildScopeSuffix(opts) + `

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
      "suggestion": "How to fix it",
      "confidence": 0.9
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
- Each issue MUST include severity, file, line (>= 1), and message; suggestion is optional
- confidence is a float in (0.0, 1.0]: reviewer's confidence in this finding (1.0 = certain, 0.5 = plausible but unverified); omit when certain or when you can't assess
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
	// Apply Gemini-specific defaults.
	if config.BackendType == BackendGemini {
		if config.Model == "" {
			config.Model = DefaultGeminiModel
		}
	}

	// Apply cursor-specific defaults.
	if config.BackendType == BackendCursor {
		if config.Model == "" {
			config.Model = DefaultCursorModel
		}
	}

	// Apply codex-specific defaults only for codex backend.
	// See Config doc for why danger-full-access is the default sandbox.
	if config.BackendType == BackendCodex {
		if config.Model == "" {
			config.Model = DefaultCodexModel
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
	case BackendGemini:
		backend = newGeminiBackend(config)
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
		if result != nil {
			r.renderer.TurnCompleteWithTokens(result.Success, result.DurationMs, result.InputTokens, result.OutputTokens)
		}
		return result, err
	}
	r.renderer.TurnCompleteWithTokens(result.Success, result.DurationMs, result.InputTokens, result.OutputTokens)
	return result, nil
}

// FollowUp sends a follow-up message to the existing backend session.
func (r *Reviewer) FollowUp(ctx context.Context, prompt string) (*ReviewResult, error) {
	handler := r.newEventHandler()
	result, err := r.backend.RunPrompt(ctx, prompt, handler)
	if err != nil {
		if result != nil {
			r.renderer.TurnCompleteWithTokens(result.Success, result.DurationMs, result.InputTokens, result.OutputTokens)
		}
		return result, err
	}
	r.renderer.TurnCompleteWithTokens(result.Success, result.DurationMs, result.InputTokens, result.OutputTokens)
	return result, nil
}

// EffectiveModel returns the model actually used by the backend. Defaults for
// all backends (Codex, Cursor, Gemini) are applied in New, so the value is
// set before the session starts. For Cursor, it may be replaced by the model
// reported in the backend's ReadyEvent (OnSessionInfo). Callers should prefer
// this over the raw --model flag, which may be empty or differ from what the
// backend actually ran.
func (r *Reviewer) EffectiveModel() string { return r.effectiveModel }

// LastSessionID returns the session/thread ID from the most recent backend
// session, or the empty string if no OnSessionInfo event has been observed.
func (r *Reviewer) LastSessionID() string { return r.lastSessionID }

// ValidateBackend returns an error if the given backend string is not supported.
func ValidateBackend(backend string) error {
	switch BackendType(backend) {
	case BackendCursor, BackendCodex, BackendGemini:
		return nil
	default:
		return fmt.Errorf("unknown backend %q (supported: cursor, codex, gemini)", backend)
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
// backend; Cursor and Gemini backends silently ignore SessionLogPath.
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
