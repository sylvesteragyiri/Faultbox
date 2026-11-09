-- M2 ingestion: issues (error groups), events (occurrences), queue failures,
-- and the per-issue rollups that survive event retention (D12, D13).
-- Timestamps are UTC Unix milliseconds (D18).

-- +goose Up
CREATE TABLE issues (
    id                INTEGER PRIMARY KEY,
    project_id        INTEGER NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    fingerprint       TEXT    NOT NULL,
    category          TEXT    NOT NULL CHECK (category IN ('exception', 'message', 'queue_failure')),
    level             TEXT    NOT NULL,
    exception_type    TEXT    NOT NULL,
    message           TEXT    NOT NULL,          -- from the first event, truncated
    culprit           TEXT    NOT NULL DEFAULT '', -- top in-app frame, e.g. App\Http\Controllers\OrderController::store
    job_name          TEXT,                      -- queue failures only
    status            TEXT    NOT NULL DEFAULT 'unresolved' CHECK (status IN ('unresolved', 'resolved', 'ignored')),
    occurrence_count  INTEGER NOT NULL DEFAULT 0,
    first_seen_at     INTEGER NOT NULL,
    last_seen_at      INTEGER NOT NULL,
    resolved_at       INTEGER,
    first_release     TEXT,
    last_release      TEXT,
    sample_event_uuid TEXT    NOT NULL,
    sample_stacktrace TEXT    NOT NULL,          -- JSON, kept after the event expires (D13)
    sample_context    TEXT    NOT NULL,          -- JSON: request, user, context, job
    created_at        INTEGER NOT NULL,
    updated_at        INTEGER NOT NULL,
    UNIQUE (project_id, fingerprint)
);

CREATE INDEX idx_issues_project_status_last_seen ON issues (project_id, status, last_seen_at DESC);
CREATE INDEX idx_issues_project_last_seen ON issues (project_id, last_seen_at DESC);
CREATE INDEX idx_issues_project_count ON issues (project_id, occurrence_count DESC);
CREATE INDEX idx_issues_project_category ON issues (project_id, category, last_seen_at DESC);

CREATE TABLE events (
    id              INTEGER PRIMARY KEY,
    project_id      INTEGER NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    issue_id        INTEGER NOT NULL REFERENCES issues (id) ON DELETE CASCADE,
    event_uuid      TEXT    NOT NULL,            -- client-generated, for idempotency (D11)
    event_type      TEXT    NOT NULL CHECK (event_type IN ('exception', 'message', 'queue_failure')),
    level           TEXT    NOT NULL,
    environment     TEXT    NOT NULL,
    release         TEXT,
    fingerprint     TEXT    NOT NULL,
    exception_type  TEXT    NOT NULL,
    message         TEXT    NOT NULL,
    stacktrace      TEXT    NOT NULL,            -- JSON {"frames": [...], "frames_omitted": n}
    request_url     TEXT,
    http_method     TEXT,
    http_status     INTEGER,
    correlation_id  TEXT,
    request_context TEXT,                        -- JSON
    user_context    TEXT,                        -- JSON
    event_context   TEXT,                        -- JSON
    occurred_at     INTEGER NOT NULL,
    created_at      INTEGER NOT NULL,
    UNIQUE (project_id, event_uuid)
);

CREATE INDEX idx_events_issue_occurred ON events (issue_id, occurred_at DESC);
CREATE INDEX idx_events_project_occurred ON events (project_id, occurred_at DESC);
CREATE INDEX idx_events_project_env_occurred ON events (project_id, environment, occurred_at DESC);
CREATE INDEX idx_events_occurred ON events (occurred_at);  -- retention sweeps (M7)
CREATE INDEX idx_events_correlation ON events (correlation_id) WHERE correlation_id IS NOT NULL;

CREATE TABLE queue_failures (
    id               INTEGER PRIMARY KEY,
    event_id         INTEGER NOT NULL UNIQUE REFERENCES events (id) ON DELETE CASCADE,
    project_id       INTEGER NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    issue_id         INTEGER NOT NULL REFERENCES issues (id) ON DELETE CASCADE,
    kind             TEXT    NOT NULL CHECK (kind IN ('terminal', 'manual', 'attempt')),
    job_name         TEXT    NOT NULL,
    job_id           TEXT,
    queue_name       TEXT,
    connection_name  TEXT,
    attempts         INTEGER,
    max_tries        INTEGER,
    batch_id         TEXT,
    payload_metadata TEXT,                       -- JSON, sanitized
    failed_at        INTEGER NOT NULL
);

CREATE INDEX idx_queue_failures_project_failed ON queue_failures (project_id, failed_at DESC);
CREATE INDEX idx_queue_failures_project_queue ON queue_failures (project_id, queue_name, failed_at DESC);
CREATE INDEX idx_queue_failures_project_job ON queue_failures (project_id, job_name, failed_at DESC);
CREATE INDEX idx_queue_failures_issue ON queue_failures (issue_id, failed_at DESC);

CREATE TABLE issue_releases (
    issue_id         INTEGER NOT NULL REFERENCES issues (id) ON DELETE CASCADE,
    release          TEXT    NOT NULL,
    first_seen_at    INTEGER NOT NULL,
    last_seen_at     INTEGER NOT NULL,
    occurrence_count INTEGER NOT NULL,
    PRIMARY KEY (issue_id, release)
) WITHOUT ROWID;

CREATE TABLE issue_daily_counts (
    issue_id INTEGER NOT NULL REFERENCES issues (id) ON DELETE CASCADE,
    day      INTEGER NOT NULL,                   -- days since the Unix epoch, UTC
    count    INTEGER NOT NULL,
    PRIMARY KEY (issue_id, day)
) WITHOUT ROWID;

-- +goose Down
DROP TABLE issue_daily_counts;
DROP TABLE issue_releases;
DROP TABLE queue_failures;
DROP TABLE events;
DROP TABLE issues;
