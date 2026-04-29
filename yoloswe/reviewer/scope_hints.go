package reviewer

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ScopeHintsSchemaVersion is the wire-format version of the scope-hints
// JSON file consumed by `bramble code-review --scope-hints-file`. Bump
// when the loader needs to distinguish shapes — i.e. when a new field
// changes the prompt-building path on the reviewer side, so old producers
// shouldn't accidentally activate it. Pure additions that don't reroute
// existing inputs (a new optional field that no clause reads) can land
// without a bump.
//
// v1: original shape (cross_service_packages only).
// v2: adds changed_packages and dependency_packages, which select the
//
//	caller/callee framing in buildScopeSuffix. cross_service_packages is
//	still accepted (union of both new fields) so v1 producers keep
//	working through a rolling upgrade.
const ScopeHintsSchemaVersion = 2

// scopeHintsMaxBytes caps the size of a scope-hints file LoadScopeHints will
// read. The expected on-disk shape is small (test_paths capped at 50 entries
// + a list of top-level packages), so a realistic file fits in a few KiB;
// 1 MiB is generous headroom. The cap defends against a hostile or accidental
// input — a symlink to /dev/zero, a never-completing FIFO, a runaway producer
// — that could otherwise read until OOM. Reads above the cap fail with a
// descriptive error and trigger the CLI's warn-and-fallback path, identical
// to malformed JSON.
const scopeHintsMaxBytes = 1 << 20

// ScopeHints is the JSON contract between bramble and any caller (today: the
// /pr-polish skill's scope_gate.py) that wants to widen the review scope.
//
// The shape is small on purpose: bramble owns the prompt structure and the
// types here, callers compute the lists. See plan
// plans/issue-175-widen-review-scope.md for the design rationale.
type ScopeHints struct {
	// TestPaths lists co-located test files the reviewer should read in
	// addition to anything in the diff. When non-empty, the test-quality
	// clause is appended to the prompt.
	TestPaths []string `json:"test_paths"`

	// CrossServicePackages names all top-level packages the PR touches
	// (union of ChangedPackages and DependencyPackages). Kept for
	// backwards compatibility with v1 consumers; v2 files populate this
	// alongside the split fields.
	CrossServicePackages []string `json:"cross_service_packages"`

	// ChangedPackages names the top-level packages directly modified by
	// this diff (v2+). When non-empty, the cross-service clause uses
	// explicit caller/callee framing — DependencyPackages is optional and
	// gates only the "callers or dependencies" sentence.
	ChangedPackages []string `json:"changed_packages,omitempty"`

	// DependencyPackages names packages that import or are imported by the
	// changed packages — the callers/callees to check for contract drift
	// (v2+). The cross-service clause flags these as the "other side" of
	// each interface.
	DependencyPackages []string `json:"dependency_packages,omitempty"`

	// SchemaVersion must be 1 or ScopeHintsSchemaVersion (currently 2).
	// LoadScopeHints accepts both to allow a rolling upgrade: old
	// scope_gate.py output (v1, no split fields) keeps working while
	// callers adopt the v2 shape (split fields present). Anything else
	// is rejected loudly so a future breaking change is caught rather
	// than silently producing a degenerate prompt.
	SchemaVersion int `json:"schema_version"`
}

