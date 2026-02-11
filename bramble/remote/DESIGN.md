# Bramble Remote Execution Mode - Design Document

## Overview

Split Bramble into a **server** (runs sessions, manages worktrees, routes tasks) and a **client** (provides TUI, streams output, sends commands). Multiple TUI clients can connect to one server simultaneously.

```
┌─────────────────┐         gRPC          ┌──────────────────────────┐
│  bramble (TUI)  │◄───────────────────►  │  bramble serve (server)  │
│                 │   StreamEvents (SSR)   │                          │
│  SessionProxy   │◄──────────────────    │  session.Manager         │
│  WorktreeProxy  │   Unary RPCs  ──────► │  wt.Manager              │
│  TaskRouterProxy│                       │  taskrouter.Router       │
└─────────────────┘                       │  EventBroadcaster        │
                                          └──────────────────────────┘
```

## Status

- **Phase 1: Extract Interfaces** - DONE
  - `session.SessionService` interface in `bramble/session/service.go`
  - `service.WorktreeService` interface in `bramble/service/worktree.go`
  - `service.TaskRouterService` interface in `bramble/service/taskrouter.go`
  - TUI (`bramble/app/`) uses interfaces instead of concrete types
  - All tests pass

## Phase 2: Proto Definitions and gRPC Server

### 2a. Proto file: `bramble/remote/proto/bramble.proto`

Three gRPC services mirroring the Go interfaces:

