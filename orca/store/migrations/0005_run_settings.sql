CREATE TABLE IF NOT EXISTS run_settings (
    run_id TEXT PRIMARY KEY,
    concurrency_cap INTEGER NOT NULL,
    stall_timeout_ns INTEGER NOT NULL,
    paused INTEGER NOT NULL DEFAULT 0
);
