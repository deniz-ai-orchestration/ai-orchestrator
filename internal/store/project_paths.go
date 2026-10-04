package store

import "context"

// ResolveLegacyProject replaces the old folder-name key without losing its
// workflows or agent history. The caller resolves it using projects.dir.
func (s *Store) ResolveLegacyProject(ctx context.Context, old string, p Project) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT INTO projects(path,trusted,has_git,autonomous,github_repo,created_at,updated_at)
	 SELECT ?,trusted,?,0,?,created_at,updated_at FROM projects WHERE path=? ON CONFLICT(path) DO NOTHING`, p.Path, p.HasGit, p.GitHubRepo, old); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE workflows SET project_path=? WHERE project_path=?`, p.Path, old); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE agents SET project=? WHERE project=?`, p.Path, old); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM projects WHERE path=?`, old); err != nil {
		return err
	}
	return tx.Commit()
}
