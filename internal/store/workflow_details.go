package store

import "context"

func (s *Store) SetWorkflowDetails(ctx context.Context, id int64, prompt, model string) error {
	return s.exec1(ctx, `UPDATE workflows SET prompt=?,model=?,updated_at=? WHERE id=?`, prompt, model, fmtTime(s.now()), id)
}

func (s *Store) SetWorkflowWorkspace(ctx context.Context, id int64, branch, base, workspace string) error {
	return s.exec1(ctx, `UPDATE workflows SET branch=?,base=?,workspace=?,updated_at=? WHERE id=?`, branch, base, workspace, fmtTime(s.now()), id)
}

func (s *Store) SetWorkflowPhase(ctx context.Context, id int64, phase string) error {
	return s.exec1(ctx, `UPDATE workflows SET phase=?,updated_at=? WHERE id=?`, phase, fmtTime(s.now()), id)
}

func (s *Store) BumpWorkflowReviewCycle(ctx context.Context, id int64) (int, error) {
	return s.bumpWorkflow(ctx, id, "review_cycles")
}
