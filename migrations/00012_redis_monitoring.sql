-- Named Redis instances and compact memory samples for dashboard history.

-- +goose Up
CREATE TABLE redis_targets (
    id INTEGER PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    endpoint BLOB NOT NULL,
    enabled INTEGER NOT NULL DEFAULT 1,
    alert_enabled INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    last_checked_at INTEGER,
    last_error TEXT NOT NULL DEFAULT ''
);

CREATE TABLE redis_samples (
    id INTEGER PRIMARY KEY,
    target_id INTEGER NOT NULL REFERENCES redis_targets (id) ON DELETE CASCADE,
    sampled_at INTEGER NOT NULL,
    used_memory INTEGER NOT NULL,
    maxmemory INTEGER NOT NULL,
    connected_clients INTEGER NOT NULL,
    key_count INTEGER NOT NULL,
    uptime_seconds INTEGER NOT NULL
);

CREATE INDEX idx_redis_samples_target_time ON redis_samples (target_id, sampled_at DESC);

-- +goose Down
DROP TABLE redis_samples;
DROP TABLE redis_targets;