// LoadScopeHints reads and validates a ScopeHints JSON file.
//
// The function is strict on schema version (rejects mismatches) and on JSON
// well-formedness (rejects malformed payloads), but permissive on missing
// fields: an empty TestPaths or CrossServicePackages is a valid
// "no clause" signal, not an error.
//
// The CLI layer is expected to log-and-fall-back on errors here rather than
// abort the review — a malformed scope-hints file should never block a
// caller from getting today's narrow review.
//
// # Path-redaction hygiene
//
// Error messages identify the file by basename only. The full path is the
// caller's input — they already know it — and run logs are routinely shared
// across machines and PRs, so embedding the developer's worktree layout in
// every fallback warning weakens the same path-redaction hygiene used
// elsewhere in the run-log pipeline. Note this means we never wrap raw
// *os.PathError values with %w (their .Error() text contains the absolute
// path); we classify them via errors.Is and emit basename-only messages.
//
// # Defensive open
//
// Stat-then-open: rejecting non-regular files (FIFOs, /dev/zero, sockets)
// before os.Open closes a separate denial-of-service shape from the
// 1-MiB read cap — opening a FIFO blocks indefinitely on read, so the
// size cap alone wouldn't help. Symlinks are followed (os.Stat, not
// Lstat) because the realistic producer (/pr-polish/scope_gate.py)
// writes to a known directory and a malicious symlink there is already
// a much bigger problem than this code can address.
func LoadScopeHints(path string) (*ScopeHints, error) {
	tag := filepath.Base(path)
	info, err := os.Stat(path)
	if err != nil {
		return nil, sanitizedFSError("stat", tag, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf(
			"scope-hints file %s: not a regular file (mode=%s)",
			tag, info.Mode().String(),
		)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, sanitizedFSError("open", tag, err)
	}
	defer f.Close()

	// LimitReader so a /dev/zero-shaped input can't OOM the process. The
	// +1 lets us detect overflow: if we read scopeHintsMaxBytes+1 bytes
	// the file is at-or-above the cap and we reject before parsing.
	data, err := io.ReadAll(io.LimitReader(f, scopeHintsMaxBytes+1))
	if err != nil {
		return nil, sanitizedFSError("read", tag, err)
	}
	if len(data) > scopeHintsMaxBytes {
		return nil, fmt.Errorf(
			"scope-hints file %s: exceeds %d-byte cap", tag, scopeHintsMaxBytes,
		)
	}
	var h ScopeHints
	if err := json.Unmarshal(data, &h); err != nil {
		return nil, fmt.Errorf("parse scope-hints file %s: %w", tag, err)
	}
	// Accept v1 (original shape) and v2 (adds changed_packages /
	// dependency_packages). Reject anything else loudly so a future
	// breaking change is caught rather than silently producing a
	// degenerate prompt.
	if h.SchemaVersion != 1 && h.SchemaVersion != ScopeHintsSchemaVersion {
		return nil, fmt.Errorf(
			"scope-hints file %s: schema_version=%d, want 1 or %d",
			tag, h.SchemaVersion, ScopeHintsSchemaVersion,
		)
	}
	if err := validateHintStrings(tag, h.TestPaths, "test_paths"); err != nil {
		return nil, err
	}
	if err := validateHintStrings(tag, h.CrossServicePackages, "cross_service_packages"); err != nil {
		return nil, err
	}
	if err := validateHintStrings(tag, h.ChangedPackages, "changed_packages"); err != nil {
		return nil, err
	}
	if err := validateHintStrings(tag, h.DependencyPackages, "dependency_packages"); err != nil {
		return nil, err
	}
	return &h, nil
}

// sanitizedFSError converts an os filesystem error into a basename-only
// message. Wrapping the original error with %w would re-export the
// absolute path embedded in *os.PathError.Error() — exactly what the
// path-redaction hygiene above is trying to prevent.
//
// The classification is intentionally narrow: we identify a few common
// shapes (not-exist, permission, deadline) and fall back to a generic
// "operation failed" otherwise. The CLI's slog warning gives operators
// enough signal to investigate via the path they already passed in;
// callers needing the raw error chain can use os.Stat / os.Open
// directly instead of this loader.
func sanitizedFSError(op, tag string, err error) error {
	switch {
	case errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("%s scope-hints file %s: does not exist", op, tag)
	case errors.Is(err, os.ErrPermission):
		return fmt.Errorf("%s scope-hints file %s: permission denied", op, tag)
	case errors.Is(err, os.ErrDeadlineExceeded):
		return fmt.Errorf("%s scope-hints file %s: deadline exceeded", op, tag)
	default:
		return fmt.Errorf("%s scope-hints file %s: %s failed", op, tag, op)
	}
}

// validateHintStrings rejects entries that SanitizePromptHint would also
// reject at the prompt-builder boundary. The clauses inline these strings
// line-by-line under fixed Markdown headings, so a hint containing a
// newline could close "## Test quality" early, and a leading Markdown
// control character (#, -, *, >, _, =) could open a new section or list
// at line start. scope_gate.py emits filesystem paths and package buckets,
// neither of which exhibits these shapes — so this is a defense against a
// buggy or hostile producer, not a normal input.
//
// Failing here gives the operator a clear CLI warning ("test_paths[3]
// starts with Markdown control char '#'") that points straight at the
// producer bug. The prompt builder also filters on the same rules so a
// future direct caller of BuildJSONPromptWithScope can't bypass the file
// loader to inject Markdown — but it does so silently to keep prompt
// construction infallible. Errors live here; defense-in-depth lives there.
func validateHintStrings(tag string, items []string, field string) error {
	for i, s := range items {
		if SanitizePromptHint(s) {
			continue
		}
		switch {
		case s == "":
			return fmt.Errorf(
				"scope-hints file %s: %s[%d] is empty", tag, field, i,
			)
		case strings.ContainsAny(s, "\r\n"):
			return fmt.Errorf(
				"scope-hints file %s: %s[%d] contains newline", tag, field, i,
			)
		case s != strings.TrimSpace(s):
			return fmt.Errorf(
				"scope-hints file %s: %s[%d] has leading or trailing whitespace",
				tag, field, i,
			)
		default:
			return fmt.Errorf(
				"scope-hints file %s: %s[%d] starts with Markdown control char %q",
				tag, field, i, s[0],
			)
		}
	}
	return nil
}

// ToPromptOptions converts hints into PromptOptions, preserving the caller's
// SkipTestExecution setting which is orthogonal to scope. Pass through nil-
// safe: a nil receiver returns a zero PromptOptions.
func (h *ScopeHints) ToPromptOptions(skipTestExecution bool) PromptOptions {
	opts := PromptOptions{SkipTestExecution: skipTestExecution}
	if h == nil {
		return opts
	}
	opts.TestScopeHints = h.TestPaths
	opts.CrossServicePackages = h.CrossServicePackages
	opts.ChangedPackages = h.ChangedPackages
	opts.DependencyPackages = h.DependencyPackages
	return opts
}
