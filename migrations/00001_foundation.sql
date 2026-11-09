-- M1 foundation: projects, dashboard users and sessions, settings.
-- Timestamps are UTC Unix milliseconds (decisions.md D18).

-- +goose Up
CREATE TABLE projects (
    id                 INTEGER PRIMARY KEY,
    name               TEXT    NOT NULL,
    slug               TEXT    NOT NULL UNIQUE,
    api_key_hash       BLOB    NOT NULL UNIQUE,  -- SHA-256 of the full key (D10)
    api_key_hint       TEXT    NOT NULL,         -- e.g. "fbx_…a1B2", safe to display
    api_key_rotated_at INTEGER NOT NULL,
    status             TEXT    NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
    last_event_at      INTEGER,
    created_at         INTEGER NOT NULL,
    updated_at         INTEGER NOT NULL
);

CREATE INDEX idx_projects_status ON projects (status);

-- Environments a project is configured for (PRD §4.1). M2 also records
-- environments seen on incoming events.
CREATE TABLE project_environments (
    project_id INTEGER NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    name       TEXT    NOT NULL,
    created_at INTEGER NOT NULL,
    PRIMARY KEY (project_id, name)
) WITHOUT ROWID;

CREATE TABLE users (
    id            INTEGER PRIMARY KEY,
    email         TEXT    NOT NULL UNIQUE COLLATE NOCASE,
    password_hash TEXT    NOT NULL,
    created_at    INTEGER NOT NULL,
    last_login_at INTEGER
);

CREATE TABLE sessions (
    id           INTEGER PRIMARY KEY,
    user_id      INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    token_hash   BLOB    NOT NULL UNIQUE,  -- SHA-256 of the cookie token
    expires_at   INTEGER NOT NULL,
    last_seen_at INTEGER NOT NULL,
    created_at   INTEGER NOT NULL
);

CREATE INDEX idx_sessions_user_id ON sessions (user_id);
CREATE INDEX idx_sessions_expires_at ON sessions (expires_at);

CREATE TABLE settings (
    key        TEXT    PRIMARY KEY,
    value      TEXT    NOT NULL,
    updated_at INTEGER NOT NULL
) WITHOUT ROWID;

-- +goose Down
DROP TABLE settings;
DROP TABLE sessions;
DROP TABLE users;
DROP TABLE project_environments;
DROP TABLE projects;
