package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Project statuses.
const (
	ProjectActive   = "active"
	ProjectDisabled = "disabled"
)

// Limits on project fields.
const (
	MaxProjectNameLen     = 100
	MaxEnvironments       = 20
	MaxEnvironmentNameLen = 64
)

// Project is a monitored application (PRD §4.1).
type Project struct {
	ID              int64
	Name            string
	Slug            string
	APIKeyHint      string
	APIKeyRotatedAt time.Time
	Status          string
	Environments    []string
	LastEventAt     time.Time // zero until the first event arrives (M2)
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// Active reports whether the project accepts events.
func (p Project) Active() bool { return p.Status == ProjectActive }

// ValidationError explains why user input was rejected.
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

func invalid(format string, args ...any) error {
	return &ValidationError{Msg: fmt.Sprintf(format, args...)}
}

// NormalizeProjectName trims a project name and checks its length.
func NormalizeProjectName(name string) (string, error) {
	name = strings.Join(strings.Fields(name), " ")
	if name == "" {
		return "", invalid("Project name is required.")
	}
	if len([]rune(name)) > MaxProjectNameLen {
		return "", invalid("Project name must be at most %d characters.", MaxProjectNameLen)
	}
	return name, nil
}

// NormalizeEnvironments parses a comma- or whitespace-separated list such as
// "production, staging". Names are lowercased, de-duplicated and sorted, and
// may contain letters, digits, '.', '_' and '-'.
func NormalizeEnvironments(input string) ([]string, error) {
	fields := strings.FieldsFunc(input, func(r rune) bool { return r == ',' || unicode.IsSpace(r) })
	envs := make([]string, 0, len(fields))
	for _, f := range fields {
		env := strings.ToLower(f)
		if len(env) > MaxEnvironmentNameLen {
			return nil, invalid("Environment %q is longer than %d characters.", env, MaxEnvironmentNameLen)
		}
		for _, r := range env {
			if !validEnvRune(r) {
				return nil, invalid("Environment %q may only contain letters, digits, '.', '_' and '-'.", env)
			}
		}
		envs = append(envs, env)
	}
	slices.Sort(envs)
	envs = slices.Compact(envs)
	if len(envs) > MaxEnvironments {
		return nil, invalid("A project can have at most %d environments.", MaxEnvironments)
	}
	return envs, nil
}

func validEnvRune(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-'
}

// Slugify turns a name into a URL-safe identifier, e.g. "My App!" → "my-app".
func Slugify(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		case b.Len() > 0 && !dash:
			b.WriteByte('-')
			dash = true
		}
	}
	slug := strings.TrimSuffix(b.String(), "-")
	if len(slug) > 60 {
		slug = strings.TrimSuffix(slug[:60], "-")
	}
	if slug == "" {
		slug = "project"
	}
	return slug
}

const projectColumns = `id, name, slug, api_key_hint, api_key_rotated_at, status, last_event_at, created_at, updated_at`

func scanProject(row interface{ Scan(...any) error }) (Project, error) {
	var (
		p                         Project
		rotated, created, updated int64
		lastEvent                 sql.NullInt64
	)
	err := row.Scan(&p.ID, &p.Name, &p.Slug, &p.APIKeyHint, &rotated, &p.Status, &lastEvent, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return Project{}, ErrNotFound
	}
	if err != nil {
		return Project{}, err
	}
	p.APIKeyRotatedAt, p.CreatedAt, p.UpdatedAt = fromMs(rotated), fromMs(created), fromMs(updated)
	p.LastEventAt = fromNullMs(lastEvent)
	return p, nil
}

// CreateProject inserts a project with a unique slug derived from name.
func (s *Store) CreateProject(ctx context.Context, name string, envs []string, keyHash []byte, keyHint string) (Project, error) {
	now := s.nowMs()
	base := Slugify(name)
	var id int64
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		for i := 1; ; i++ {
			slug := base
			if i > 1 {
				slug = base + "-" + strconv.Itoa(i)
			}
			res, err := tx.ExecContext(ctx,
				`INSERT INTO projects (name, slug, api_key_hash, api_key_hint, api_key_rotated_at, status, created_at, updated_at)
				 VALUES (?, ?, ?, ?, ?, 'active', ?, ?)
				 ON CONFLICT (slug) DO NOTHING`,
				name, slug, keyHash, keyHint, now, now, now)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n == 1 {
				id, err = res.LastInsertId()
				if err != nil {
					return err
				}
				break
			}
			if i > 1000 {
				return ErrConflict
			}
		}
		return replaceEnvironments(ctx, tx, id, envs, now)
	})
	if err != nil {
		if isUniqueViolation(err) {
			return Project{}, ErrConflict
		}
		return Project{}, fmt.Errorf("create project: %w", err)
	}
	return s.Project(ctx, id)
}

