package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Project is a directory on PC2 orch may work in. Trust is granted once
// from the panel; an untrusted path gets no agents and no worktrees. A
// directory without .git is chat-only: Autonomous stays off and no PR is
// pushed from it.
type Project struct {
	Path       string // absolute, cleaned
	Trusted    bool
	HasGit     bool
	Autonomous bool   // the autonomous loop; only meaningful with HasGit
	GitHubRepo string // owner/name of the origin remote, "" when not GitHub
	// Name and Description are asked on creation; empty falls back to the
	// directory's last folder.
	Name        string
	Description string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

const projectCols = `path, trusted, has_git, autonomous, github_repo, name, description, created_at, updated_at`

func scanProject(row scanner) (Project, error) {
	var p Project
	var created, updated string
	err := row.Scan(&p.Path, &p.Trusted, &p.HasGit, &p.Autonomous, &p.GitHubRepo, &p.Name, &p.Description, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return Project{}, ErrNotFound
	}
	if err != nil {
		return Project{}, err
	}
	p.CreatedAt, _ = parseTime(created)
	p.UpdatedAt, _ = parseTime(updated)
	return p, nil
}

// UpsertProject records what orch found at a path and whether you trust
// it. The autonomous switch belongs to you, so an existing row keeps its
// value and new rows start with it off; a path that lost its .git has the
// switch forced off.
func (s *Store) UpsertProject(ctx context.Context, p Project) error {
	now := fmtTime(s.now())
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO projects (path, trusted, has_git, autonomous, github_repo, name, description, created_at, updated_at)
		VALUES (?, ?, ?, 0, ?, ?, ?, ?, ?)
		ON CONFLICT (path) DO UPDATE SET
			trusted = excluded.trusted, has_git = excluded.has_git,
			github_repo = excluded.github_repo,
			name = excluded.name, description = excluded.description,
			autonomous = CASE WHEN excluded.has_git = 0 THEN 0 ELSE projects.autonomous END,
			updated_at = excluded.updated_at`,
		p.Path, p.Trusted, p.HasGit, p.GitHubRepo, p.Name, p.Description, now, now)
	if err != nil {
		return err
	}
	return nil
}

// GetProject returns one stored project, or ErrNotFound.
func (s *Store) GetProject(ctx context.Context, path string) (Project, error) {
	return scanProject(s.db.QueryRowContext(ctx, `SELECT `+projectCols+` FROM projects WHERE path = ?`, path))
}

// ListProjects returns every known project, by path.
func (s *Store) ListProjects(ctx context.Context) ([]Project, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+projectCols+` FROM projects ORDER BY path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// SetProjectAutonomous switches a project's autonomous loop on or off.
// The loop needs git: on a chat-only project the switch is forced off.
func (s *Store) SetProjectAutonomous(ctx context.Context, path string, on bool) error {
	return s.exec1(ctx, `UPDATE projects SET autonomous = (? AND has_git), updated_at = ? WHERE path = ?`,
		on, fmtTime(s.now()), path)
}
