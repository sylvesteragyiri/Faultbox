-- Cache use by the monitored apps: hits, misses, writes, deletes and failed
-- writes or deletes, per store and key pattern. Counters only, never a row
-- per operation: the SDK totals them per flush and reduces keys to patterns
-- (users:42:profile becomes users:*:profile). They arrive in telemetry
-- batches (POST /api/v1/telemetry), deduplicated by batch ID.

-- +goose Up
CREATE TABLE app_cache_daily_counts (
    day         INTEGER NOT NULL,                -- days since the Unix epoch, UTC
    project_id  INTEGER NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    environment TEXT    NOT NULL,
    store       TEXT    NOT NULL,                -- the cache store's name: redis, file, array
    key         TEXT    NOT NULL,                -- a key pattern, or (other) past the daily cap
    hits        INTEGER NOT NULL DEFAULT 0,
    misses      INTEGER NOT NULL DEFAULT 0,
    writes      INTEGER NOT NULL DEFAULT 0,
    deletes     INTEGER NOT NULL DEFAULT 0,
    failures    INTEGER NOT NULL DEFAULT 0,      -- failed writes and deletes
    last_at     INTEGER NOT NULL,
    PRIMARY KEY (day, project_id, environment, store, key)
) WITHOUT ROWID;

CREATE INDEX idx_app_cache_daily_counts_project ON app_cache_daily_counts (project_id, day);

-- +goose Down
DROP TABLE app_cache_daily_counts;
