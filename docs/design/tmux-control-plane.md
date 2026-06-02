# Bramble tmux control plane + cloud hub

This document explains how to use the remote-control features added in PR #260:
driving bramble's tmux agent sessions from the command line, and reaching them
from a browser anywhere via a cloud relay hub.

## What this gives you

A bramble TUI runs AI sessions (planner/builder/codetalk) in tmux windows. The
control plane lets you **read and drive those sessions without being attached to
the TUI**:

- **Locally** — `bramble send-input` / `send-key` / `capture-pane` /
  `list-sessions` talk to the running TUI over a Unix socket.
- **Remotely** — the TUI dials out to a **hub** over a WebSocket; you open the
  hub's web UI in a browser, see your machines and their sessions, watch live
  pane output, and type prompts back — all behind a shared-secret login.

The same versioned JSON protocol (`bramble/control`) carries both paths. Every
tmux command is constrained by an allowlist (`bramble/tmuxctl`), so a
network-reachable control plane can never run `kill-server` or arbitrary shell.

```
                 ┌──────────────── your laptop / phone (browser) ────────────────┐
                 │   https://hub.example/   ←─ login (BRAMBLE_HUB_SECRET)         │
                 └───────────────────────────────┬───────────────────────────────┘
                                                  │ WebSocket (browser ↔ hub)
                                       ┌──────────▼──────────┐
                                       │   bramble-hub       │  relay only,
                                       │  (cloud / Tailscale)│  no tmux logic
                                       └──────────▲──────────┘
                                                  │ WebSocket (agent dials OUT)
                                                  │  auth: BRAMBLE_HUB_TOKEN
   ┌──────────────────────────── dev machine ────┴───────────────────────────────┐
   │  bramble TUI ── control.Dispatcher ── tmuxctl (allowlist) ── tmux ── sessions │
   │       ▲ Unix socket ($BRAMBLE_CONTROL_SOCK)                                   │
   │       └── bramble send-input / send-key / capture-pane (local CLI)            │
   └──────────────────────────────────────────────────────────────────────────────┘
```

The dev machine **dials out** to the hub (it never accepts inbound), so it works
behind NAT with no open port.

---

## 1. Local control (no hub)

Whenever a bramble TUI is running, it starts a control server on a per-process
Unix socket and exports its path as `$BRAMBLE_CONTROL_SOCK`. The CLI subcommands
read that env var to find the running TUI, so **run them from a shell that
inherited the TUI's environment** (e.g. a tmux pane bramble spawned), or export
`BRAMBLE_CONTROL_SOCK` yourself.

### List sessions

```bash
bramble list-sessions
```

(`list-sessions` uses the IPC channel; it works the same way and prints the
session IDs you pass to the commands below.)

### Send a prompt to a session

```bash
# Type text into a session's pane and press Enter:
bramble send-input --session-id <ID> --text "run the tests and summarize failures" --submit

# Type without submitting (e.g. to let the user review first):
bramble send-input --session-id <ID> --text "draft prompt..."
```

Text is delivered via tmux bracketed paste, so multi-line prompts are pasted as
one block and embedded newlines are **not** interpreted as separate submits.

### Send a single key

```bash
bramble send-key --session-id <ID> --key Enter
bramble send-key --session-id <ID> --key C-c      # interrupt
bramble send-key --session-id <ID> --key Escape
```

Allowed keys: `Enter`, `Escape`, `C-c`, `C-d`, `Tab`, `BSpace`, `Up`, `Down`,
`Left`, `Right`. Unknown keys are rejected before they reach tmux.

### Capture pane output

```bash
bramble capture-pane --session-id <ID> --lines 50
```

### Raw-pane mode (power use)

Instead of `--session-id`, you can address a tmux target directly with
`--target` (a window/pane id like `@3` or `%5`). This bypasses the session
registry but is still constrained by the allowlist:

```bash
bramble send-input --target @3 --text "hi" --submit
bramble send-key   --target %5 --key C-c
```

---

## 2. Remote control via the hub

### 2a. Run the hub

The hub is a separate binary (`bramble/cmd/hub`). Run it somewhere both your
browser and your dev machine can reach — typically behind TLS or on a private
network such as Tailscale.

```bash
# Build it:
bazel build //bramble/cmd/hub:hub

# Run it. BOTH secrets are required — the hub refuses to start without them.
BRAMBLE_HUB_SECRET=<browser-login-secret> \
BRAMBLE_HUB_AGENT_TOKEN=<agent-auth-token> \
  ./bazel-bin/bramble/cmd/hub/hub_/hub --addr :8787
```

- `BRAMBLE_HUB_SECRET` — the secret a human types at `/login` to get a browser
  session cookie.
