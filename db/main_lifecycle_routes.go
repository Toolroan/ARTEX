package db

import "context"

// Continue/wake commands remain deliverable when an ordinary Main mutation is
// waiting behind a lifecycle gate. Only queued, exclusively classified control
// messages are selected; expired/inflight effects are never replayed here.
func (s *ExplorationStore) ReadyMainLifecycleDeliveryIDs(ctx context.Context, limit int) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM main_message_routes
		WHERE exploration_id=$1 AND routing_status='routed' AND route='control'
		AND delivery_status='queued' AND delivery_owner='main'
		AND decision_data->>'kind'='control'
		AND (decision_data->'control_tools'='["wake_planner"]'::jsonb OR decision_data->'control_tools'='["continue_task"]'::jsonb)
		ORDER BY id LIMIT $2`, s.expID, mainPageLimit(limit))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
