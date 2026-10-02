# Conductor API mechanics

Use the [Conductor API documentation](https://www.conductor.build/docs/api) and its [OpenAPI schema](https://api.conductor.build/v0/openapi.json) as the current contract. The API is beta; refresh the schema when a request shape or model ID is uncertain. This reference was checked against the schema on 2026-10-02.

The API base is `https://api.conductor.build/v0`. Require `CONDUCTOR_API_KEY` and authenticate with `Authorization: Bearer $CONDUCTOR_API_KEY`. If it is absent, ask the user to create a key at [Conductor API keys](https://app.conductor.build/home/api-keys) and make it available through their environment or secret store. Never put the key in prompts, output, a run ledger, or a checked-in file.

Use `scripts/api.py` from the skill directory for requests. It prints JSON on success and an API error on stderr without printing the credential:

```bash
python3 scripts/api.py GET /projects
python3 scripts/api.py POST /workspaces --data-file /path/to/create.json
python3 scripts/api.py GET /sessions/SESSION_ID/status
python3 scripts/api.py GET '/sessions/SESSION_ID/messages?after=MESSAGE_ID'
python3 scripts/api.py POST /sessions/SESSION_ID/messages --data-file /path/to/message.json
```

Resolve a project from paginated `GET /projects` results using its repository identity. `POST /workspaces` takes `projectId` (or `repositoryUrl`), `branch` (source branch), `name`, explicit `agent` and compatible `model`, and optionally `effort` and `message`. It returns `workspaceId`, `sessionId`, `deepLink`, and, with `message`, `initialMessage`. Name the workspace after its lane. The workspace name also determines its new git branch; `branch` chooses the source branch. Do not assume a local branch on the orchestrator's machine is present remotely. Have the lane report its initial commit SHA and compare it with the intended remote source SHA before work proceeds.

`POST /sessions` with `workspaceId`, `agent`, `model`, and optional `name` makes a new agent chat in an existing workspace. `POST /sessions/{sessionId}/messages` takes `{"message":"..."}` and returns a message ID and `queued` or `sent` state. Persist that ID. `GET /sessions/{sessionId}/status` reports `idle`, `working`, or `error`; `GET /sessions/{sessionId}/messages?after=<id>` returns new transcript entries. List endpoints use `{data, offset, hasMore}`; follow `hasMore` and advance the transcript cursor by the last processed message ID. Preserve each session's cursor separately.

Use `GET /workspaces/{workspaceId}/status` for machine lifecycle (`initializing`, `ready`, `sleeping`, `archived`, and errors) and `GET /workspaces/{workspaceId}` for its deep link and current identity. A machine that is initializing or sleeping is not an agent failure. A session can remain `idle` while its first message is queued, so seek a reply or observe a `working -> idle` transition before judging completion. Treat an empty or failed API response as unknown, not proof of absence.

Conductor does not expose the remote filesystem through these endpoints. To verify files or git state, ask the remote session for concrete output, inspect an authorized pushed branch/PR from the orchestrator, or use another explicitly available remote execution tool. Do not apply local `git -C`, tmux, pane, process, or file-signal checks to the remote machine.

`POST /sessions/{id}/cancel` drops the current turn and queued messages. `POST /workspaces/{id}/sleep` and `/archive` change the workspace lifecycle. Use these only when the invocation authorizes that effect and after recording recoverable work. Avoid cancelling a turn just because progress is slow.
