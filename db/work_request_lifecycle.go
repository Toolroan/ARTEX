package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

func routedWorkRequest(d MainRouteDecision) bool {
	if d.Route == "action" || d.Route == "correction" {
		return true
	}
	if d.Route != "control" {
		return false
	}
	var decision struct {
		ControlTools []string `json:"control_tools"`
	}
	if json.Unmarshal(d.DecisionData, &decision) != nil {
		return false
	}
	for _, name := range decision.ControlTools {
		if name == "set_goals" {
			return true
		}
	}
	return false
}

func (s *ExplorationStore) createRoutedWorkRequest(ctx context.Context, tx *sql.Tx, msg *MainMessage, d MainRouteDecision) error {
	if !routedWorkRequest(d) {
		return nil
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO work_requests(id,exploration_id,acceptance) VALUES($1,$2,$3) ON CONFLICT(id) DO NOTHING`, msg.ID, s.expID, msg.OperatorText)
	return err
}

// HandoffMainRequest transfers the same durable request and existing graph IDs
// to Planner. It never creates another operator message or another obligation.
func (s *ExplorationStore) HandoffMainRequest(ctx context.Context, messageID int64, token string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = s.lockMainParent(ctx, tx); err != nil {
		return err
	}
	var state string
	err = tx.QueryRowContext(ctx, `SELECT w.state FROM work_requests w JOIN main_message_routes m ON m.id=w.id WHERE w.exploration_id=$1 AND w.id=$2 AND m.delivery_owner='main' AND m.delivery_status='inflight' AND m.delivery_token=$3 AND m.delivery_until>now() FOR UPDATE OF w`, s.expID, messageID, token).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrWorkRequestConflict
	}
	if err != nil {
		return err
	}
	if state == "completed" || state == "cancelled" {
		return nil
	}
	_, err = tx.ExecContext(ctx, `UPDATE main_message_routes SET delivery_owner='planner',delivery_status='queued',delivery_token='',delivery_until=NULL,last_error='',updated_at=now() WHERE id=$1`, messageID)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE work_requests SET state=CASE WHEN state='pending' THEN 'review' ELSE state END,review_requested=false,updated_at=now() WHERE id=$1`, messageID)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// Worker termination requests outcome review. While Planner owns a live lease,
// only a dirty flag is set; its token remains valid until its round finishes.
func (s *ExplorationStore) MarkIntentRequestsForReview(ctx context.Context, intentID int64) error {
	return s.markIntentRequests(ctx, intentID, false)
}

// ReviseWorkRequest changes explicit acceptance criteria under a current lease
// and invalidates every prior revision grant. Source operator text is immutable.
func (s *ExplorationStore) ReviseWorkRequest(ctx context.Context, g WorkRequestGrant, acceptance string) (*WorkRequest, error) {
	acceptance = strings.TrimSpace(acceptance)
	if acceptance == "" || !validMainText(acceptance, 32000) {
		return nil, errors.New("invalid request acceptance criteria")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = s.lockWorkGrant(ctx, tx, g); err != nil {
		return nil, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE work_requests SET revision=revision+1,acceptance=$2,state='review',resolution='',review_requested=true,updated_at=now() WHERE id=$1`, g.RequestID, acceptance)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return s.GetWorkRequest(ctx, g.RequestID)
}

// ResolveWorkRequest records an outcome, not a model-turn delivery. An empty
// frontier or a worker's normal return is never sufficient completion proof.
func (s *ExplorationStore) ResolveWorkRequest(ctx context.Context, g WorkRequestGrant, state, reason string, evidenceIDs []int64) error {
	if state != "active" && state != "blocked" && state != "completed" && state != "cancelled" {
		return errors.New("invalid work request outcome")
	}
	reason = strings.TrimSpace(reason)
	if reason == "" || !validMainText(reason, 32000) || len(evidenceIDs) > 64 {
		return errors.New("a bounded resolution and evidence list are required")
	}
	if state == "cancelled" && !g.AllowCancel {
		return errors.New("cancelling a request requires explicit operator authorization")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = s.lockWorkGrant(ctx, tx, g); err != nil {
		return err
	}
	var dirty bool
	if err = tx.QueryRowContext(ctx, `SELECT review_requested FROM work_requests WHERE id=$1`, g.RequestID).Scan(&dirty); err != nil {
		return err
	}
	if state == "completed" && dirty {
		return errors.New("new worker results require another review before completion")
	}
	var goals, unmet, active int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FILTER(WHERE i.kind='goal'),count(*) FILTER(WHERE i.kind='goal' AND (n.id IS NULL OR n.state<>'met')),count(*) FILTER(WHERE i.kind='intent' AND (n.id IS NULL OR n.state IN ('open','running','paused'))) FROM work_request_nodes i LEFT JOIN exploration_nodes n ON n.id=i.node_id WHERE i.request_id=$1`, g.RequestID).Scan(&goals, &unmet, &active); err != nil {
		return err
	}
	if state == "completed" && (unmet > 0 || active > 0) {
		return errors.New("linked goals or work remain unresolved")
	}
	if state == "completed" && goals == 0 && len(evidenceIDs) == 0 {
		return errors.New("a bounded request needs observed evidence before completion")
	}
	seen := map[int64]bool{}
	for _, nodeID := range evidenceIDs {
		if seen[nodeID] {
			continue
		}
		seen[nodeID] = true
		var kind, nodeState string
		var raw json.RawMessage
		if err = tx.QueryRowContext(ctx, `SELECT kind,state,payload FROM exploration_nodes WHERE exploration_id=$1 AND id=$2 FOR KEY SHARE`, s.expID, nodeID).Scan(&kind, &nodeState, &raw); err != nil {
			return errors.New("evidence must belong to this request's task")
		}
		if kind != KindFact && kind != KindFinding {
			return errors.New("request evidence must be an observed fact or proven finding")
		}
		if state == "completed" && goals == 0 {
			var payload map[string]any
			_ = json.Unmarshal(raw, &payload)
			confidence, _ := payload["confidence"].(string)
			source, _ := payload["source"].(string)
			if kind != KindFact || nodeState != "confirmed" || strings.ToLower(strings.TrimSpace(confidence)) != "observed" || (source != "tool_output" && source != "operator_input") {
				return errors.New("a bounded request needs an observed tool_output/operator_input fact; findings must prove a linked goal")
			}
		}
		key := fmt.Sprintf("node:%d", nodeID)
		_, err = tx.ExecContext(ctx, `INSERT INTO work_request_nodes(request_id,request_revision,kind,slot_key,summary,allocation_key,node_id) VALUES($1,$2,'evidence',$3,$4,$4,$5) ON CONFLICT(request_id,node_id) DO NOTHING`, g.RequestID, g.Revision, uuid.NewString(), key, nodeID)
		if err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE work_requests SET state=CASE WHEN review_requested AND $2 NOT IN ('completed','cancelled') THEN 'review' ELSE $2 END,resolution=$3,updated_at=now() WHERE id=$1`, g.RequestID, state, reason)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// TryCompleteTask is the success-only terminal transition. It shares the task
// row lock with message acceptance, routing and request/graph writes. Resource
// limits and operator cancellation must use their separate termination paths.
func (d *DB) TryCompleteTask(ctx context.Context, taskID int64) (bool, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var expID int64
	var status string
	var paused, queued bool
	err = tx.QueryRowContext(ctx, `SELECT exploration_id,status,paused,queued FROM tasks WHERE id=$1 AND deleted_at IS NULL AND archived_at IS NULL FOR UPDATE`, taskID).Scan(&expID, &status, &paused, &queued)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if IsTerminal(status) || paused || queued {
		return false, nil
	}
	// Historical failed/uncertain chat is not silently adopted as new work.
	// New uncertain deliveries still block via their unresolved request above.
	var blocked bool
	err = tx.QueryRowContext(ctx, `SELECT
EXISTS(SELECT 1 FROM work_requests WHERE exploration_id=$1 AND state NOT IN ('completed','cancelled')) OR
EXISTS(SELECT 1 FROM main_message_routes WHERE exploration_id=$1 AND (routing_status IN ('pending','running') OR delivery_status IN ('queued','inflight'))) OR
EXISTS(SELECT 1 FROM exploration_nodes WHERE exploration_id=$1 AND ((kind='goal' AND state='open') OR (kind='intent' AND state IN ('open','running','paused'))))`, expID).Scan(&blocked)
	if err != nil || blocked {
		return false, err
	}
	res, err := tx.ExecContext(ctx, `UPDATE tasks SET status='done',completed_at=COALESCE(completed_at,now()),end_reason='' WHERE id=$1 AND status=$2`, taskID, status)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, tx.Commit()
}

// WorkRequestsAwaitingInput suppresses autonomous retry loops only when every
// outstanding obligation is explicitly blocked and no unrelated work can run.
// An explicit operator wake requeues a review and bypasses this idle condition.
func (s *ExplorationStore) WorkRequestsAwaitingInput(ctx context.Context) (bool, error) {
	var waiting bool
	err := s.db.QueryRowContext(ctx, `SELECT
EXISTS(SELECT 1 FROM work_requests WHERE exploration_id=$1 AND state='blocked') AND
NOT EXISTS(SELECT 1 FROM work_requests WHERE exploration_id=$1 AND state NOT IN ('blocked','completed','cancelled')) AND
NOT EXISTS(SELECT 1 FROM exploration_nodes WHERE exploration_id=$1 AND kind='intent' AND state IN ('open','running')) AND
NOT EXISTS(SELECT 1 FROM exploration_nodes n WHERE n.exploration_id=$1 AND n.kind='goal' AND n.state='open' AND NOT EXISTS
 (SELECT 1 FROM work_request_nodes i JOIN work_requests w ON w.id=i.request_id
  WHERE i.node_id=n.id AND i.kind='goal' AND w.exploration_id=$1 AND w.state='blocked'))`, s.expID).Scan(&waiting)
	return waiting, err
}
