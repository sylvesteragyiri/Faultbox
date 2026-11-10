-- Failed Laravel notifications become issues, next to exceptions and failed
-- jobs: each failure is an event of an issue grouped by notification class,
-- channel and error type, so it gets the issue page (full message, context),
-- a status (resolve, ignore) and the usual alerts.
--
-- SQLite can't change a CHECK constraint in place, so issues and events are
-- rebuilt with 'notification' added to their category lists (the 12-step
-- procedure from https://www.sqlite.org/lang_altertable.html). Foreign keys
-- must be off while the tables are swapped, and that pragma has no effect
-- inside a transaction, hence NO TRANSACTION and the explicit BEGIN/COMMIT.
--
-- Failures stored before this migration are turned into issues by the
-- store when it opens (Store.backfillNotificationIssues), which needs the
-- same fingerprint code as ingestion.

-- +goose NO TRANSACTION
-- +goose Up
PRAGMA foreign_keys = OFF;

BEGIN IMMEDIATE;

CREATE TABLE issues_new (
    id                INTEGER PRIMARY KEY,
    project_id        INTEGER NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    fingerprint       TEXT    NOT NULL,
    category          TEXT    NOT NULL CHECK (category IN ('exception', 'message', 'queue_failure', 'notification')),
    level             TEXT    NOT NULL,
    exception_type    TEXT    NOT NULL,
    message           TEXT    NOT NULL,
    culprit           TEXT    NOT NULL DEFAULT '',
    job_name          TEXT,                      -- queue failures: the job; notifications: the notification class
    status            TEXT    NOT NULL DEFAULT 'unresolved' CHECK (status IN ('unresolved', 'resolved', 'ignored')),
    occurrence_count  INTEGER NOT NULL DEFAULT 0,
    first_seen_at     INTEGER NOT NULL,
    last_seen_at      INTEGER NOT NULL,
    resolved_at       INTEGER,
    first_release     TEXT,
    last_release      TEXT,
    sample_event_uuid TEXT    NOT NULL,
    sample_stacktrace TEXT    NOT NULL,
    sample_context    TEXT    NOT NULL,
    created_at        INTEGER NOT NULL,
    updated_at        INTEGER NOT NULL,
    UNIQUE (project_id, fingerprint)
);
INSERT INTO issues_new SELECT id, project_id, fingerprint, category, level, exception_type, message, culprit, job_name,
       status, occurrence_count, first_seen_at, last_seen_at, resolved_at, first_release, last_release,
       sample_event_uuid, sample_stacktrace, sample_context, created_at, updated_at FROM issues;
DROP TABLE issues;
ALTER TABLE issues_new RENAME TO issues;

CREATE INDEX idx_issues_project_status_last_seen ON issues (project_id, status, last_seen_at DESC);
CREATE INDEX idx_issues_project_last_seen ON issues (project_id, last_seen_at DESC);
CREATE INDEX idx_issues_project_count ON issues (project_id, occurrence_count DESC);
CREATE INDEX idx_issues_project_category ON issues (project_id, category, last_seen_at DESC);
CREATE INDEX idx_issues_status_last_seen ON issues (status, last_seen_at DESC);
CREATE INDEX idx_issues_status_count ON issues (status, occurrence_count DESC);
CREATE INDEX idx_issues_last_seen ON issues (last_seen_at DESC);
CREATE INDEX idx_issues_count ON issues (occurrence_count DESC);
CREATE INDEX idx_issues_first_seen ON issues (first_seen_at DESC);
CREATE INDEX idx_issues_project_first_seen ON issues (project_id, first_seen_at DESC);
CREATE INDEX idx_issues_fingerprint ON issues (fingerprint);
CREATE INDEX idx_issues_type ON issues (exception_type);

CREATE TABLE events_new (
    id              INTEGER PRIMARY KEY,
    project_id      INTEGER NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    issue_id        INTEGER NOT NULL REFERENCES issues (id) ON DELETE CASCADE,
    event_uuid      TEXT    NOT NULL,
    event_type      TEXT    NOT NULL CHECK (event_type IN ('exception', 'message', 'queue_failure', 'notification')),
    level           TEXT    NOT NULL,
    environment     TEXT    NOT NULL,
    release         TEXT,
    fingerprint     TEXT    NOT NULL,
    exception_type  TEXT    NOT NULL,
    message         TEXT    NOT NULL,
    stacktrace      TEXT    NOT NULL,
    request_url     TEXT,
    http_method     TEXT,
    http_status     INTEGER,
    correlation_id  TEXT,
    request_context TEXT,
    user_context    TEXT,
    event_context   TEXT,
    occurred_at     INTEGER NOT NULL,
    created_at      INTEGER NOT NULL,
    UNIQUE (project_id, event_uuid)
);
INSERT INTO events_new SELECT id, project_id, issue_id, event_uuid, event_type, level, environment, release, fingerprint,
       exception_type, message, stacktrace, request_url, http_method, http_status, correlation_id,
       request_context, user_context, event_context, occurred_at, created_at FROM events;
DROP TABLE events;
ALTER TABLE events_new RENAME TO events;

CREATE INDEX idx_events_issue_occurred ON events (issue_id, occurred_at DESC);
CREATE INDEX idx_events_project_occurred ON events (project_id, occurred_at DESC);
CREATE INDEX idx_events_project_env_occurred ON events (project_id, environment, occurred_at DESC);
CREATE INDEX idx_events_occurred ON events (occurred_at);
CREATE INDEX idx_events_correlation ON events (correlation_id) WHERE correlation_id IS NOT NULL;

-- The issue each failure belongs to; NULL until it is linked.
ALTER TABLE app_notification_failures ADD COLUMN issue_id INTEGER REFERENCES issues (id) ON DELETE SET NULL;
CREATE INDEX idx_app_notification_failures_issue ON app_notification_failures (issue_id);

COMMIT;

PRAGMA foreign_keys = ON;

-- +goose Down
PRAGMA foreign_keys = OFF;

BEGIN IMMEDIATE;

DROP INDEX idx_app_notification_failures_issue;
ALTER TABLE app_notification_failures DROP COLUMN issue_id;

DELETE FROM issues_fts WHERE rowid IN (SELECT id FROM issues WHERE category = 'notification');
DELETE FROM issue_environments WHERE issue_id IN (SELECT id FROM issues WHERE category = 'notification');
DELETE FROM issue_releases WHERE issue_id IN (SELECT id FROM issues WHERE category = 'notification');
DELETE FROM issue_daily_counts WHERE issue_id IN (SELECT id FROM issues WHERE category = 'notification');
UPDATE notification_history SET issue_id = NULL WHERE issue_id IN (SELECT id FROM issues WHERE category = 'notification');
DELETE FROM events WHERE event_type = 'notification';
DELETE FROM issues WHERE category = 'notification';

CREATE TABLE events_old (
    id              INTEGER PRIMARY KEY,
    project_id      INTEGER NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    issue_id        INTEGER NOT NULL REFERENCES issues (id) ON DELETE CASCADE,
    event_uuid      TEXT    NOT NULL,
    event_type      TEXT    NOT NULL CHECK (event_type IN ('exception', 'message', 'queue_failure')),
    level           TEXT    NOT NULL,
    environment     TEXT    NOT NULL,
    release         TEXT,
    fingerprint     TEXT    NOT NULL,
    exception_type  TEXT    NOT NULL,
    message         TEXT    NOT NULL,
    stacktrace      TEXT    NOT NULL,
    request_url     TEXT,
    http_method     TEXT,
    http_status     INTEGER,
    correlation_id  TEXT,
    request_context TEXT,
    user_context    TEXT,
    event_context   TEXT,
    occurred_at     INTEGER NOT NULL,
    created_at      INTEGER NOT NULL,
    UNIQUE (project_id, event_uuid)
);
INSERT INTO events_old SELECT * FROM events;
DROP TABLE events;
ALTER TABLE events_old RENAME TO events;
CREATE INDEX idx_events_issue_occurred ON events (issue_id, occurred_at DESC);
CREATE INDEX idx_events_project_occurred ON events (project_id, occurred_at DESC);
CREATE INDEX idx_events_project_env_occurred ON events (project_id, environment, occurred_at DESC);
CREATE INDEX idx_events_occurred ON events (occurred_at);
CREATE INDEX idx_events_correlation ON events (correlation_id) WHERE correlation_id IS NOT NULL;

CREATE TABLE issues_old (
    id                INTEGER PRIMARY KEY,
    project_id        INTEGER NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    fingerprint       TEXT    NOT NULL,
    category          TEXT    NOT NULL CHECK (category IN ('exception', 'message', 'queue_failure')),
    level             TEXT    NOT NULL,
    exception_type    TEXT    NOT NULL,
    message           TEXT    NOT NULL,
    culprit           TEXT    NOT NULL DEFAULT '',
    job_name          TEXT,
    status            TEXT    NOT NULL DEFAULT 'unresolved' CHECK (status IN ('unresolved', 'resolved', 'ignored')),
    occurrence_count  INTEGER NOT NULL DEFAULT 0,
    first_seen_at     INTEGER NOT NULL,
    last_seen_at      INTEGER NOT NULL,
    resolved_at       INTEGER,
    first_release     TEXT,
    last_release      TEXT,
    sample_event_uuid TEXT    NOT NULL,
    sample_stacktrace TEXT    NOT NULL,
    sample_context    TEXT    NOT NULL,
    created_at        INTEGER NOT NULL,
    updated_at        INTEGER NOT NULL,
    UNIQUE (project_id, fingerprint)
);
INSERT INTO issues_old SELECT * FROM issues;
DROP TABLE issues;
ALTER TABLE issues_old RENAME TO issues;
CREATE INDEX idx_issues_project_status_last_seen ON issues (project_id, status, last_seen_at DESC);
CREATE INDEX idx_issues_project_last_seen ON issues (project_id, last_seen_at DESC);
CREATE INDEX idx_issues_project_count ON issues (project_id, occurrence_count DESC);
CREATE INDEX idx_issues_project_category ON issues (project_id, category, last_seen_at DESC);
CREATE INDEX idx_issues_status_last_seen ON issues (status, last_seen_at DESC);
CREATE INDEX idx_issues_status_count ON issues (status, occurrence_count DESC);
CREATE INDEX idx_issues_last_seen ON issues (last_seen_at DESC);
CREATE INDEX idx_issues_count ON issues (occurrence_count DESC);
CREATE INDEX idx_issues_first_seen ON issues (first_seen_at DESC);
CREATE INDEX idx_issues_project_first_seen ON issues (project_id, first_seen_at DESC);
CREATE INDEX idx_issues_fingerprint ON issues (fingerprint);
CREATE INDEX idx_issues_type ON issues (exception_type);

COMMIT;

PRAGMA foreign_keys = ON;
