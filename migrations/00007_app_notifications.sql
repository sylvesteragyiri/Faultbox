-- Laravel notifications sent by the monitored apps (upcoming-features P2).
-- Not to be confused with notification_channels / notification_history,
-- which are Faultbox's own alerts (M6).
--
-- Sent notifications are counted in a daily rollup; failures are also kept
-- one per row, so the dashboard can show why they failed. Batches carry an
-- ID the SDK reuses on retry, so a retried batch is never counted twice.

-- +goose Up
CREATE TABLE app_notification_daily_counts (
    day          INTEGER NOT NULL,               -- days since the Unix epoch, UTC
    project_id   INTEGER NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    environment  TEXT    NOT NULL,
    notification TEXT    NOT NULL,               -- class, e.g. App\Notifications\InvoicePaid
    channel      TEXT    NOT NULL,               -- mail, database, broadcast, slack, vonage, ...
    sent         INTEGER NOT NULL DEFAULT 0,
    failed       INTEGER NOT NULL DEFAULT 0,
    last_at      INTEGER NOT NULL,
    PRIMARY KEY (day, project_id, environment, notification, channel)
) WITHOUT ROWID;

CREATE INDEX idx_app_notification_daily_counts_project ON app_notification_daily_counts (project_id, day);

CREATE TABLE app_notification_failures (
    id             INTEGER PRIMARY KEY,
    project_id     INTEGER NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    uuid           TEXT    NOT NULL,
    environment    TEXT    NOT NULL,
    release        TEXT,
    notification   TEXT    NOT NULL,
    channel        TEXT    NOT NULL,
    notifiable     TEXT    NOT NULL,             -- class only, never the record's attributes
    error_type     TEXT    NOT NULL,
    error_message  TEXT    NOT NULL,
    correlation_id TEXT,
    occurred_at    INTEGER NOT NULL,
    UNIQUE (project_id, uuid)
);

CREATE INDEX idx_app_notification_failures_project ON app_notification_failures (project_id, occurred_at DESC);
CREATE INDEX idx_app_notification_failures_occurred ON app_notification_failures (occurred_at DESC);

CREATE TABLE telemetry_batches (
    project_id  INTEGER NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    batch_id    TEXT    NOT NULL,
    received_at INTEGER NOT NULL,
    PRIMARY KEY (project_id, batch_id)
) WITHOUT ROWID;

CREATE INDEX idx_telemetry_batches_received ON telemetry_batches (received_at);

-- +goose Down
DROP TABLE telemetry_batches;
DROP TABLE app_notification_failures;
DROP TABLE app_notification_daily_counts;
