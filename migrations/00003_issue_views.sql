-- M4 issue views: environment rollup, full-text search and the indexes the
-- issue list, overview and detail pages need. Every list query must be
-- answerable from an index with a LIMIT, so dashboard reads stay cheap and
-- never hold up ingestion.

-- +goose Up

-- Per-issue environment breakdown. Like issue_releases (D12), it survives
-- event retention and makes the environment filter an indexed lookup.
CREATE TABLE issue_environments (
    issue_id         INTEGER NOT NULL REFERENCES issues (id) ON DELETE CASCADE,
    environment      TEXT    NOT NULL,
    first_seen_at    INTEGER NOT NULL,
    last_seen_at     INTEGER NOT NULL,
    occurrence_count INTEGER NOT NULL,
    PRIMARY KEY (issue_id, environment)
) WITHOUT ROWID;

CREATE INDEX idx_issue_environments_env ON issue_environments (environment, issue_id);
CREATE INDEX idx_issue_releases_release ON issue_releases (release, issue_id);

INSERT INTO issue_environments (issue_id, environment, first_seen_at, last_seen_at, occurrence_count)
SELECT issue_id, environment, MIN(occurred_at), MAX(occurred_at), COUNT(*)
  FROM events GROUP BY issue_id, environment;

-- Issue list orderings without a project filter.
CREATE INDEX idx_issues_status_last_seen ON issues (status, last_seen_at DESC);
CREATE INDEX idx_issues_status_count ON issues (status, occurrence_count DESC);
CREATE INDEX idx_issues_last_seen ON issues (last_seen_at DESC);
CREATE INDEX idx_issues_count ON issues (occurrence_count DESC);
CREATE INDEX idx_issues_first_seen ON issues (first_seen_at DESC);
CREATE INDEX idx_issues_project_first_seen ON issues (project_id, first_seen_at DESC);
CREATE INDEX idx_issues_fingerprint ON issues (fingerprint);
CREATE INDEX idx_issues_type ON issues (exception_type);

-- Overview: latest failed jobs across projects.
CREATE INDEX idx_queue_failures_failed ON queue_failures (failed_at DESC);

-- Overview trend: sum per day across issues.
CREATE INDEX idx_issue_daily_counts_day ON issue_daily_counts (day, issue_id);

-- Substring search over issue text (trigram tokenizer: "NotFound" matches
-- ModelNotFoundException). Maintained by the ingest transaction; rowid = issues.id.
CREATE VIRTUAL TABLE issues_fts USING fts5(
    exception_type, message, culprit, job_name, request_path,
    tokenize = 'trigram'
);

INSERT INTO issues_fts (rowid, exception_type, message, culprit, job_name, request_path)
SELECT i.id, i.exception_type, i.message, i.culprit, COALESCE(i.job_name, ''),
       COALESCE(substr(u.url, 1, instr(u.url || '?', '?') - 1), '')  -- path only, like ingest
  FROM issues i, (SELECT json_extract(i2.sample_context, '$.request.url') AS url, i2.id AS id FROM issues i2) u
 WHERE u.id = i.id;

-- +goose Down
DROP TABLE issues_fts;
DROP INDEX idx_issue_daily_counts_day;
DROP INDEX idx_queue_failures_failed;
DROP INDEX idx_issues_project_first_seen;
DROP INDEX idx_issues_first_seen;
DROP INDEX idx_issues_type;
DROP INDEX idx_issues_fingerprint;
DROP INDEX idx_issues_count;
DROP INDEX idx_issues_last_seen;
DROP INDEX idx_issues_status_count;
DROP INDEX idx_issues_status_last_seen;
DROP INDEX idx_issue_releases_release;
DROP INDEX idx_issue_environments_env;
DROP TABLE issue_environments;
