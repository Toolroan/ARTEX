package db

import (
	"context"
	"errors"
)

// MainInstructionStatuses reads only delivery metadata. In particular it does
// not load raw/operator/assistant text, context snapshots, or provider errors.
// Explicit IDs must all belong to the task's exploration and Main segment.
// Without IDs, the current message is first, followed by recent actionable
// messages, with a hard cap of 20 regardless of conversation length.
func (s *ExplorationStore) MainInstructionStatuses(ctx context.Context, segment int, currentID int64, ids []int64) ([]MainMessage, error) {
	if segment < 0 || currentID <= 0 || len(ids) > 20 {
		return nil, errors.New("invalid instruction status query")
	}
	seen := make(map[int64]bool, len(ids))
	for _, id := range ids {
		if id <= 0 || seen[id] {
			return nil, errors.New("invalid instruction status IDs")
		}
		seen[id] = true
	}
	const cols = `id,main_seg,route,routing_status,delivery_status,delivery_owner,delivery_until,created_at,updated_at`
	query := `SELECT ` + cols + ` FROM main_message_routes WHERE exploration_id=$1 AND main_seg=$2 AND (id=$3 OR route IN ('action','correction','control')) ORDER BY (id=$3) DESC,id DESC LIMIT 20`
	args := []any{s.expID, segment, currentID}
	if len(ids) > 0 {
		query = `SELECT ` + cols + ` FROM main_message_routes WHERE exploration_id=$1 AND main_seg=$2 AND id=ANY($3::bigint[]) ORDER BY id DESC`
		args = []any{s.expID, segment, ids}
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]MainMessage, 0, 20)
	currentFound := false
	for rows.Next() {
		var msg MainMessage
		if err := rows.Scan(&msg.ID, &msg.Segment, &msg.Route, &msg.RoutingStatus, &msg.DeliveryStatus, &msg.DeliveryOwner, &msg.DeliveryUntil, &msg.CreatedAt, &msg.UpdatedAt); err != nil {
			return nil, err
		}
		msg.ExplorationID = s.expID
		currentFound = currentFound || msg.ID == currentID
		out = append(out, msg)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if (len(ids) > 0 && len(out) != len(ids)) || (len(ids) == 0 && !currentFound) {
		return nil, errors.New("instruction status is outside the current task or Main session")
	}
	return out, nil
}

// MainInstructionEligibility is the context-aware, metadata-only eligibility
// check used by live status reads. Both pool waits and statement execution are
// bounded by the caller's deadline; the legacy contextless planner helpers must
// not be used in this bounded diagnostic path.
func (s *ExplorationStore) MainInstructionEligibility(ctx context.Context) (openGoal, activeIntent bool, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT
		EXISTS(SELECT 1 FROM exploration_nodes WHERE exploration_id=$1 AND kind='goal' AND state='open'),
		EXISTS(SELECT 1 FROM exploration_nodes WHERE exploration_id=$1 AND kind='intent' AND state IN ('open','running'))`, s.expID).Scan(&openGoal, &activeIntent)
	return
}
