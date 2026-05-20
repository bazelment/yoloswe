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

// ReviewMode selects which review persona, focus areas, and output schema the
// reviewer prompt builders should produce. It exists so the same code-review
// command can drive multiple kinds of review (today: code diffs and design
// docs; tomorrow potentially security audits, API-contract reviews, etc.)
// without conflating persona/rubric (which the model reads) with the output
// schema (which the orchestrator's triage layer keys on).
//
// New modes should be added as new constants here, paired with a new branch
// in buildBasePrompt and jsonOutputRules and a matching schema in
// validateReviewBody. Keeping the enumeration in Go (vs. a YAML registry)
// preserves compile-time checking — the JSON validator and the prompt
// builder must move in lock-step or the envelope contract breaks.
type ReviewMode string

const (
	// ReviewModeCode is the legacy mode: code-review persona, file/line
	// citations, accepted/rejected verdict. The empty string is treated as
	// ReviewModeCode for backward compatibility with existing callers.
	ReviewModeCode ReviewMode = "code"
	// ReviewModeDesignDoc grills a markdown design document against a
	// caller-supplied rubric, cites section headings (not file/line), and
	// emits ready/revise/rethink verdicts with a confidence
	// score.
	ReviewModeDesignDoc ReviewMode = "design-doc"
)

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
	ApprovalPolicy  codex.ApprovalPolicy // Codex approval policy; see doc above for constraints
	WorkDir         string
	Goal            string
	SessionLogPath  string
	Effort          string // Reasoning effort level for codex (low, medium, high)
	Sandbox         string // Codex sandbox: "read-only", "workspace-write", "danger-full-access"
	Model           string
	BackendType     BackendType
	ResumeSessionID string // Prior reviewer session/thread id to resume when supported.
	ReadOnly        bool   // Deny file writes via approval handler (Codex only; CLI entrypoints default this to true)
	Verbose         bool
	NoColor         bool
	// SkipTestExecution instructs the reviewer not to run test/build commands
	// (bazel, go test, etc.). Callers that already run tests in a separate step
	// (e.g. /pr-polish quality gates) should enable this to avoid duplicate work.
	SkipTestExecution bool
}

// ResumeStatus records whether a requested backend resume succeeded.
//
// The three values form a finalization order: a backend that was asked to
// resume starts at Unverified; resumeStatusAfterSessionReady promotes that
// to OK once a Ready event confirms the backend is on the requested session,
// or to Fallback when the backend ran fresh because the requested session
// was unavailable. A run that exits before any Ready event therefore
// surfaces Unverified — distinguishable from a non-resume run, where the
// status stays empty and is dropped by omitempty in the envelope.
type ResumeStatus string

