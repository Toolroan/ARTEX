package db

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Existing graph operations that reopen work share the same parent lock as
// TryCompleteTask. Their public signatures remain compatible with legacy tools.
func (s *ExplorationStore) execObligationMutation(query string, args ...any) (sql.Result, error) {
	ctx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var deleted, archived sql.NullTime
	err = tx.QueryRowContext(ctx, `SELECT deleted_at,archived_at FROM tasks WHERE exploration_id=$1 FOR UPDATE`, s.expID).Scan(&deleted, &archived)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err == nil && (deleted.Valid || archived.Valid) {
		return nil, ErrMainMessageParentGone
	}
	res, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return res, tx.Commit()
}

func (s *ExplorationStore) execNodeTransition(state, query string, args ...any) (sql.Result, error) {
	if state == "open" || state == "running" || state == "paused" {
		return s.execObligationMutation(query, args...)
	}
	return s.db.Exec(query, args...)
}
