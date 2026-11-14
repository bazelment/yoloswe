Captured once in the PR #399 round-2 terminal with `codex exec --json
--sandbox read-only`, 2026-11-13. Only thread IDs were replaced. Token counts
are the recorded values; there are no account or endpoint fields.

`codex_exec_call.jsonl`: requested exactly
`{"tool":"calc","id":"call-1","input":{"a":2,"b":3}}` with no native tools.

`codex_exec_final.jsonl`: supplied the preceding assistant tool_use and a user
tool_result (`call-1`, content `5`), requested exactly
`{"text":"The result is 5."}` with no native tools.

These are two independent `exec` invocations, as used by the orca adapter.
Orca transports its function calls as JSON in assistant messages and executes
them through its own registry; it does not expose them as native Codex tools.