func replaceEnvironments(ctx context.Context, tx *sql.Tx, projectID int64, envs []string, now int64) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM project_environments WHERE project_id = ?`, projectID); err != nil {
		return err
	}
	for _, env := range envs {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO project_environments (project_id, name, created_at) VALUES (?, ?, ?)`,
			projectID, env, now); err != nil {
			return err
		}
	}
	return nil
}

// Project returns one project by ID.
func (s *Store) Project(ctx context.Context, id int64) (Project, error) {
	p, err := scanProject(s.reader.QueryRowContext(ctx, `SELECT `+projectColumns+` FROM projects WHERE id = ?`, id))
	if err != nil {
		return Project{}, err
	}
	return s.withEnvironments(ctx, p)
}

// ProjectByAPIKeyHash finds the project owning an API key.
func (s *Store) ProjectByAPIKeyHash(ctx context.Context, hash []byte) (Project, error) {
	p, err := scanProject(s.reader.QueryRowContext(ctx, `SELECT `+projectColumns+` FROM projects WHERE api_key_hash = ?`, hash))
	if err != nil {
		return Project{}, err
	}
	return s.withEnvironments(ctx, p)
}

func (s *Store) withEnvironments(ctx context.Context, p Project) (Project, error) {
	rows, err := s.reader.QueryContext(ctx, `SELECT name FROM project_environments WHERE project_id = ? ORDER BY name`, p.ID)
	if err != nil {
		return Project{}, err
	}
	defer func() { _ = rows.Close() }()
	p.Environments = []string{}
	for rows.Next() {
		var env string
		if err := rows.Scan(&env); err != nil {
			return Project{}, err
		}
		p.Environments = append(p.Environments, env)
	}
	return p, rows.Err()
}

// Projects lists every project, active ones first, then by name.
func (s *Store) Projects(ctx context.Context) ([]Project, error) {
	rows, err := s.reader.QueryContext(ctx,
		`SELECT `+projectColumns+` FROM projects ORDER BY status = 'disabled', name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var projects []Project
	index := map[int64]int{}
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		p.Environments = []string{}
		index[p.ID] = len(projects)
		projects = append(projects, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	envRows, err := s.reader.QueryContext(ctx, `SELECT project_id, name FROM project_environments ORDER BY project_id, name`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = envRows.Close() }()
	for envRows.Next() {
		var (
			id  int64
			env string
		)
		if err := envRows.Scan(&id, &env); err != nil {
			return nil, err
		}
		if i, ok := index[id]; ok {
			projects[i].Environments = append(projects[i].Environments, env)
		}
	}
	return projects, envRows.Err()
}

// CountProjects returns the number of active and disabled projects.
func (s *Store) CountProjects(ctx context.Context) (active, disabled int, err error) {
	err = s.reader.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(status = 'active'), 0), COALESCE(SUM(status = 'disabled'), 0) FROM projects`,
	).Scan(&active, &disabled)
	return active, disabled, err
}

// UpdateProject changes a project's name and environments.
func (s *Store) UpdateProject(ctx context.Context, id int64, name string, envs []string) error {
	now := s.nowMs()
	return s.inTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE projects SET name = ?, updated_at = ? WHERE id = ?`, name, now, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return replaceEnvironments(ctx, tx, id, envs, now)
	})
}

// SetProjectStatus enables or disables a project.
func (s *Store) SetProjectStatus(ctx context.Context, id int64, status string) error {
	if status != ProjectActive && status != ProjectDisabled {
		return invalid("unknown project status %q", status)
	}
	return s.execOne(ctx, `UPDATE projects SET status = ?, updated_at = ? WHERE id = ?`, status, s.nowMs(), id)
}

// RotateProjectKey replaces a project's API key. The old key stops working
// immediately (D10).
func (s *Store) RotateProjectKey(ctx context.Context, id int64, keyHash []byte, keyHint string) error {
	now := s.nowMs()
	return s.execOne(ctx,
		`UPDATE projects SET api_key_hash = ?, api_key_hint = ?, api_key_rotated_at = ?, updated_at = ? WHERE id = ?`,
		keyHash, keyHint, now, now, id)
}

// execOne runs a write that must affect exactly one row.
func (s *Store) execOne(ctx context.Context, query string, args ...any) error {
	res, err := s.writer.ExecContext(ctx, query, args...)
	if err != nil {
		if isUniqueViolation(err) {
			return ErrConflict
		}
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
