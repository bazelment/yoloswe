`fallback.json` and `gemini.json` are deterministic protocol examples, not
captures of owner conversations or a measured model-accuracy dataset.

`fallback.json` exercises structured multipart model output. `gemini.json` follows
the public google-genai SDK's generated Decisions response types (`answers`,
`choice`, `noul`, and per-question `confidence`):
https://github.com/googleapis/python-genai/tree/main/google/genai/_gaos/types/decisions
The SDK sends POST /v1beta/decisions with model `models/gemini-decision-1` and a
map of named choice/Noul questions. The HTTP replay tests validate that request
contract as well as response decoding. Live model calls remain outside Bazel's
normal test suite. The owner-message accuracy evaluation belongs to router-eval.

`fallback-live.json` is a real gpt-6.1-sol response captured through the session's
OpenAI gateway on 2026-11-13 for "Change the plan to ship chat first." The raw
structured model text is replayed by TestRecordedFallbackLiveResponse. The probe
ran separately from Bazel tests. A live Gemini recording could not be made:
the session has no configured GEMINI_API_KEY or GOOGLE_API_KEY. No owner-home
credential lookup was performed.
