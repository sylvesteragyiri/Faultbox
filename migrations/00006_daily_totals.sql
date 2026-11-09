-- Per-day event totals per project, maintained by the ingest transaction.
-- The overview's trend, project cards and activity heatmap read this
-- instead of summing issue_daily_counts across every issue, so their cost
-- depends on days x projects, not on how many issues exist (roadmap M4 note).

-- +goose Up
CREATE TABLE daily_totals (
    day            INTEGER NOT NULL,             -- days since the Unix epoch, UTC
    project_id     INTEGER NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    errors         INTEGER NOT NULL DEFAULT 0,   -- exceptions and messages
    queue_failures INTEGER NOT NULL DEFAULT 0,   -- every queue failure event, retries included
    PRIMARY KEY (day, project_id)
) WITHOUT ROWID;

CREATE INDEX idx_daily_totals_project ON daily_totals (project_id, day);

INSERT INTO daily_totals (day, project_id, errors, queue_failures)
SELECT d.day, i.project_id,
       SUM(CASE WHEN i.category = 'queue_failure' THEN 0 ELSE d.count END),
       SUM(CASE WHEN i.category = 'queue_failure' THEN d.count ELSE 0 END)
  FROM issue_daily_counts d JOIN issues i ON i.id = d.issue_id
 GROUP BY 1, 2;

-- +goose Down
DROP TABLE daily_totals;
