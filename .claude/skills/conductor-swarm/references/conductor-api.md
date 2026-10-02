# Conductor API mechanics

Use the [Conductor API documentation](https://www.conductor.build/docs/api) and its [OpenAPI schema](https://api.conductor.build/v0/openapi.json) as the current contract. The API is beta; refresh the schema when a request shape or model ID is uncertain. This reference was checked against the schema on 2026-10-02.

The API base is `https://api.conductor.build/v0`. Require `CONDUCTOR_API_KEY` and authenticate with `Authorization: Bearer $CONDUCTOR_API_KEY`. If it is absent, ask the user to create a key at [Conductor API keys](https://app.conductor.build/home/api-keys) and make it available through their environment or secret store. Never put the key in prompts, output, a run ledger, or a checked-in file.

Use `scripts/api.py` from the skill directory. It builds API payloads, follows list pagination, and prints JSON results or an error without printing the credential:

```bash
python3 scripts/api.py projects
python3 scripts/api.py find-workspaces --name lane-name --repo-url REPOSITORY_URL
python3 scripts/api.py create-workspace --project-id PROJECT_ID --branch main \
  --name lane-name --agent codex --model MODEL_ID --brief-file /path/to/brief.txt
python3 scripts/api.py sessions --workspace-id WORKSPACE_ID
python3 scripts/api.py create-session --workspace-id WORKSPACE_ID \
  --name lane-clean --agent codex --model MODEL_ID --brief-file /path/to/clean-brief.txt
python3 scripts/api.py observe-session --session-id SESSION_ID --after LAST_MESSAGE_ID
python3 scripts/api.py send --session-id SESSION_ID --brief-file /path/to/follow-up.txt
```

Resolve a project from the complete `projects` result using its repository identity. `create-workspace` accepts `--project-id` or `--repository-url`, a source `--branch`, a lane `--name`, an explicit compatible `--agent` and `--model`, optional `--effort`, and a nonempty brief file. It sends the brief as the initial message in the same creation request and returns `workspaceId`, `sessionId`, `deepLink`, and `initialMessage`. Name the workspace after its lane. The workspace name also determines its new git branch; `branch` chooses the source branch. Do not assume a local branch on the orchestrator's machine is present remotely. Have the lane report its initial commit SHA and compare it with the intended remote source SHA before work proceeds.

`create-session` makes a fresh agent chat in the existing workspace and sends its phase brief in the same request. `send` handles follow-up messages: it prints a UUID before sending, so reuse `--message-id` with that UUID if the request outcome is uncertain. Record the returned message ID. `observe-session` combines session status (`idle`, `working`, or `error`) with all new transcript entries and `lastMessageId`, following pagination internally. Advance the ledger cursor only after processing the returned messages. Preserve each session's cursor separately.

If workspace or session creation returns an uncertain result, do not immediately repeat the POST. Use `find-workspaces --name ... --repo-url ...` for a server-filtered exact workspace match, then `sessions --workspace-id` to reconcile the chat. Both follow pagination. Listing every workspace in a large project can be slow. If a creation response has no `initialMessage`, inspect the session transcript before sending the brief separately.

Use `workspace-status` for machine lifecycle (`initializing`, `ready`, `sleeping`, `archived`, and errors) and `workspace` for its deep link and current identity. A machine that is initializing or sleeping is not an agent failure. A session can remain `idle` while its first message is queued, so seek a reply or observe a `working -> idle` transition before judging completion. Treat an empty or failed API response as unknown, not proof of absence.

Conductor does not expose the remote filesystem through these endpoints. To verify files or git state, ask the remote session for concrete output, inspect an authorized pushed branch/PR from the orchestrator, or use another explicitly available remote execution tool. Do not apply local `git -C`, tmux, pane, process, or file-signal checks to the remote machine.

`cancel-session` drops the current turn and queued messages. `sleep-workspace` and `archive-workspace` change the workspace lifecycle. Use these only when the invocation authorizes that effect and after recording recoverable work. Avoid cancelling a turn just because progress is slow.