- `BRAMBLE_HUB_AGENT_TOKEN` — the token dev machines present to register as
  agents. **Required**: agent auth fails closed — a hub with no agent token
  rejects every agent, so `/agent` is never unauthenticated.

> Run the hub behind TLS (or Tailscale). The login cookie is marked `Secure`
> automatically when the request arrives over HTTPS (or via an
> `X-Forwarded-Proto: https` proxy).

### 2b. Point a dev machine at the hub

The bramble TUI dials the hub when `BRAMBLE_HUB_URL` is set. Auth and identity
come from the environment (kept off the TUI flags):

| Env var | Meaning |
|---|---|
| `BRAMBLE_HUB_URL` | Hub agent endpoint, e.g. `wss://hub.example/agent` |
| `BRAMBLE_HUB_TOKEN` | Must match the hub's `BRAMBLE_HUB_AGENT_TOKEN` |
| `BRAMBLE_MACHINE_ID` | Stable id for this machine (defaults to hostname) |

```bash
BRAMBLE_HUB_URL=wss://hub.example/agent \
BRAMBLE_HUB_TOKEN=<agent-auth-token> \
BRAMBLE_MACHINE_ID=workstation-1 \
  bramble
```

The TUI keeps the connection alive with exponential backoff and reconnects
automatically if the hub restarts. If `BRAMBLE_HUB_URL` is unset, nothing dials
out — local control still works.

### 2c. Use the web UI

1. Open `http://<hub-host>:8787/` in a browser.
2. Log in with `BRAMBLE_HUB_SECRET`.
3. Pick a machine in the sidebar (the sole machine is auto-selected).
4. Pick a session — its live pane streams in, updating as the agent works.
5. Type in the input box and press Enter (or use the key buttons) to drive the
   session remotely.

The UI streams pane deltas only when content actually changes, so an idle
session produces no traffic. If a pane can no longer be captured (e.g. it was
killed), the UI shows a stream-error notice instead of silently freezing.

---

## 3. Security model

- **Two independent secrets.** Browser login (`BRAMBLE_HUB_SECRET`) and agent
  registration (`BRAMBLE_HUB_AGENT_TOKEN`) are separate; both are mandatory and
  compared in constant time. Auth fails **closed**.
- **Allowlist chokepoint.** Every tmux subcommand routes through
  `tmuxctl`'s allowlist (read + send + window lifecycle only). Destructive
  server-wide commands (`kill-server`, `kill-session`) and arbitrary shell are
  never permitted, even for a network caller.
- **Outbound-only agents.** Dev machines dial the hub; they never listen, so no
  inbound port is exposed.
- **Run behind TLS / a private network.** The hub's auth is a deliberately
  minimal network-boundary + token model (mirrors tmux-mobile); it expects to
  sit behind TLS or Tailscale, not on the open internet.

---

## 4. How it fits together (for contributors)

| Package | Role |
|---|---|
| `bramble/tmuxctl` | The tmux command vocabulary behind an allowlist. Reads delegate to `bramble/session` capture/parse primitives; writes (send-keys, paste-buffer, window lifecycle) are new here. |
| `bramble/control` | Versioned JSON protocol (`ProtocolVersion`), a transport-agnostic `Dispatcher`, the Unix + WebSocket transports, and live pane streaming (`pane.subscribe`/`pane.delta`/`pane.error`). |
| `bramble/remote` | The agent-side hub client: dials out over WebSocket, does the `Hello`/`HelloAck` handshake, then serves control requests the hub forwards. |
| `bramble/hub` | The cloud relay: browser auth + session cookies, an agent registry, request/delta routing, and the web UI. Holds **no** tmux logic. |
| `bramble/cmd/hub` | The standalone hub binary. |

The protocol has two addressing modes:

- **Session-centric** (`session.*`) — address a bramble agent session by its
  `SessionID`; the dispatcher resolves it to a tmux target through the registry.
- **Raw-pane** (`tmux.*` / `pane.*`) — address a tmux window/pane id directly.

Both are served by the same `Dispatcher` and constrained by the same allowlist,
whether the request arrives over the local Unix socket or the hub WebSocket.

### Tests

- `bramble/tmuxctl/exec_test.go` — argv builders, allowlist, paste-buffer naming.
- `bramble/control/*_test.go` — dispatcher routing, transport, streaming
  (dedup, terminal error frame, model-only change detection).
- `bramble/hub/*_test.go` — browser auth, agent handshake (accept/reject),
  request forwarding, disconnect + timeout handling, end-to-end stream.
- `bramble/cmd/hub/integration/` and `bramble/tmuxctl/integration/` — full-chain
  and live-tmux tests (tagged `manual`, excluded from `bazel test //...`).
