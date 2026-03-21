# Delegator Output Analysis Checklist

Read this during Step 5 (Analyze). Check every item against both rendered stdout and JSONL logs.

## Rendered Output (stdout)

| # | Check | Pass condition |
|---|-------|----------------|
| 1 | Text coherence | No word-boundary fragments like `"TheI"`, `"GreatHere"` — streaming chunks must be buffered at word boundaries |
| 2 | Tool display | Only 3 delegator tools shown: `start_session`, `stop_session`, `get_session_progress` |
| 3 | Child lifecycle | Sessions start, run, complete with status lines |
| 4 | Turn structure | Each turn: thinking (dim) → text → tool calls → `✓ Turn N (Xs, $X.XXXX)` |
| 5 | No noise | No stderr leakage, no "Starting claude with flags", no "WARN skipping unknown" |
| 6 | Cost summary | Final `Total: $X.XXXX (...)` line present with reasonable values |
| 7 | No timeout | Run completed before the 8-minute timeout |
| 8 | Progress ticker | For children running >30s: `⏳ session-id (type) Ns, turns: N, $X.XXXX` |

## JSONL Verification

JSONL session logs are **ground truth**. Check these even if rendered output looks clean:

- **Tool count per session:** Delegator sessions must have exactly 3 tools. Children should have ~26. Count from `"type":"system","subtype":"init"` messages.
- **No ToolSearch in delegator:** Any `ToolSearch` call in a 3-tool session is a regression.
- **Child completion:** `"stop_reason":"end_turn"` must appear — absence means timeout or crash.
- **Model propagation:** Child `init` messages should show the expected model, not a default.
- **Cost values:** `OutputTypeTurnEnd` lines carry the authoritative cost data.

```bash
# Tool count per session
for f in "$LOG_DIR"/session-*/messages.jsonl; do
  count=$(grep '"type":"system","subtype":"init"' "$f" | \
    python3 -c "import sys,json; d=json.loads(sys.stdin.readline()); print(len(d['message'].get('tools',[])))" 2>/dev/null)
  echo "$(basename $(dirname $f)): $count tools"
done

# Check child completion
grep -c '"stop_reason":"end_turn"' "$LOG_DIR"/session-*/messages.jsonl
```

## Finding Template

For each gap found, record:

```
## Finding: <short title>
**Observed:** What happened
**Expected:** What should have happened
**Severity:** high | medium | low
**Root cause:** Where in the code this originates
**Files:** Source files to fix
```

Group findings by root cause — multiple symptoms often share one underlying issue.
