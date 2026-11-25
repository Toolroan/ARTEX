package db

import "context"

// FrontierContext is the cancellable pool-worker frontier read. The decisive
// inbox gate still belongs to the claim UPDATE, not this earlier snapshot.
func (s *ExplorationStore) FrontierContext(ctx context.Context, limit int) ([]*Node, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+nodeCols+` FROM exploration_nodes
WHERE exploration_id=$1 AND kind='intent' AND state='open'
ORDER BY priority DESC, id ASC LIMIT $2`, s.expID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNodes(rows)
}

// ClaimIntentTokenAfterPlannerReview is the autonomous worker-pool claim. The
// inbox predicate and open->running transition share one statement: a queued
// correction, including one already claimed by Planner, must be reviewed before
// a pool worker can pick up stale open work. It does not cancel existing workers
// or reinterpret needs_review as a permanent execution barrier. Explicit human
// worker controls retain the ordinary ClaimIntentToken/ClaimPausedIntentToken.
func (s *ExplorationStore) ClaimIntentTokenAfterPlannerReview(ctx context.Context, id int64, owner string) (string, error) {
	token, err := newUnclaimToken()
	if err != nil {
		return "", err
	}
	res, err := s.db.ExecContext(ctx, `UPDATE exploration_nodes
SET state='running', owner=$1, unclaim_token=$4, last_heartbeat_at=now()
WHERE id=$2 AND exploration_id=$3 AND kind='intent' AND state='open'
AND NOT EXISTS (SELECT 1 FROM main_message_routes
    WHERE exploration_id=$3 AND delivery_owner='planner'
      AND delivery_status IN ('queued','inflight'))`, owner, id, s.expID, token)
	if err != nil {
		return "", err
	}
	n, err := res.RowsAffected()
	if err != nil || n != 1 {
		return "", err
	}
	return token, nil
}