```protobuf
syntax = "proto3";
package bramble;
option go_package = "github.com/bazelment/yoloswe/bramble/remote/proto";

// --- Shared types ---

message SessionInfo {
  string id = 1;
  string type = 2;           // "planner" or "builder"
  string status = 3;         // "pending","running","idle","completed","failed","stopped"
  string worktree_path = 4;
  string worktree_name = 5;
  string prompt = 6;
  string title = 7;
  string model = 8;
  string plan_file_path = 9;
  string tmux_window_name = 10;
  string runner_type = 11;
  int64 created_at_unix = 12;
  int64 started_at_unix = 13;   // 0 if nil
  int64 completed_at_unix = 14; // 0 if nil
  string error_msg = 15;
  SessionProgress progress = 16;
}

message SessionProgress {
  string current_phase = 1;
  string current_tool = 2;
  string status_line = 3;
  int32 turn_count = 4;
  double total_cost_usd = 5;
  int32 input_tokens = 6;
  int32 output_tokens = 7;
  int64 last_activity_unix = 8;
}

message OutputLine {
  int64 timestamp_unix = 1;
  string type = 2;       // OutputLineType as string
  string content = 3;
  string tool_name = 4;
  string tool_id = 5;
  string tool_state = 6; // ToolState as string
  bytes tool_input = 7;  // JSON-encoded map[string]interface{}
  bytes tool_result = 8; // JSON-encoded interface{}
  int64 start_time_unix = 9;
  int32 turn_number = 10;
  double cost_usd = 11;
  int64 duration_ms = 12;
  bool is_error = 13;
}

message Worktree {
  string path = 1;
  string branch = 2;
  string commit = 3;
  bool is_detached = 4;
}

message WorktreeStatus {
  Worktree worktree = 1;
  bool is_dirty = 2;
  int32 ahead = 3;
  int32 behind = 4;
  int32 pr_number = 5;
  string pr_url = 6;
  string pr_state = 7;
  string pr_review_status = 8;
  bool pr_is_draft = 9;
  string last_commit_msg = 10;
  int64 last_commit_time_unix = 11;
}

message PRInfo {
  string url = 1;
  string head_ref_name = 2;
  string base_ref_name = 3;
  string state = 4;
  string review_decision = 5;
  int32 number = 6;
  bool is_draft = 7;
}

message CommitInfo {
  string hash = 1;
  string subject = 2;
  string author = 3;
  int64 date_unix = 4;
}

message WorktreeContext {
  string path = 1;
  string branch = 2;
  string goal = 3;
  string parent = 4;
  bool is_dirty = 5;
  int32 ahead = 6;
  int32 behind = 7;
  repeated string changed_files = 8;
  repeated string untracked_files = 9;
  repeated CommitInfo recent_commits = 10;
  string diff_stat = 11;
  string diff_content = 12;
  int32 pr_number = 13;
  string pr_url = 14;
  string pr_state = 15;
  int64 gathered_at_unix = 16;
}

message ContextOptions {
  bool include_diff = 1;
  bool include_diff_stat = 2;
  bool include_file_list = 3;
  bool include_pr_info = 4;
  int32 include_commits = 5;
  int32 max_diff_bytes = 6;
}

message MergeOptions {
  string merge_method = 1;
  bool keep = 2;
}

message SessionMeta {
  string id = 1;
  string type = 2;
  string status = 3;
  string repo_name = 4;
  string worktree_name = 5;
  string prompt = 6;
  string title = 7;
  string model = 8;
  int64 created_at_unix = 9;
  int64 completed_at_unix = 10;
}

message StoredSession {
  string id = 1;
  string type = 2;
  string status = 3;
  string repo_name = 4;
  string worktree_path = 5;
  string worktree_name = 6;
  string prompt = 7;
  string title = 8;
  string model = 9;
  int64 created_at_unix = 10;
  int64 started_at_unix = 11;
  int64 completed_at_unix = 12;
  string error_msg = 13;
  SessionProgress progress = 14;
  repeated OutputLine output = 15;
}

// --- Events ---

message SessionEvent {
  oneof event {
    StateChangeEvent state_change = 1;
    OutputEvent output = 2;
  }
}

message StateChangeEvent {
  string session_id = 1;
  string old_status = 2;
  string new_status = 3;
}

message OutputEvent {
  string session_id = 1;
  OutputLine line = 2;
}

// --- Task Router ---

message RouteRequest {
  string prompt = 1;
  string current_wt = 2;
  string repo_name = 3;
  repeated TaskWorktreeInfo worktrees = 4;
}

message TaskWorktreeInfo {
  string name = 1;
  string path = 2;
  string goal = 3;
  string parent = 4;
  string pr_state = 5;
  string last_commit = 6;
  bool is_dirty = 7;
  bool is_ahead = 8;
  bool is_merged = 9;
}

message RouteProposal {
  string action = 1;    // "use_existing" or "create_new"
  string worktree = 2;
  string parent = 3;
  string reasoning = 4;
}

// --- Service RPCs ---

service SessionService {
  rpc StartSession(StartSessionRequest) returns (StartSessionResponse);
  rpc StopSession(StopSessionRequest) returns (StopSessionResponse);
  rpc SendFollowUp(SendFollowUpRequest) returns (SendFollowUpResponse);
  rpc CompleteSession(CompleteSessionRequest) returns (CompleteSessionResponse);
  rpc DeleteSession(DeleteSessionRequest) returns (DeleteSessionResponse);
  rpc GetSessionInfo(GetSessionInfoRequest) returns (GetSessionInfoResponse);
  rpc GetSessionsForWorktree(GetSessionsForWorktreeRequest) returns (GetSessionsForWorktreeResponse);
  rpc GetAllSessions(GetAllSessionsRequest) returns (GetAllSessionsResponse);
  rpc GetSessionOutput(GetSessionOutputRequest) returns (GetSessionOutputResponse);
  rpc CountByStatus(CountByStatusRequest) returns (CountByStatusResponse);
  rpc LoadHistorySessions(LoadHistorySessionsRequest) returns (LoadHistorySessionsResponse);
  rpc LoadSessionFromHistory(LoadSessionFromHistoryRequest) returns (LoadSessionFromHistoryResponse);
  rpc IsInTmuxMode(IsInTmuxModeRequest) returns (IsInTmuxModeResponse);
  rpc StreamEvents(StreamEventsRequest) returns (stream SessionEvent);
}

service WorktreeService {
  rpc List(ListWorktreesRequest) returns (ListWorktreesResponse);
  rpc GetGitStatus(GetGitStatusRequest) returns (GetGitStatusResponse);
  rpc FetchAllPRInfo(FetchAllPRInfoRequest) returns (FetchAllPRInfoResponse);
  rpc NewAtomic(NewAtomicRequest) returns (NewAtomicResponse);
  rpc Remove(RemoveWorktreeRequest) returns (RemoveWorktreeResponse);
  rpc Sync(SyncRequest) returns (SyncResponse);
  rpc MergePRForBranch(MergePRRequest) returns (MergePRResponse);
  rpc GatherContext(GatherContextRequest) returns (GatherContextResponse);
  rpc ResetToDefault(ResetToDefaultRequest) returns (ResetToDefaultResponse);
}

service TaskRouterService {
  rpc Route(RouteTaskRequest) returns (RouteTaskResponse);
}

// --- Request/Response messages (all RPCs) ---
// (Elided for brevity - each is a thin wrapper around the parameters)
```

