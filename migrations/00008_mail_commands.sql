-- Mail sent directly by the monitored apps (Mailables and Mail::raw; mail
-- sent by notifications is counted under app notifications instead) and the
-- artisan commands they run. Both arrive in telemetry batches
-- (POST /api/v1/telemetry), deduplicated by batch ID (telemetry_batches).

-- +goose Up
CREATE TABLE app_mail_daily_counts (
    day         INTEGER NOT NULL,                -- days since the Unix epoch, UTC
    project_id  INTEGER NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    environment TEXT    NOT NULL,
    mailable    TEXT    NOT NULL,                -- class, or "raw" for Mail::raw and closures
    mailer      TEXT    NOT NULL,                -- smtp, ses, postmark, log, ...
    sent        INTEGER NOT NULL DEFAULT 0,
    failed      INTEGER NOT NULL DEFAULT 0,
    last_at     INTEGER NOT NULL,
    PRIMARY KEY (day, project_id, environment, mailable, mailer)
) WITHOUT ROWID;

CREATE INDEX idx_app_mail_daily_counts_project ON app_mail_daily_counts (project_id, day);

CREATE TABLE app_mail_failures (
    id             INTEGER PRIMARY KEY,
    project_id     INTEGER NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    uuid           TEXT    NOT NULL,
    environment    TEXT    NOT NULL,
    release        TEXT,
    mailable       TEXT    NOT NULL,
    mailer         TEXT    NOT NULL,
    recipients     INTEGER NOT NULL DEFAULT 0,   -- a count only, never addresses
    error_type     TEXT    NOT NULL,
    error_message  TEXT    NOT NULL,
    correlation_id TEXT,
    occurred_at    INTEGER NOT NULL,
    UNIQUE (project_id, uuid)
);

CREATE INDEX idx_app_mail_failures_project ON app_mail_failures (project_id, occurred_at DESC);
CREATE INDEX idx_app_mail_failures_occurred ON app_mail_failures (occurred_at DESC);

-- Every artisan command run (minus the long-running and noisy ones the SDK
-- ignores), plus a daily rollup so the dashboard never scans all runs.
CREATE TABLE app_command_runs (
    id            INTEGER PRIMARY KEY,
    project_id    INTEGER NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    uuid          TEXT    NOT NULL,
    environment   TEXT    NOT NULL,
    release       TEXT,
    command       TEXT    NOT NULL,
    exit_code     INTEGER NOT NULL,
    duration_ms   INTEGER NOT NULL,
    failed        INTEGER NOT NULL,              -- exit_code != 0
    error_type    TEXT,
    error_message TEXT,
    started_at    INTEGER NOT NULL,
    UNIQUE (project_id, uuid)
);

CREATE INDEX idx_app_command_runs_project ON app_command_runs (project_id, started_at DESC);
CREATE INDEX idx_app_command_runs_started ON app_command_runs (started_at DESC);
CREATE INDEX idx_app_command_runs_failed ON app_command_runs (failed, started_at DESC);

CREATE TABLE app_command_daily_counts (
    day         INTEGER NOT NULL,
    project_id  INTEGER NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    environment TEXT    NOT NULL,
    command     TEXT    NOT NULL,
    runs        INTEGER NOT NULL DEFAULT 0,
    failed      INTEGER NOT NULL DEFAULT 0,
    total_ms    INTEGER NOT NULL DEFAULT 0,
    max_ms      INTEGER NOT NULL DEFAULT 0,
    last_at     INTEGER NOT NULL,
    last_exit   INTEGER NOT NULL,
    PRIMARY KEY (day, project_id, environment, command)
) WITHOUT ROWID;

CREATE INDEX idx_app_command_daily_counts_project ON app_command_daily_counts (project_id, day);

-- +goose Down
DROP TABLE app_command_daily_counts;
DROP TABLE app_command_runs;
DROP TABLE app_mail_failures;
DROP TABLE app_mail_daily_counts;
