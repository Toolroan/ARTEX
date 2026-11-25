package db

import "context"

// Continue/wake commands remain deliverable when an ordinary Main mutation is
// waiting behind a lifecycle gate. A current explicit Continue + goal request
// is eligible too; unrelated controls and expired/inflight effects are not.
func (s *ExplorationStore) ReadyMainLifecycleDeliveryIDs(ctx context.Context, limit int) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT m.id FROM main_message_routes m
		WHERE m.exploration_id=$1 AND m.routing_status='routed' AND m.route='control'
		AND m.delivery_status='queued' AND m.delivery_owner='main'
		AND m.decision_data->>'kind'='control'
		AND (SELECT count(*)=count(DISTINCT tool)
		  AND count(*) FILTER(WHERE tool IN ('wake_planner','continue_task'))=1
		  AND bool_and(tool IN ('wake_planner','continue_task','set_goals','add_intent'))
		  AND (count(*)=1 OR count(*) FILTER(WHERE tool='set_goals')=1)
		  FROM jsonb_array_elements_text(CASE WHEN jsonb_typeof(m.decision_data->'control_tools')='array' THEN m.decision_data->'control_tools' ELSE '[]'::jsonb END) AS controls(tool))
		AND `+mainDeliveryReadySQL+`
		ORDER BY m.id LIMIT $2`, s.expID, mainPageLimit(limit))
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