### 2b. Bazel protobuf/gRPC rules

`bramble/remote/proto/BUILD.bazel`:

```python
load("@rules_proto//proto:defs.bzl", "proto_library")
load("@rules_go//proto:def.bzl", "go_proto_library")

proto_library(
    name = "bramble_proto",
    srcs = ["bramble.proto"],
    visibility = ["//visibility:public"],
)

go_proto_library(
    name = "bramble_go_proto",
    compilers = ["@rules_go//proto:go_grpc"],
    importpath = "github.com/bazelment/yoloswe/bramble/remote/proto",
    proto = ":bramble_proto",
    visibility = ["//visibility:public"],
)
```

### 2c. EventBroadcaster: `bramble/remote/broadcaster.go`

```go
type EventBroadcaster struct {
    subscribers map[int]chan interface{}
    mu          sync.RWMutex
    nextID      int
}

func (b *EventBroadcaster) Subscribe(bufSize int) (id int, ch <-chan interface{})
func (b *EventBroadcaster) Unsubscribe(id int)
func (b *EventBroadcaster) Run(ctx context.Context, source <-chan interface{})
```

- Drains the single `Manager.Events()` channel and fans out to all subscribers.
- Per-subscriber buffered channel (10000). Drop-oldest on overflow with warning log.

### 2d. gRPC server: `bramble/remote/server.go`

- `sessionServer` wraps `*session.Manager` + `*EventBroadcaster`
- `worktreeServer` wraps `service.WorktreeService`
- `taskRouterServer` wraps `service.TaskRouterService`

`StreamEvents`: subscribe to broadcaster, loop: read -> convert to proto -> `stream.Send()`. On client disconnect, unsubscribe.

### 2e. `bramble serve` subcommand in `bramble/main.go`

```
bramble serve --repo <name> --port 9090 [--addr 0.0.0.0:9090] [--yolo]
```

Starts session.Manager, worktree service, task router, EventBroadcaster, gRPC server.

## Phase 3: Client Proxies

### 3a. Session proxy: `bramble/remote/session_proxy.go`

Implements `session.SessionService`:
- Unary methods: gRPC call + convert response
- `Events()`: returns local channel. Background goroutine calls `StreamEvents`, pushes converted events. On disconnect, reconnects and re-syncs via `GetAllSessions()` + `GetSessionOutput()`.
- `IsInTmuxMode()`: always `false`

### 3b. Worktree proxy: `bramble/remote/worktree_proxy.go`

Implements `service.WorktreeService`. Each method: gRPC call + convert. `Messages()` populated from response `messages` field.

### 3c. Task router proxy: `bramble/remote/taskrouter_proxy.go`

Implements `service.TaskRouterService`. Single `Route`: gRPC call + convert.

### 3d. Proto <-> Go converters: `bramble/remote/convert.go`

Bidirectional conversion for: SessionInfo, OutputLine, SessionProgress, Worktree, WorktreeStatus, PRInfo, CommitInfo, WorktreeContext, ContextOptions, MergeOptions, RouteRequest, RouteProposal, events, SessionMeta, StoredSession.

## Phase 4: Wire Into TUI

### 4a. `--remote` flag in `bramble/main.go`

When `--remote host:port` is set:
1. Create gRPC connection
2. Create SessionProxy, WorktreeProxy, TaskRouterProxy
3. Pass to `app.NewModel` (same interface)
4. Skip local repo detection, store, manager creation

When not set: existing behavior (local Manager, wrapped in interfaces).

## Phase 5: Reconnection and Polish

- Stream disconnect -> re-subscribe + full state sync
- gRPC keepalive: 30s interval, 10s timeout
- Connection failure -> error toast in TUI, retry with backoff
- Health RPC or gRPC health checking protocol
