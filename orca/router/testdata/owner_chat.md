# Owner-chat router labels

60 manually labelled cases from the scrubbed owner-mail archive in Nebula post
`92a3afb2-…` (title: *nebula-orch gate-2 source: scrubbed run mail
(cicd-300, feedback, s1-main)*), retrieved with `nebula post get` on 2026-11-13.
Every label carries the archive's hashed `s:…` mail ID and a rationale. These are
owner-side messages (`fromKind=user`), including delegated host relays; they are
not synthetic conversations. Most cases use a contiguous excerpt (`excerpt=true`)
to isolate a request that could be typed into chat. Full messages are marked
`excerpt=false`. No email addresses, credentials, or raw UUIDs are retained.

The first five cases were scored on the fallback before the full-set scoring.
They remain in the full set and are not a holdout. Question context, where present,
is a scrubbed, bounded excerpt of the original linked question. The deliberately
ambiguous cases omit that context: a bare option or unnamed merge request cannot
identify the intended question or work item. The source archive remains the
provenance authority; no private HOME lookup was used.

Labels are **unaudited**. A second pass in this lane is not an independent audit.
The orchestrator will fund an independent label audit. These exploratory scores
must not be described as audited quality measurements.

Destinations follow the public router contract: concierge answers a run/policy
question; answer-open-question records a reply; planner changes existing scope,
order or timing; memory stores a recurring rule; dispatcher assigns/steers work;
controller pauses, stops or resumes execution. Multipart labels list every
independent destination. Repeated destinations collapse into a set, so splitting
one request into several parts with the same destination earns no extra credit.
A multipart message only succeeds when its complete destination set and its
message-level needs-clarification flag agree. Ambiguous cases still have a
plausible intended destination plus `needs_clarification=true`.

Accuracy counts whole messages with exact destination-set **and** clarification
agreement; backend errors count as misses. Unnecessary-clarification rate is
needless asks divided by labelled unambiguous cases (not all messages). Latency
is end-to-end `Router.Route`, including transport/retries/normalization, with
serial calls and nearest-rank p95. Targets are >=90% accuracy, <10% unnecessary
clarifications, and zero backend errors. There is no latency target.

Run live, explicitly (excluded from `bazel test //...`):

```
ORCA_ROUTER_INNER_LOOP=1 bazel test //orca/router/integration:eval_test --test_timeout=60 --test_output=streamed
bazel test //orca/router/integration:eval_test --test_timeout=600 --test_output=streamed
```

The default is the LLM fallback using `gpt-6.1-sol` through the session OpenAI
gateway. `ORCH_MODEL` selects a different model. Environment credentials are
inherited by Bazel; `ANTHROPIC_BASE_URL` allows the existing OpenAI client to
derive the session gateway route when `OPENAI_BASE_URL` is unset. Gemini scoring
is skipped because no Gemini key exists for this owner today.

## Observed fallback scorecards (2026-11-13)

Five-case gateway inner loop, before full-set scoring:

> routing accuracy: 2/5 (40.00%); unnecessary clarifications: 0/5 (0.00%); errors: 0; latency mean=5.001s p50=3.605s p95=7.579s; targets_met=false

Frozen full set, same fallback model (`gpt-6.1-sol`):

> routing accuracy: 23/60 (38.33%); unnecessary clarifications: 9/55 (16.36%); errors: 0; latency mean=6.14s p50=5.466s p95=10.891s; targets_met=false

Both requested bars are missed by the full set. The manual test intentionally
returns failure for this score. The ordinary suite remains deterministic and
never calls a model. Gemini was skipped: no owner key exists. The labels have
not had independent review; the orchestrator will fund that audit before these
numbers are treated as audited evidence. Common observed confusions include
concierge questions being called answers to open questions, and plan changes or
standing rules being called controller operations. No production router prompt
was changed in this lane.

Two initial five-case attempts omitted gateway URL inheritance and received
five HTTP 401 failures each (0/5). Adding `ANTHROPIC_BASE_URL` to the manual
target's inherited environment corrected endpoint selection. Those attempts do
not measure router accuracy or successful-call latency.
