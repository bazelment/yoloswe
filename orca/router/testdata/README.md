`gemini.json` is a deterministic Decisions protocol example, not a live capture.
It contains one `answers.handler` choice with confidence and probabilities for
concierge, planner and dispatcher. HTTP replay tests check the single-question
request, decoding, margin handling and fallback behavior.

The frozen six-destination owner-message labels live in
`../eval/testdata/owner_chat.json`; eval maps those labels to accepted handlers.
Live evaluations use the manual `//orca/router/integration:eval_test` target and
`ORCA_ROUTER_SPLIT=tune|holdout`. Credentials are inherited from the environment.
