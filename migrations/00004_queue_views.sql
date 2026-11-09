-- M5 queue monitoring: the environment of each failed job, so the queue views
-- can filter and break down by it without a join through events, plus the
-- indexes the failed-job list needs for every filter. Projects get an optional
-- link to the application's own queue tool (Horizon, for example).

-- +goose Up
ALTER TABLE queue_failures ADD COLUMN environment TEXT NOT NULL DEFAULT '';

UPDATE queue_failures
   SET environment = COALESCE((SELECT e.environment FROM events e WHERE e.id = queue_failures.event_id), '');

-- Failed-job list filters, with and without a project.
CREATE INDEX idx_queue_failures_queue ON queue_failures (queue_name, failed_at DESC);
CREATE INDEX idx_queue_failures_connection ON queue_failures (connection_name, failed_at DESC);
CREATE INDEX idx_queue_failures_job ON queue_failures (job_name, failed_at DESC);
CREATE INDEX idx_queue_failures_env ON queue_failures (environment, failed_at DESC);
CREATE INDEX idx_queue_failures_project_env ON queue_failures (project_id, environment, failed_at DESC);
CREATE INDEX idx_queue_failures_project_connection ON queue_failures (project_id, connection_name, failed_at DESC);

ALTER TABLE projects ADD COLUMN queue_dashboard_url TEXT NOT NULL DEFAULT '';

-- Daily failed-job counts per job, queue and connection, maintained by the
-- ingest transaction. The queue dashboard reads this instead of scanning
-- queue_failures, so its cost depends on how many distinct jobs fail, not
-- on how often.
CREATE TABLE queue_daily_counts (
    day             INTEGER NOT NULL,            -- days since the Unix epoch, UTC
    project_id      INTEGER NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    job_name        TEXT    NOT NULL,
    queue_name      TEXT    NOT NULL DEFAULT '',
    connection_name TEXT    NOT NULL DEFAULT '',
    failures        INTEGER NOT NULL DEFAULT 0,  -- terminal and manual
    attempts        INTEGER NOT NULL DEFAULT 0,  -- failed retries
    last_failed_at  INTEGER NOT NULL,
    PRIMARY KEY (day, project_id, job_name, queue_name, connection_name)
) WITHOUT ROWID;

CREATE INDEX idx_queue_daily_counts_project ON queue_daily_counts (project_id, day);

INSERT INTO queue_daily_counts (day, project_id, job_name, queue_name, connection_name, failures, attempts, last_failed_at)
SELECT failed_at / 86400000, project_id, job_name, COALESCE(queue_name, ''), COALESCE(connection_name, ''),
       SUM(kind != 'attempt'), SUM(kind = 'attempt'), MAX(failed_at)
  FROM queue_failures
 GROUP BY 1, 2, 3, 4, 5;

-- +goose Down
DROP TABLE queue_daily_counts;
ALTER TABLE projects DROP COLUMN queue_dashboard_url;
DROP INDEX idx_queue_failures_project_connection;
DROP INDEX idx_queue_failures_project_env;
DROP INDEX idx_queue_failures_env;
DROP INDEX idx_queue_failures_job;
DROP INDEX idx_queue_failures_connection;
DROP INDEX idx_queue_failures_queue;
ALTER TABLE queue_failures DROP COLUMN environment;
