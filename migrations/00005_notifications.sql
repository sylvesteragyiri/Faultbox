-- M6 notifications: channels (email and webhook) and the delivery history
-- that also serves as the durable delivery queue. Timestamps are UTC Unix
-- milliseconds (D18). Secrets are sealed with AES-256-GCM (D17).

-- +goose Up
CREATE TABLE notification_channels (
    id                       INTEGER PRIMARY KEY,
    name                     TEXT    NOT NULL,
    kind                     TEXT    NOT NULL CHECK (kind IN ('email', 'webhook')),
    enabled                  INTEGER NOT NULL DEFAULT 1,
    -- Non-secret settings as JSON. email: host, port, security, username,
    -- from, to. webhook: url.
    config                   TEXT    NOT NULL,
    -- Sealed secret: the SMTP password or the webhook signing secret.
    secret                   BLOB,
    project_id               INTEGER REFERENCES projects (id) ON DELETE CASCADE, -- NULL: every project
    environments             TEXT    NOT NULL DEFAULT '',                      -- comma-separated; '' = every environment
    on_new_issue             INTEGER NOT NULL DEFAULT 1,
    on_regression            INTEGER NOT NULL DEFAULT 1,
    on_queue_failure         INTEGER NOT NULL DEFAULT 1,
    threshold_count          INTEGER NOT NULL DEFAULT 0,                       -- 0 = off
    threshold_window_minutes INTEGER NOT NULL DEFAULT 60,
    cooldown_minutes         INTEGER NOT NULL DEFAULT 30,
    created_at               INTEGER NOT NULL,
    updated_at               INTEGER NOT NULL
);

CREATE TABLE notification_history (
    id              INTEGER PRIMARY KEY,
    channel_id      INTEGER NOT NULL REFERENCES notification_channels (id) ON DELETE CASCADE,
    issue_id        INTEGER REFERENCES issues (id) ON DELETE SET NULL,
    project_id      INTEGER REFERENCES projects (id) ON DELETE SET NULL,
    trigger         TEXT    NOT NULL CHECK (trigger IN ('new_issue', 'regression', 'queue_failure', 'threshold', 'test')),
    status          TEXT    NOT NULL CHECK (status IN ('pending', 'sent', 'failed')),
    subject         TEXT    NOT NULL,
    payload         TEXT    NOT NULL,            -- JSON message, rendered when the alert was raised
    attempts        INTEGER NOT NULL DEFAULT 0,
    next_attempt_at INTEGER,                     -- set while pending
    last_error      TEXT,
    created_at      INTEGER NOT NULL,
    sent_at         INTEGER
);

CREATE INDEX idx_notification_history_created ON notification_history (created_at DESC);
CREATE INDEX idx_notification_history_channel ON notification_history (channel_id, created_at DESC);
CREATE INDEX idx_notification_history_due ON notification_history (next_attempt_at) WHERE status = 'pending';

-- +goose Down
DROP TABLE notification_history;
DROP TABLE notification_channels;
