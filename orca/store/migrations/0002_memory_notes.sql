CREATE TABLE IF NOT EXISTS memories (
 id TEXT PRIMARY KEY, name TEXT NOT NULL, description TEXT NOT NULL DEFAULT '',
 type TEXT NOT NULL, scope TEXT NOT NULL, run_id TEXT NOT NULL DEFAULT '',
 source TEXT NOT NULL DEFAULT '', body TEXT NOT NULL, proposed_by TEXT NOT NULL,
 created_at TEXT NOT NULL, updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_memories_scope_run ON memories(scope, run_id, id);
CREATE TABLE IF NOT EXISTS run_notes (
 id TEXT PRIMARY KEY, run_id TEXT NOT NULL, task_id TEXT NOT NULL DEFAULT '',
 phase TEXT NOT NULL DEFAULT '', text TEXT NOT NULL, created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_run_notes_run_task ON run_notes(run_id, task_id, created_at, id);

CREATE TABLE IF NOT EXISTS memory_seed_state (
 id INTEGER PRIMARY KEY CHECK (id = 1)
);
