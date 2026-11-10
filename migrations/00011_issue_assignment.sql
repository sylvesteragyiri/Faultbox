-- M11 UI: allow dashboard users to own issues.

-- +goose Up
ALTER TABLE issues ADD COLUMN assigned_user_id INTEGER;
CREATE INDEX idx_issues_assignee_status_seen ON issues (assigned_user_id, status, last_seen_at DESC);
CREATE INDEX idx_issues_project_status ON issues (project_id, status);

-- +goose Down
DROP INDEX idx_issues_assignee_status_seen;
DROP INDEX idx_issues_project_status;
ALTER TABLE issues DROP COLUMN assigned_user_id;
