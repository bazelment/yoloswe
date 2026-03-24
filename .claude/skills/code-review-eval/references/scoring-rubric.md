# Review Eval Scoring Rubric

## Severity Weights

When computing weighted metrics, multiply each finding's score by its severity weight:

| Severity | Weight | Rationale |
|----------|--------|-----------|
| High     | 3.0    | Bug, security flaw, or data loss risk — must catch |
| Medium   | 2.0    | Correctness issue or significant maintainability problem |
| Low      | 1.0    | Minor issue, suboptimal pattern, missing edge case |
| Nit      | 0.5    | Style, naming, formatting — nice to catch, not critical |

## Category Definitions

| Category | What counts |
|----------|------------|
| **Correctness** | Logic bugs, race conditions, nil dereference, wrong return values, broken invariants |
| **Security** | Injection, auth bypass, secrets in code, unsafe deserialization, SSRF |
| **Performance** | O(n²) where O(n) is possible, unnecessary allocations, missing caching, unbounded growth |
| **Maintainability** | Code duplication, leaky abstractions, missing error handling, poor naming |
| **Style** | Formatting, naming conventions, comment quality, idiomatic patterns |
| **Test coverage** | Untested code paths, missing edge cases, brittle assertions |

## Finding Classification Rules

### True Positive (TP)
The reviewer identified a real issue that exists in the ground truth. The finding must:
- Reference the correct file (and approximately correct line range, within ~10 lines)
- Identify the same category of problem (e.g., both say "race condition" or both say "goroutine leak")
- A finding that identifies the right symptom but wrong root cause is still TP (diagnosis quality scored separately)

### False Positive (FP)
The reviewer flagged something that is NOT an issue. Common FP patterns:
- Flagging correct error handling as "missing error handling"
- Flagging intentional design choices as bugs
- Flagging framework-guaranteed behavior as risky
- Hallucinating issues in code that doesn't exist in the diff

### False Negative (FN)
A ground truth issue that the reviewer completely missed. No finding references
the same file+area or the same problem category.

### Partial Match (PM)
The reviewer found something in the right area but got the diagnosis wrong.
Score as 0.5 TP for precision/recall computation.

### Out of Scope
A real issue that exists in the codebase but was NOT introduced by this diff.
Do not count as TP or FP — exclude from metrics entirely. Note it separately.

## Quality Sub-Scores (per TP finding)

### Location Accuracy (0-2)
- 0: Wrong file entirely
- 1: Right file, wrong line range (off by >10 lines)
- 2: Correct file and line range (within ~5 lines)

### Diagnosis Quality (0-2)
- 0: Wrong explanation of the problem
- 1: Vague or partially correct ("this might be a problem")
- 2: Precise root cause identification

### Fix Quality (0-2)
- 0: No fix suggested, or suggested fix is wrong/would break something
- 1: Partial fix (addresses symptom but not root cause, or incomplete)
- 2: Correct, complete fix that addresses the root cause