const (
	ResumeStatusOK         ResumeStatus = "ok"
	ResumeStatusFallback   ResumeStatus = "fallback"
	ResumeStatusUnverified ResumeStatus = "unverified"
)

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
// Each clause is gated by data presence rather than a separate boolean. The
// test-quality clause is appended when TestScopeHints is non-empty. The
// cross-service clause has two framings, picked in this order:
//
//   - When ChangedPackages is non-empty, the caller/callee framing names the
//     changed packages explicitly and uses DependencyPackages (when set) to
//     name the other side; a single changed package is fine.
//   - Otherwise, when CrossServicePackages has at least two entries, the
//     generic flat-list framing is used (v1 compat — kept so old scope-hints
//     producers still get the clause without ChangedPackages).
//
// This collapses the (boolean, list) cartesian product into a single source
// of truth — the caller controls everything by what they put in the lists.
type PromptOptions struct {
	// Mode picks the review persona and output schema. The empty string
	// behaves as ReviewModeCode to keep legacy callers byte-equal.
	// ReviewModeDesignDoc requires Rubric to be non-empty; the prompt
	// builders return an error-shaped placeholder when that contract is
	// violated rather than silently emitting a doc-review with no rubric.
	Mode ReviewMode

	// Rubric carries the grilling questions inlined into the design-doc
	// prompt. One entry per question. Ignored unless Mode is
	// ReviewModeDesignDoc. Validated and capped by the caller (typically
	// loadPromptOptions in cmd/codereview); each entry is fed through
	// SanitizePromptHint to keep markdown structure intact.
	Rubric []string

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

// effectiveMode returns Mode with the empty-string-defaults-to-code
// behaviour applied. Centralised so the rest of the reviewer package never
// has to repeat the conditional.
func (o PromptOptions) effectiveMode() ReviewMode {
	if o.Mode == "" {
		return ReviewModeCode
	}
	return o.Mode
}

// rubricCap bounds the number of rubric questions inlined into the
// design-doc prompt. Callers that load rubrics from a file should enforce a
// matching cap before constructing PromptOptions; this cap is the
// defence-in-depth at the prompt-builder boundary, mirroring
// testScopeHintsCap. Twenty questions is more than any real grilling rubric
// needs and well below any prompt-token concern.
const rubricCap = 20

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

// buildBasePrompt dispatches on review mode to produce the persona +
// focus-areas portion of the prompt. ReviewModeCode is the default and
// produces today's prompt byte-for-byte; ReviewModeDesignDoc swaps to a
// staff-engineer-reviewing-a-doc persona and inlines the caller-supplied
// rubric.
func buildBasePrompt(goal string, opts PromptOptions) string {
	switch opts.effectiveMode() {
	case ReviewModeDesignDoc:
		return buildDesignDocBasePrompt(goal, opts)
	default:
		return buildCodeBasePrompt(goal, opts.SkipTestExecution)
	}
}

// buildCodeBasePrompt is the legacy code-review persona + focus list. The
// body must stay byte-equivalent to the prior buildBasePrompt output so
// existing snapshot tests and prompt-shape callers see no change.
func buildCodeBasePrompt(goal string, skipTestExecution bool) string {
	base := fmt.Sprintf(`You are experienced software engineer, with bias toward code quality and correctness.
%s

Focus on these areas:
- Is the implementation correct? Is there any gap that should be addressed.
- Does it provide sufficient test coverage about the code path it touched.
- maintainability. also look at code around it, is there any code duplication that can be avoided.
- developer experience.
- performance.
- security.

When you find N >= 2 sibling sites of the same underlying rule violation (same invariant, different lines), emit ONE issue with a named "invariant" and a "sites" array. Do NOT emit N separate single-site issues. The invariant name describes the rule being violated (e.g. "ambient env vars shadow explicit proxy keys"), not the symptom. A single finding that names the producer-side invariant beats N findings that patch consumer sites — see the Class-level findings section of the output format for the exact shape.

Prioritize systemic problems over local ones. If after scanning the diff and adjacent code you find no structural issues, return an empty issues array with verdict "accepted". Finding nothing on a clean diff is the right call; do not strain to find something to flag.

When you flag an issue, provide a short, direct explanation and cite the affected file and line range.
Prioritize severe issues and avoid nit-level comments unless they block understanding of the diff.
Ensure that file citations and line numbers are exactly correct using the tools available; if they are incorrect your comments will be rejected.`, buildGoalText(goal))
	if skipTestExecution {
		base += skipTestExecutionSuffix
	}
	return base
}

// buildDesignDocBasePrompt produces the design-doc persona + rubric. The
// rubric is inlined as a numbered list — same shape as the four-question
// starting rubric the user supplied — so the model can refer to questions
// by number (`dimension: q1`, `dimension: q2`) without the orchestrator
// pre-computing slugs.
//
// The persona is deliberately blunt: design docs ship better when the
// reviewer is asked to grill systemic issues rather than triage details.
// The "do not modify the document" line keeps the model in advice-only
// mode — the orchestrator (e.g. /design-doc-polish) owns mutations.
//
// SkipTestExecution and the scope-hint clauses are irrelevant for a
// single-doc review, so they are not consumed here. The caller layer in
// cmd/codereview warns-and-ignores both --scope-hints-file and
// --skip-test-execution in design-doc mode (validateModeFlags), so a
// stray flag won't error out — it just produces a warning in the run log
// while the prompt builder drops the clauses cleanly. Mirrors the
// general principle that a mode change shouldn't require unrelated flags
// to be dropped from existing automation invocations.
func buildDesignDocBasePrompt(goal string, opts PromptOptions) string {
	rubric := filterPromptHints(opts.Rubric)
	if len(rubric) > rubricCap {
		rubric = rubric[:rubricCap]
	}
	if len(rubric) == 0 {
		// Defence-in-depth: cmd/codereview is supposed to reject
		// design-doc mode without a rubric at flag-parse time, so the
		// only way we land here is a direct PromptOptions caller that
		// bypassed that gate. Falling through to an empty rubric would
		// produce a doc-grilling prompt with nothing to grill on.
		// Surface the misconfiguration as an error-shaped sentinel
		// inside the prompt so the reviewer's response (and the
		// validator) trips loudly instead of returning bland output.
		return fmt.Sprintf(`MISCONFIGURED: design-doc review mode requires a non-empty rubric. The orchestrator must pass a rubric file via --review-rubric-file. Goal supplied: %q. Refuse this turn.`, goal)
	}
	var rubricLines strings.Builder
	for i, q := range rubric {
		fmt.Fprintf(&rubricLines, "%d. %s\n", i+1, q)
	}
	return fmt.Sprintf(`You are a staff engineer reviewing a software design document. The author wants the doc grilled — your job is to surface systemic issues, not nits. Focus on the substance of the design, not on prose style or markdown formatting.

%s

Grill the document on the following questions. Each issue you raise must be tagged with the question it answers (e.g. "dimension": "q1") so the orchestrator can group findings by axis:

%s
When you flag an issue, cite the section heading the issue lives under (e.g. "section": "Milestone 2: Multi-tenant rollout"). Do not invent line numbers — the doc may be edited between rounds and section headings are the durable address. If the issue is doc-wide rather than section-specific, set "section" to "(whole document)".

Prioritize systemic problems over local ones. A finding that says "the milestone strategy doesn't frontload risk" is more useful than five findings flagging individual late-milestone risks. Bundle related observations into one finding tagged with the rubric question they jointly point at.

Do not modify the document. The orchestrator applies fixes between rounds — your job is feedback only.`, buildDesignDocGoalText(goal), strings.TrimRight(rubricLines.String(), "\n"))
}

// buildDesignDocGoalText is the design-doc analogue of buildGoalText. Empty
// goals are common in design-doc reviews because the rubric carries the
// review intent (the goal channel is the per-turn context — round 1 names
// the doc, round 2+ carries action history). Keep the no-goal default
// minimal so the model doesn't anchor on a stale "review the changes" line.
func buildDesignDocGoalText(goal string) string {
	if goal == "" {
		return "Review the design document below."
	}
	return goal
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
//
// The caller/callee branch tries to emit first (it's the more informative
// framing), but if every ChangedPackages entry was filtered out by
// SanitizePromptHint the clause comes back empty — in that case we fall
// back to the generic flat-list framing rather than dropping the entire
// cross-service section. A direct PromptOptions caller that bypassed
// LoadScopeHints could otherwise lose all cross-service guidance just
// because their ChangedPackages list happened to be unsanitary.
func buildScopeSuffix(opts PromptOptions) string {
	var s string
	if len(opts.TestScopeHints) > 0 {
		s += testQualityClause(opts.TestScopeHints)
	}
	if len(opts.ChangedPackages) > 0 {
		clause := crossServiceClauseCallerCallee(opts.ChangedPackages, opts.DependencyPackages)
		if clause == "" && len(opts.CrossServicePackages) >= 2 {
			clause = crossServiceClauseGeneric(opts.CrossServicePackages)
		}
		s += clause
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
//
// For ReviewModeDesignDoc the scope clauses (test-quality, cross-service)
// are skipped — they are diff-derived signals that don't apply to a
// single markdown file — and the verdict footer is replaced with the
// doc-flavoured ready/revise/rethink values.
func BuildPromptWithScope(goal string, opts PromptOptions) string {
	if opts.effectiveMode() == ReviewModeDesignDoc {
		return buildBasePrompt(goal, opts) + `

After listing findings, produce an overall verdict ("ready", "revise", or "rethink") with a concise justification and an overall confidence score in [0.0, 1.0]. This overall score is distinct from the optional per-issue confidence in the JSON output format; it summarizes confidence in the verdict itself.`
	}
	return buildBasePrompt(goal, opts) + buildScopeSuffix(opts) + `

After listing findings, produce an overall correctness verdict ("patch is correct" or "patch is incorrect") with a concise justification and an overall confidence score in [0.0, 1.0]. This overall score is distinct from the optional per-issue confidence in the JSON output format (which is in (0.0, 1.0]); it summarizes confidence in the verdict itself.`
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
//
// For ReviewModeDesignDoc the scope clauses are skipped (irrelevant for a
// single-file doc review) and jsonOutputRules emits the design-doc schema
// (section/dimension instead of file/line; ready/revise/rethink
// verdict; per-issue confidence carried up to the
// review-level confidence too).
func BuildJSONPromptWithScope(goal string, opts PromptOptions) string {
	if opts.effectiveMode() == ReviewModeDesignDoc {
		return buildBasePrompt(goal, opts) + jsonOutputRules(ReviewModeDesignDoc)
	}
	return buildBasePrompt(goal, opts) + buildScopeSuffix(opts) + jsonOutputRules(ReviewModeCode)
}

// BuildFollowUpJSONPromptWithScope creates the shorter resumed-session prompt
// used after a prior review round has already established context.
//
// Design intent: a follow-up prompt sits in tension between two competing
// goals.
//
//   - Dedup-aware: don't have the model re-list every prior finding verbatim;
//     the orchestrator (e.g. /pr-polish) has already triaged those.
//   - Bias-guard: don't let the narrowing bias the model into ratifying its
//     prior verdict by ignoring code it already accepted. A second pass that
//     finds something the first pass missed is more useful than one that just
//     re-confirms the prior conclusion.
//
// The earlier shape leaned hard on the dedup side ("focus on the changes made
// since that prior turn", with a strict 3-item focus list and an explicit
// "do not re-list" rule) and produced visibly biased output in the round-2
// eval — cursor turn 2 returned 0 issues with the summary "HEAD is unchanged
// since the earlier pass", which is exactly the failure mode we want to
// avoid. This rewrite explicitly invites a fresh look at the full diff,
// rewards finding new issues over confirming the prior verdict, and only
// uses the (1)/(2)/(3) framing as "pay particular attention to" hints
// rather than the sole acceptable scope.
//
// Token-cost cuts vs the prior shape: the goal re-statement, the persona, the
// per-issue citation/rubric instructions, and the full JSON output spec are
// dropped. They were established in the fresh prompt that started the session
// and are already in the model's context window; restating them adds noise
// without signal. A single-line "same severity rubric and JSON output format"
// pointer keeps format compliance anchored without re-pasting the spec.
//
// The skip-test-execution suffix and the scope suffix (test-quality +
// cross-service clauses, derived from opts) ARE conditionally appended,
// however — round-8 codex+cursor consensus flagged that on a silent resume
// fallback (resume_status="fallback") the model reads this prompt cold and
// missing those clauses gives it materially less guidance than a real fresh
// review. The few extra tokens on a successfully-resumed turn are noise vs
// the safety net for the fallback case. Skip-test/scope clauses fire only
// when opts.SkipTestExecution is set or the scope-hint lists are non-empty,
// so the empty-opts case still produces the minimal prompt.
//
// The opts argument is preserved on the signature for symmetry with the
// fresh prompt and so callers don't need a separate dispatch — but its
// goal serves two purposes on a follow-up turn, depending on whether it's
// empty:
//
//   - empty goal: the original PR-level goal was established in the fresh
//     prompt that started the session, and the resumed model already has it
//     in context. Don't re-state it — that's redundant and was the round-2
//     eval's bias-amplifier. The no-prior-context escape hatch below handles
//     the silent-fallback case via the model's first-pass review behavior.
//
//   - non-empty goal: the caller is using the goal channel for per-turn
//     metadata, not PR-level intent. The canonical example is /pr-polish
//     on rounds 2+: it passes the action history (what prior rounds fixed,
//     skipped, and why) so the resumed model knows which of its own prior
//     findings have already been addressed and doesn't waste a turn
//     re-flagging them. Embed the goal text as "Context for this turn: ..."
//     so the model treats it as orchestrator-supplied state, not as the
//     original PR goal restated.
//
// opts.SkipTestExecution and the scope-hint fields ARE conditionally
// rendered (see the design intent above) so a fallback session reads them
// cold.
func BuildFollowUpJSONPromptWithScope(goal string, opts PromptOptions) string {
	if opts.effectiveMode() == ReviewModeDesignDoc {
		return buildDesignDocFollowUpPrompt(goal, opts)
	}
	prompt := `Continue the review on the same diff against the same goal as the prior turn.`
	if goal != "" {
		prompt += "\n\nContext for this turn: " + goal
	}
	prompt += `

If you have no prior review context for this diff (because the backend silently fell back to a fresh session despite the resume request), treat this as a first-pass review: examine the entire diff and apply the standard severity rubric and JSON output format. Otherwise, proceed with the resume protocol below.

Re-review the full diff with fresh eyes. Pay particular attention to:
1. Issues introduced by new commits since the prior turn.
2. Items you flagged before that you now have stronger evidence for — cite the file:line that proves it.
3. Anything you skipped or dismissed before that, on a second look, warrants flagging.
4. New sites of an invariant you already named in a prior turn — fold them into the existing invariant's "sites" array via a single issue. Do NOT re-flag the same invariant as separate per-site findings; the orchestrator will treat that as a spiral and waste a round chasing your symptom list.

If the prior turn's fixes addressed the structural issues you raised and you find nothing new at this scope, return an empty issues array with verdict "accepted". A clean second pass is a legitimate outcome — better than re-surfacing nits to justify the turn. On resumed sessions you MAY emit a top-level "sufficiency" object signalling whether you believe the prior turn's fixes addressed every invariant you can find; see the output format spec for the shape.

Apply the same severity rubric and JSON output format as the prior turn.`
	// Append the scope suffix and skip-test-execution suffix here, even
	// though a successfully resumed session already has them in context.
	// On a silent resume fallback (resume_status="fallback"), the model
	// is reading this prompt for the first time — without these clauses
	// the no-prior-context escape hatch would tell it to "treat as a
	// first-pass review" while withholding the test-quality and
	// cross-service hints a real fresh review would have. Round-8
	// codex+cursor consensus flagged that contradiction. The few extra
	// tokens are noise compared to a fresh review missing structural
	// findings outside the immediately changed code.
	if opts.SkipTestExecution {
		prompt += skipTestExecutionSuffix
	}
	return prompt + buildScopeSuffix(opts)
}

// buildDesignDocFollowUpPrompt is the design-doc analogue of the code
// follow-up prompt. Two differences from the code path:
//
//   - No scope-suffix safety net (scope hints are diff-derived and don't
//     apply to a single doc), but the rubric IS the review for design-doc
//     mode, so we inline a compact recap in the follow-up prompt — the
//     same "survive a silent resume fallback" reasoning as the code-mode
//     scope-suffix appendage. Without this, a backend that silently
//     cold-starts despite --resume-session-id reads this prompt with no
//     idea what rubric questions the orchestrator wanted grilled.
//   - The "fresh eyes" framing keeps the same anti-bias intent but
//     swaps file/line cues for section/dimension cues so the resumed
//     model doesn't drift back to code-review citation shape.
func buildDesignDocFollowUpPrompt(goal string, opts PromptOptions) string {
	prompt := `Continue grilling the same design document against the same rubric as the prior turn.`
	if goal != "" {
		prompt += "\n\nContext for this turn: " + goal
	}
	prompt += `

If you have no prior review context for this document (because the backend silently fell back to a fresh session despite the resume request), treat this as a first-pass review: re-read the document and apply the design-doc severity rubric and JSON output format. Otherwise, proceed with the resume protocol below.

Re-grill the document with fresh eyes — including sections you previously accepted. Pay particular attention to:
1. Issues introduced by edits the orchestrator made since the prior turn.
2. Items you flagged before that you now have stronger evidence for — cite the section that proves it.
3. Systemic issues you skipped or dismissed before that, on a second look, warrant flagging.

Avoid restating prior findings verbatim, but DO surface any new systemic issues — including in sections you already accepted. A second pass that finds something the first pass missed is more useful than one that just confirms the prior verdict.

Cite section headings (not line numbers). Tag each issue with the rubric question it answers ("dimension": "qN"). Apply the same severity rubric and JSON output format as the prior turn.`
	prompt += buildRubricRecap(opts.Rubric)
	return prompt
}

// buildRubricRecap appends a short numbered list of the rubric questions
// to a follow-up prompt so a silent-resume fallback session has the
// rubric in hand without depending on the orchestrator threading
// --review-rubric-file (which it does, but the model still has to read
// the file). Mirrors the code-mode follow-up's scope-suffix safety net.
// Returns empty when the rubric is empty (defence-in-depth — the call
// site that produced this prompt would have already returned a
// MISCONFIGURED sentinel from buildDesignDocBasePrompt; this is just a
// belt-and-suspenders no-op).
func buildRubricRecap(rubric []string) string {
	rubric = filterPromptHints(rubric)
	if len(rubric) > rubricCap {
		rubric = rubric[:rubricCap]
	}
	if len(rubric) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\nRubric (recap):\n")
	for i, q := range rubric {
		fmt.Fprintf(&b, "%d. %s\n", i+1, q)
	}
	return strings.TrimRight(b.String(), "\n")
}

// jsonOutputRules returns the per-mode output-format spec appended to every
// JSON-output prompt. Code mode keeps the legacy schema (file/line,
// accepted/rejected) byte-for-byte. Design-doc mode swaps file/line for
// section/dimension and uses ready/revise/rethink verdicts.
//
// The two specs deliberately share severity vocabulary so the orchestrator's
// triage layer can rank findings the same way across modes. They diverge
// only on the addressing fields (file/line vs section/dimension) and the
// verdict enum, which is what validateReviewBody dispatches on.
func jsonOutputRules(mode ReviewMode) string {
	if mode == ReviewModeDesignDoc {
		return designDocJSONOutputRules
	}
	return codeJSONOutputRules
}

const codeJSONOutputRules = `

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
      "confidence": 0.9,
      "invariant": "short name of the rule (optional, see Class-level findings)",
      "sites": [{"file": "path/to/file.go", "line": 42}, {"file": "path/to/other.go", "line": 17}]
    }
  ],
  "sufficiency": {"is_confident_complete": true, "evidence": "..."}
}

## Severity Levels
- critical: Bugs, security vulnerabilities, broken functionality, data loss risks
- high: Missing error handling, incorrect logic that could cause failures
- medium: Code style issues, minor inefficiencies, missing edge cases
- low: Naming preferences, formatting, optional improvements

## Class-level findings (invariant + sites)
When you find N >= 2 sibling sites of the same rule violation, emit ONE
issue with:
- "invariant": a short name for the rule (3-8 words; e.g. "ambient env
  vars shadow explicit proxy keys"). Name the rule being violated, not
  the symptom.
- "sites": [{"file": "...", "line": ...}, ...] listing every site.
- "file" and "line" at the top of the issue: the most representative
  site (pick one entry from "sites"). Required for back-compat with
  single-site triage.
- "message": describe the invariant once, then list the sites by reference.
  Do not repeat the same wording per site.
Do NOT emit N separate single-site issues for one invariant. A single
finding that names the producer-side invariant beats N findings that
patch consumer sites.

## Sufficiency (optional, resume sessions only)
On round 2+, you MAY emit a top-level "sufficiency" object:
- "is_confident_complete": true if you believe the prior turn's fixes
  addressed every invariant you can find at this scope. false if you
  suspect more sites or a different class of issue remain.
- "evidence": 1-2 sentences explaining why. Optional.
This is a hint to the orchestrator, not a new gate — the verdict +
issues array remain authoritative. Omit on first-pass reviews.

## Rules
- verdict MUST be exactly "accepted" or "rejected". Aliases like
  "approve_with_notes", "request_changes", "lgtm", or "needs-changes"
  are normalized but you should pick the canonical token.
- If there are any critical or high severity issues, verdict MUST be "rejected"
- issues array can be empty if verdict is "accepted". A clean diff is a
  legitimate outcome; do not strain to find something to flag.
- Each issue MUST include severity, file, line (>= 1), and message; suggestion is optional
- confidence is an optional float in (0.0, 1.0]: 1.0 = certain, 0.5 = plausible but unverified; omit only when you cannot assess (the field is treated as "no signal", not as a default value)
- When "invariant" is set, "sites" MUST list >=1 entries and "file"/"line" MUST match one of them.
- Output ONLY the JSON object, no other text`

const designDocJSONOutputRules = `

## Output Format
You MUST respond with valid JSON in this exact format:
{
  "verdict": "ready" or "revise" or "rethink",
  "summary": "Brief overall assessment of the document",
  "confidence": 0.7,
  "issues": [
    {
      "severity": "critical|high|medium|low",
      "section": "Milestone 2: Multi-tenant rollout",
      "dimension": "q2",
      "message": "Description of the systemic issue",
      "suggestion": "What to change in the doc",
      "confidence": 0.9
    }
  ]
}

## Severity Levels
- critical: The design as written will not work or will cause irreversible harm.
- high: A core question (long-term fit, milestone boundary, simplicity) the doc fails to answer convincingly.
- medium: A meaningful systemic gap or ambiguity that a careful reader would catch.
- low: A minor clarification or improvement that doesn't block the design.

## Rules
- verdict MUST be exactly one of "ready", "revise", or "rethink". The verdict is an imperative — what should the author do with this doc?
- "ready": ship as-is. No high/critical issues; the design is implementation-ready.
- "revise": address issues, the doc shape is right. Prefer this band when issues are fixable in-place.
- "rethink": premise needs reconsideration. Use sparingly — pick this when the doc needs more than copy-edits, e.g. at least one high/critical issue or a cluster of mediums that together challenge the design's premise.
- These bands are guidance, not a hard rule. The validator only enforces the "ready ⇔ no high/critical issues" symmetry; verdict-vs-severity calibration beyond that is your judgement call.
- Top-level "confidence" is a float in [0.0, 1.0] reporting confidence in the verdict itself.
- Each issue MUST include severity, section (a heading from the doc, or "(whole document)" for doc-wide issues), dimension (the rubric question it answers, e.g. "q1"), and message; suggestion is optional.
- Per-issue "confidence" is an optional float in (0.0, 1.0]; omit when you cannot assess.
- Do NOT include "file" or "line" — section is the durable address for a doc.
- issues array can be empty only if verdict is "ready".
- Output ONLY the JSON object, no other text`

// ReviewResult contains the result of a review turn.
type ReviewResult struct {
	ResponseText string
	ErrorMessage string
	ResumeStatus ResumeStatus
	DurationMs   int64
	InputTokens  int64
	OutputTokens int64
	Success      bool
}

// Reviewer wraps an agent backend for code review operations.
type Reviewer struct {
	output         io.Writer
	backend        Backend
	renderer       *render.Renderer
	lastSessionID  string
	resumeStatus   ResumeStatus
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
	defer r.renderer.Reset()

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
	r.resumeStatus = ""
	result, err := r.backend.RunPrompt(ctx, prompt, handler)
	if result != nil && result.ResumeStatus != "" {
		r.resumeStatus = result.ResumeStatus
	}
	if err != nil {
		if result != nil {
			r.renderer.TurnCompleteWithTokens(result.Success, result.DurationMs, result.InputTokens, result.OutputTokens)
		}
		return result, err
	}
	r.renderer.TurnCompleteWithTokens(result.Success, result.DurationMs, result.InputTokens, result.OutputTokens)
	return result, nil
}

// ResumeStatus returns the most recently observed resume status from a
// completed RunPrompt turn:
//   - ResumeStatusOK       when a requested resume succeeded
//   - ResumeStatusFallback when the backend cold-started after a failed resume
//   - ResumeStatusUnverified when resume was requested but the backend
//     reached an early error before any Ready event could confirm the session id
//   - "" when no resume was requested
//
// Note: the field is cleared at the start of every turn (see ReviewWithResult
// and FollowUp) and only repopulated from result.ResumeStatus once RunPrompt
// returns. Callers that need a non-empty answer when a turn panics or aborts
// mid-flight should fall back to ResumeStatusUnverified themselves when
// config.ResumeSessionID was set; the panic-recovery guard in
// bramble/cmd/codereview/codereview.go is the canonical example.
func (r *Reviewer) ResumeStatus() ResumeStatus {
	return r.resumeStatus
}

// FollowUp sends a follow-up message to the existing backend session.
func (r *Reviewer) FollowUp(ctx context.Context, prompt string) (*ReviewResult, error) {
	defer r.renderer.Reset()

	handler := r.newEventHandler()
	r.resumeStatus = ""
	result, err := r.backend.RunPrompt(ctx, prompt, handler)
	if result != nil && result.ResumeStatus != "" {
		r.resumeStatus = result.ResumeStatus
	}
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
