# Engineering Principles

Lessons distilled from code review findings. Refer to these when designing new
abstractions, adding wire formats, or reviewing PRs.

---

## 1. A new abstraction is only real when it owns its invariants end-to-end

When you introduce a type as "the single source of truth," ask:
*What are the invariants this type is responsible for, and what enforces them?*

If the answer is "callers must remember to do X," the abstraction is incomplete.
Every lifecycle transition (e.g. status → terminal), every identity field
(e.g. session ID), every capacity contract (e.g. capped vs. uncapped buffer)
must have a clear owner — and that owner must enforce it, not document it.

**Example failure mode:** `SessionModel` declared itself the source of truth for
status, but `handleResult` never called `UpdateStatus`. The invariant had no
enforcer, so it was silently violated.

---

## 2. Parallel code paths drift unless something forces them to stay in sync

When a new code path is added alongside an existing one, it silently omits
things the old one handled. Review diligence is not a reliable defense.

The only reliable fix is a **shared contract** — a test every path must satisfy,
or a single function all paths must call — that makes omissions fail loudly.

**Example failure mode:** Raw JSONL parsing omitted the session ID copy and the
user-message capture that the SDK recorder path handled correctly. Nothing in
the test suite required both paths to produce the same metadata shape.

---

## 3. Tests should make wrong states unrepresentable, not just test happy paths

Design tests by asking: *What states must this code never be in?* — then assert
those directly.

"Output is non-empty" leaves an enormous space of silent failures. Prefer
property assertions:
- Status is always terminal after a complete session parse
- Identity fields (session ID, model, CWD) are always non-empty after init
- Prompt always reflects the user's first message, not the assistant's first reply

For new wire formats specifically, the loader test must assert:
- Final status is terminal (not stuck at `running`)
- `meta.SessionID` is non-empty
- Prompt extracted matches the user's first message
- Line count is not artificially capped

---

## 4. The cost of a gap is paid at the edge, not where the gap was introduced

Status bugs surface as display errors in the UI. Missing session IDs surface as
broken identity in future features. Wrong prompts surface as confusing replay
headers. The gap is cheap to close at the point of introduction and expensive
to find downstream.

Write down invariants and test them where the code is written — not where it
is consumed.
