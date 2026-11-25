package db

import (
	"context"
	"database/sql"
	"errors"
)

func (s *ExplorationStore) finishPlannerWorkRequests(ctx context.Context, token string, success bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = s.lockMainParent(ctx, tx); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT m.id,COALESCE(w.state,''),COALESCE(w.review_requested,false) FROM main_message_routes m LEFT JOIN work_requests w ON w.id=m.id WHERE m.exploration_id=$1 AND m.delivery_owner='planner' AND m.delivery_status='inflight' AND m.delivery_token=$2 AND m.delivery_until>now() ORDER BY m.id`, s.expID, token)
	if err != nil {
		return err
	}
	type item struct {
		id    int64
		state string
		dirty bool
	}
	var items []item
	for rows.Next() {
		var i item
		if err = rows.Scan(&i.id, &i.state, &i.dirty); err != nil {
			rows.Close()
			return err
		}
		items = append(items, i)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if len(items) == 0 {
		return ErrMainMessageClaimLost
	}
	for _, i := range items {
		delivery, reason := "presented", ""
		unfinished := i.state != "" && i.state != "completed" && i.state != "cancelled"
		if !success {
			delivery, reason = "needs_review", "Planner delivery was interrupted; review existing effects before retrying"
			if unfinished {
				_, err = tx.ExecContext(ctx, `UPDATE work_requests SET state='blocked',resolution=$2,review_requested=false,updated_at=now() WHERE id=$1`, i.id, reason)
			}
		} else if unfinished && i.dirty {
			delivery = "queued"
		} else if unfinished && (i.state == "pending" || i.state == "review") {
			_, err = tx.ExecContext(ctx, `UPDATE work_requests SET state='blocked',resolution='Planner reviewed the request without recording a verified outcome or next work item; operator review is needed.',updated_at=now() WHERE id=$1`, i.id)
		} else if unfinished && i.state == "active" {
			_, err = tx.ExecContext(ctx, `UPDATE work_requests SET state='blocked',resolution='No linked open or running work remains, but the requested outcome has not been verified.',updated_at=now() WHERE id=$1 AND NOT EXISTS(SELECT 1 FROM work_request_nodes i JOIN exploration_nodes n ON n.id=i.node_id WHERE i.request_id=$1 AND i.kind='intent' AND n.state IN ('open','running'))`, i.id)
		}
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE main_message_routes SET delivery_status=$2,delivery_token='',delivery_until=NULL,last_error=$3,updated_at=now() WHERE id=$1`, i.id, delivery, reason); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// markIntentRequests records each terminal node version once. A late duplicate
// NotifyDone cannot keep requeuing a request that has already been reviewed.
func (s *ExplorationStore) markIntentRequests(ctx context.Context, intentID int64, reconcile bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = s.lockMainParent(ctx, tx); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `UPDATE work_request_nodes i SET observed_terminal_at=n.updated_at
FROM exploration_nodes n,work_requests w
WHERE i.request_id=w.id AND w.exploration_id=$1 AND w.state NOT IN ('completed','cancelled')
AND i.kind='intent' AND i.node_id=n.id AND n.state IN ('done','blocked','exhausted','stopped','paused','deleted')
AND i.observed_terminal_at IS DISTINCT FROM n.updated_at
AND ($2::bigint=0 OR i.node_id=$2)
AND (NOT $3 OR (w.state='active' AND NOT EXISTS
 (SELECT 1 FROM work_request_nodes j JOIN exploration_nodes x ON x.id=j.node_id
  WHERE j.request_id=w.id AND j.kind='intent' AND x.state IN ('open','running'))))
RETURNING i.request_id`, s.expID, intentID, reconcile)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		return tx.Commit()
	}
	if _, err = tx.ExecContext(ctx, `UPDATE work_requests SET state='review',review_requested=true,updated_at=now() WHERE id=ANY($1::bigint[])`, ids); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE main_message_routes SET delivery_owner='planner',delivery_status='queued',delivery_token='',delivery_until=NULL,last_error='',updated_at=now() WHERE exploration_id=$1 AND id=ANY($2::bigint[]) AND delivery_status='presented'`, s.expID, ids); err != nil {
		return err
	}
	return tx.Commit()
}

// MarkFinishedWorkRequestsForReview repairs a crash between a worker's fenced
// terminal write and its review notification. Only active work transitions;
// already-blocked or reviewed requests do not spin on recovery heartbeats.
func (s *ExplorationStore) MarkFinishedWorkRequestsForReview(ctx context.Context) error {
	return s.markIntentRequests(ctx, 0, true)
}

// QueueWorkRequestReviews is an explicit operator wake/restart action. It
// grants one review of existing tracked obligations, preserving all IDs. An
// uncertain interrupted delivery is never silently promoted to replayable work.
func (s *ExplorationStore) QueueWorkRequestReviews(ctx context.Context) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if err = s.lockMainParent(ctx, tx); err != nil {
		return 0, err
	}
	rows, err := tx.QueryContext(ctx, `UPDATE main_message_routes m SET delivery_owner='planner',delivery_status='queued',delivery_token='',delivery_until=NULL,last_error='',updated_at=now() FROM work_requests w WHERE w.id=m.id AND w.exploration_id=$1 AND w.state NOT IN ('completed','cancelled') AND m.delivery_status='presented' RETURNING m.id`, s.expID)
	if err != nil {
		return 0, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	if len(ids) > 0 {
		if _, err = tx.ExecContext(ctx, `UPDATE work_requests SET state='review',review_requested=true,updated_at=now() WHERE id=ANY($1::bigint[])`, ids); err != nil {
			return 0, err
		}
	}
	return int64(len(ids)), tx.Commit()
}

// ReviewWorkRequest is an explicit authenticated operator reconciliation action,
// unlike automatic recovery/wake. It may reopen an uncertain prior delivery,
// preserving its effects and identity for inspection rather than redispatch.
func (s *ExplorationStore) ReviewWorkRequest(ctx context.Context, requestID int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = s.lockMainParent(ctx, tx); err != nil {
		return err
	}
	var state, delivery string
	var live bool
	err = tx.QueryRowContext(ctx, `SELECT w.state,m.delivery_status,COALESCE(m.delivery_status='inflight' AND m.delivery_until>now(),false) FROM work_requests w JOIN main_message_routes m ON m.id=w.id WHERE w.exploration_id=$1 AND w.id=$2 FOR UPDATE OF w,m`, s.expID, requestID).Scan(&state, &delivery, &live)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrWorkRequestConflict
	}
	if err != nil {
		return err
	}
	if state == "completed" || state == "cancelled" {
		return errors.New("only unresolved work requests can be reviewed")
	}
	if live {
		return errors.New("this request currently has a live owner; wait for its review to finish")
	}
	if delivery == "queued" {
		return tx.Commit()
	}
	if delivery != "presented" && delivery != "needs_review" && delivery != "inflight" {
		return errors.New("request delivery is not ready for review")
	}
	if _, err = tx.ExecContext(ctx, `UPDATE work_requests SET state='review',review_requested=true,updated_at=now() WHERE id=$1`, requestID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE main_message_routes SET delivery_owner='planner',delivery_status='queued',delivery_token='',delivery_until=NULL,last_error='',updated_at=now() WHERE id=$1`, requestID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *ExplorationStore) blockInterruptedWorkRequests(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `UPDATE work_requests w SET state='blocked',resolution='Delivery lease expired; inspect existing effects before retrying.',review_requested=false,updated_at=now() FROM main_message_routes m WHERE m.id=w.id AND w.exploration_id=$1 AND w.state NOT IN ('completed','cancelled') AND m.delivery_status='inflight' AND (m.delivery_until IS NULL OR m.delivery_until<=now())`, s.expID)
	return err
}
