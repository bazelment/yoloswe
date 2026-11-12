CREATE TABLE IF NOT EXISTS runs (
    id TEXT PRIMARY KEY,
    objective TEXT NOT NULL DEFAULT '',
    plan_ref TEXT NOT NULL DEFAULT '',
    default_recipe TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    lease_holder_id TEXT NOT NULL DEFAULT '',
    lease_acquired_at TEXT NOT NULL DEFAULT '',
    lease_heartbeat_at TEXT NOT NULL DEFAULT '',
    lease_expires_at TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS backlog_items (
    run_id TEXT NOT NULL,
    id TEXT NOT NULL,
    title TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    acceptance TEXT NOT NULL DEFAULT '',
    priority TEXT NOT NULL,
    priority_rank INTEGER NOT NULL,
    sort_order INTEGER NOT NULL DEFAULT 0,
    status TEXT NOT NULL,
    lane_id TEXT NOT NULL DEFAULT '',
    recipe_name TEXT NOT NULL DEFAULT '',
    depends_on_json TEXT NOT NULL DEFAULT '[]',
    tags_json TEXT NOT NULL DEFAULT '[]',
    history_json TEXT NOT NULL DEFAULT '[]',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (run_id, id)
);

CREATE INDEX IF NOT EXISTS idx_backlog_items_run_sort
    ON backlog_items (run_id, priority_rank, sort_order, created_at, id);

CREATE TABLE IF NOT EXISTS lanes (
    run_id TEXT NOT NULL,
    id TEXT NOT NULL,
    item_id TEXT NOT NULL DEFAULT '',
    title TEXT NOT NULL DEFAULT '',
    branch TEXT NOT NULL DEFAULT '',
    base_branch TEXT NOT NULL DEFAULT '',
    worktree TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL,
    phase TEXT NOT NULL DEFAULT '',
    round INTEGER NOT NULL DEFAULT 0,
    attempt INTEGER NOT NULL DEFAULT 0,
    recipe_name TEXT NOT NULL DEFAULT '',
    recipe_version INTEGER NOT NULL DEFAULT 0,
    session_id TEXT NOT NULL DEFAULT '',
    terminal_id TEXT NOT NULL DEFAULT '',
    sessions_json TEXT NOT NULL DEFAULT '{}',
    terminals_json TEXT NOT NULL DEFAULT '{}',
    pr INTEGER NOT NULL DEFAULT 0,
    pr_head TEXT NOT NULL DEFAULT '',
    approval_sha TEXT NOT NULL DEFAULT '',
    merge_sha TEXT NOT NULL DEFAULT '',
    checks TEXT NOT NULL DEFAULT '',
    last_event_id TEXT NOT NULL DEFAULT '',
    last_event_at TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (run_id, id)
);

CREATE INDEX IF NOT EXISTS idx_lanes_run_status
    ON lanes (run_id, status, id);

CREATE TABLE IF NOT EXISTS directives (
    run_id TEXT NOT NULL,
    id TEXT NOT NULL,
    kind TEXT NOT NULL,
    source TEXT NOT NULL,
    scope TEXT NOT NULL DEFAULT '',
    text TEXT NOT NULL DEFAULT '',
    ref TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL,
    expires_at TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    PRIMARY KEY (run_id, id)
);

CREATE INDEX IF NOT EXISTS idx_directives_run_status
    ON directives (run_id, status, created_at, id);

CREATE TABLE IF NOT EXISTS decisions (
    run_id TEXT NOT NULL,
    seq INTEGER NOT NULL,
    id TEXT NOT NULL,
    category TEXT NOT NULL,
    actor TEXT NOT NULL DEFAULT '',
    thread_id TEXT NOT NULL DEFAULT '',
    turn_id TEXT NOT NULL DEFAULT '',
    lane_id TEXT NOT NULL DEFAULT '',
    tool TEXT NOT NULL,
    args_json TEXT NOT NULL DEFAULT '',
    reason TEXT NOT NULL DEFAULT '',
    result TEXT NOT NULL DEFAULT '',
    vetted INTEGER NOT NULL DEFAULT 0,
    input_tokens INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    cost_usd REAL NOT NULL DEFAULT 0,
    latency_ms INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL,
    PRIMARY KEY (run_id, seq)
);

CREATE INDEX IF NOT EXISTS idx_decisions_run_seq
    ON decisions (run_id, seq);

CREATE TABLE IF NOT EXISTS events (
    run_id TEXT NOT NULL,
    seq INTEGER NOT NULL,
    id TEXT NOT NULL,
    kind TEXT NOT NULL,
    class TEXT NOT NULL DEFAULT '',
    lane_id TEXT NOT NULL DEFAULT '',
    source TEXT NOT NULL DEFAULT '',
    summary TEXT NOT NULL DEFAULT '',
    payload_json TEXT NOT NULL DEFAULT '',
    urgent INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL,
    PRIMARY KEY (run_id, seq)
);

CREATE INDEX IF NOT EXISTS idx_events_run_seq
    ON events (run_id, seq);

CREATE TABLE IF NOT EXISTS event_cursors (
    run_id TEXT NOT NULL,
    consumer TEXT NOT NULL,
    seq INTEGER NOT NULL DEFAULT 0,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (run_id, consumer)
);

CREATE TABLE IF NOT EXISTS threads (
    id TEXT PRIMARY KEY,
    run_id TEXT NOT NULL,
    role TEXT NOT NULL,
    goal TEXT NOT NULL DEFAULT '',
    parent_thread_id TEXT NOT NULL DEFAULT '',
    forked_at_turn_id TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL,
    queued_steers_json TEXT NOT NULL DEFAULT '[]',
    input_tokens INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    cost_usd REAL NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_threads_run_role
    ON threads (run_id, role, status);

CREATE TABLE IF NOT EXISTS turns (
    thread_id TEXT NOT NULL,
    id TEXT NOT NULL,
    run_id TEXT NOT NULL,
    seq INTEGER NOT NULL,
    status TEXT NOT NULL,
    input TEXT NOT NULL DEFAULT '',
    error TEXT NOT NULL DEFAULT '',
    steers_json TEXT NOT NULL DEFAULT '[]',
    input_tokens INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    cost_usd REAL NOT NULL DEFAULT 0,
    started_at TEXT NOT NULL,
    completed_at TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (thread_id, id),
    UNIQUE (thread_id, seq)
);

CREATE INDEX IF NOT EXISTS idx_turns_thread_seq
    ON turns (thread_id, seq);

CREATE TABLE IF NOT EXISTS thread_items (
    thread_id TEXT NOT NULL,
    seq INTEGER NOT NULL,
    id TEXT NOT NULL,
    turn_id TEXT NOT NULL,
    kind TEXT NOT NULL,
    name TEXT NOT NULL DEFAULT '',
    call_id TEXT NOT NULL DEFAULT '',
    content TEXT NOT NULL DEFAULT '',
    payload_json TEXT NOT NULL DEFAULT '',
    is_error INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL,
    PRIMARY KEY (thread_id, seq)
);

CREATE INDEX IF NOT EXISTS idx_thread_items_thread_turn
    ON thread_items (thread_id, turn_id, seq);

CREATE TABLE IF NOT EXISTS recipes (
    run_id TEXT NOT NULL,
    name TEXT NOT NULL,
    version INTEGER NOT NULL,
    source TEXT NOT NULL DEFAULT '',
    base_recipe TEXT NOT NULL DEFAULT '',
    base_version INTEGER NOT NULL DEFAULT 0,
    phases_json TEXT NOT NULL DEFAULT '[]',
    transitions_json TEXT NOT NULL DEFAULT '[]',
    unmapped_json TEXT NOT NULL DEFAULT '[]',
    merge_policy_json TEXT NOT NULL DEFAULT '{}',
    limits_json TEXT NOT NULL DEFAULT '{}',
    created_at TEXT NOT NULL,
    PRIMARY KEY (run_id, name, version)
);
