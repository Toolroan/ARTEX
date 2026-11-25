package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

var ErrWorkRequestConflict = errors.New("work request ownership or revision changed")

type WorkRequestGrant struct {
	RequestID     int64
	Revision      int64
	DeliveryToken string
	Owner         string
	AllowCancel   bool // server-only explicit operator authorization
}

type WorkRequestNode struct {
	ID              int64  `json:"id"`
	NodeID          int64  `json:"node_id,omitempty"`
	Kind            string `json:"kind"`
	SlotKey         string `json:"slot_key"`
	Summary         string `json:"summary"`
	State           string `json:"state"`
	RequestRevision int64  `json:"request_revision"`
}

type WorkRequest struct {
	ID              int64             `json:"id"`
	SourceMessageID int64             `json:"source_message_id"`
	Route           string            `json:"route"`
	OperatorText    string            `json:"operator_text"`
	State           string            `json:"state"`
	Revision        int64             `json:"revision"`
	Acceptance      string            `json:"acceptance"`
	Resolution      string            `json:"resolution"`
	ReviewRequested bool              `json:"review_requested"`
	CreatedAt       time.Time         `json:"created_at"`
	UpdatedAt       time.Time         `json:"updated_at"`
	Nodes           []WorkRequestNode `json:"nodes"`
}

type WorkNodeSpec struct {
	ItemID    int64
	Payload   map[string]any
	Priority  int
	Anchors   []int64
	Origin    string
	ParentIDs []int64
}

const workRequestCols = `w.id,w.revision,w.state,w.acceptance,w.resolution,w.review_requested,w.created_at,w.updated_at,m.route,m.operator_text`

func scanWorkRequest(row interface{ Scan(...any) error }) (*WorkRequest, error) {
	w := new(WorkRequest)
	err := row.Scan(&w.ID, &w.Revision, &w.State, &w.Acceptance, &w.Resolution, &w.ReviewRequested, &w.CreatedAt, &w.UpdatedAt, &w.Route, &w.OperatorText)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	w.SourceMessageID = w.ID
	w.Nodes = []WorkRequestNode{}
	return w, err
}

func (s *ExplorationStore) GetWorkRequest(ctx context.Context, id int64) (*WorkRequest, error) {
	w, err := scanWorkRequest(s.db.QueryRowContext(ctx, `SELECT `+workRequestCols+` FROM work_requests w JOIN main_message_routes m ON m.id=w.id WHERE w.exploration_id=$1 AND w.id=$2`, s.expID, id))
	if err != nil || w == nil {
		return w, err
	}
	err = s.readWorkRequestNodes(ctx, w)
	return w, err
}

func (s *ExplorationStore) ListWorkRequests(ctx context.Context, limit int) ([]WorkRequest, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+workRequestCols+` FROM work_requests w JOIN main_message_routes m ON m.id=w.id WHERE w.exploration_id=$1 ORDER BY (w.state IN ('completed','cancelled')),w.id DESC LIMIT $2`, s.expID, limit)
	if err != nil {
		return nil, err
	}
	out := []WorkRequest{}
	for rows.Next() {
		w, e := scanWorkRequest(rows)
		if e != nil {
			rows.Close()
			return nil, e
		}
		out = append(out, *w)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for i := range out {
		if err = s.readWorkRequestNodes(ctx, &out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *ExplorationStore) readWorkRequestNodes(ctx context.Context, w *WorkRequest) error {
	rows, err := s.db.QueryContext(ctx, `SELECT i.id,COALESCE(i.node_id,0),i.kind,i.slot_key,i.summary,COALESCE(n.state,'planned'),i.request_revision FROM work_request_nodes i LEFT JOIN exploration_nodes n ON n.id=i.node_id WHERE i.request_id=$1 ORDER BY i.id`, w.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var n WorkRequestNode
		if err = rows.Scan(&n.ID, &n.NodeID, &n.Kind, &n.SlotKey, &n.Summary, &n.State, &n.RequestRevision); err != nil {
			return err
		}
		w.Nodes = append(w.Nodes, n)
	}
	return rows.Err()
}

// Every mutating request operation uses this lock order: task, request, nodes.
// Existing message-delivery leases are the sole planning authority. A tool
// argument cannot manufacture another request's lease or a newer revision.
func (s *ExplorationStore) lockWorkGrant(ctx context.Context, tx *sql.Tx, g WorkRequestGrant) (string, error) {
	if g.RequestID <= 0 || g.Revision <= 0 || g.DeliveryToken == "" || (g.Owner != "main" && g.Owner != "planner") {
		return "", ErrWorkRequestConflict
	}
	if err := s.lockMainParent(ctx, tx); err != nil {
		return "", err
	}
	var state string
	err := tx.QueryRowContext(ctx, `SELECT w.state FROM work_requests w JOIN main_message_routes m ON m.id=w.id WHERE w.exploration_id=$1 AND w.id=$2 AND w.revision=$3 AND m.delivery_owner=$4 AND m.delivery_status='inflight' AND m.delivery_token=$5 AND m.delivery_until>now() FOR UPDATE OF w`, s.expID, g.RequestID, g.Revision, g.Owner, g.DeliveryToken).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrWorkRequestConflict
	}
	if err == nil && (state == "completed" || state == "cancelled") {
		return "", ErrWorkRequestConflict
	}
	return state, err
}

func workDigest(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func (s *ExplorationStore) AllocateWorkRequestItem(ctx context.Context, g WorkRequestGrant, kind, summary string) (WorkRequestNode, error) {
	var out WorkRequestNode
	summary = strings.TrimSpace(summary)
	if (kind != "goal" && kind != "intent") || summary == "" || !validMainText(summary, 16000) {
		return out, errors.New("invalid work item kind or summary")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	if _, err = s.lockWorkGrant(ctx, tx, g); err != nil {
		return out, err
	}
	key := workDigest(strings.Join(strings.Fields(summary), " "))
	err = tx.QueryRowContext(ctx, `INSERT INTO work_request_nodes(request_id,request_revision,kind,slot_key,summary,allocation_key) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(request_id,request_revision,kind,allocation_key) DO UPDATE SET summary=work_request_nodes.summary RETURNING id,COALESCE(node_id,0),kind,slot_key,summary,request_revision`, g.RequestID, g.Revision, kind, uuid.NewString(), summary, key).Scan(&out.ID, &out.NodeID, &out.Kind, &out.SlotKey, &out.Summary, &out.RequestRevision)
	if err != nil {
		return out, err
	}
	out.State = "planned"
	return out, tx.Commit()
}

func (s *ExplorationStore) CreateWorkRequestNode(ctx context.Context, g WorkRequestGrant, spec WorkNodeSpec) (int64, bool, error) {
	if spec.ItemID <= 0 || len(spec.ParentIDs) > 64 || len(spec.Anchors) > 64 {
		return 0, false, errors.New("invalid work item")
	}
	raw, err := json.Marshal(spec.Payload)
	if err != nil || len(raw) > 128<<10 {
		return 0, false, errors.New("invalid work item payload")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback()
	if _, err = s.lockWorkGrant(ctx, tx, g); err != nil {
		return 0, false, err
	}
	var kind, summary, oldHash string
	var existing sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT kind,summary,node_id,creation_hash FROM work_request_nodes WHERE id=$1 AND request_id=$2 AND request_revision=$3 FOR UPDATE`, spec.ItemID, g.RequestID, g.Revision).Scan(&kind, &summary, &existing, &oldHash)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, ErrWorkRequestConflict
	}
	if err != nil {
		return 0, false, err
	}
	if kind != "goal" && kind != "intent" {
		return 0, false, errors.New("only goal and intent items may create nodes")
	}
	field := "summary"
	if kind == "goal" {
		field = "text"
	}
	text, _ := spec.Payload[field].(string)
	if strings.TrimSpace(text) != summary {
		return 0, false, errors.New("work item text must match its allocated summary")
	}
	hash := workDigest(struct {
		Payload          map[string]any
		Priority         int
		Anchors, Parents []int64
	}{spec.Payload, spec.Priority, spec.Anchors, spec.ParentIDs})
	if existing.Valid {
		if oldHash != hash {
			return 0, false, errors.New("work item was already created with different inputs")
		}
		return existing.Int64, false, tx.Commit()
	}
	if oldHash != "" {
		return 0, false, errors.New("the work item's previously created node was deleted; allocate a revised item instead")
	}
	for _, parent := range spec.ParentIDs {
		var parentKind string
		if err = tx.QueryRowContext(ctx, `SELECT kind FROM exploration_nodes WHERE id=$1 AND exploration_id=$2 FOR KEY SHARE`, parent, s.expID).Scan(&parentKind); err != nil {
			return 0, false, err
		}
		if parentKind != KindFact && parentKind != KindFinding {
			return 0, false, errors.New("intent parents must be facts or findings")
		}
	}
	var id int64
	origin := g.Owner
	if spec.Origin != "" {
		origin = spec.Origin
	}
	err = tx.QueryRowContext(ctx, `INSERT INTO exploration_nodes(exploration_id,kind,payload,priority,state,origin) VALUES($1,$2,$3,$4,'open',$5) RETURNING id`, s.expID, kind, string(raw), spec.Priority, origin).Scan(&id)
	if err != nil {
		return 0, false, err
	}
	for _, anchor := range spec.Anchors {
		if _, err = tx.ExecContext(ctx, `INSERT INTO exploration_anchors(node_id,asset_id) VALUES($1,$2) ON CONFLICT DO NOTHING`, id, anchor); err != nil {
			return 0, false, err
		}
	}
	parents := spec.ParentIDs
	if kind == "intent" && len(parents) == 0 {
		var originID int64
		err = tx.QueryRowContext(ctx, `SELECT id FROM exploration_nodes WHERE exploration_id=$1 AND kind='fact' AND state='origin' ORDER BY id LIMIT 1`, s.expID).Scan(&originID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return 0, false, err
		}
		if originID > 0 {
			parents = []int64{originID}
		}
	}
	if kind == "intent" {
		for _, parent := range parents {
			if _, err = tx.ExecContext(ctx, `INSERT INTO exploration_edges(exploration_id,src_id,rel,dst_id) VALUES($1,$2,'derived_from',$3) ON CONFLICT DO NOTHING`, s.expID, parent, id); err != nil {
				return 0, false, err
			}
		}
	}
	if kind == "goal" {
		if _, err = tx.ExecContext(ctx, `INSERT INTO exploration_edges(exploration_id,src_id,rel,dst_id) SELECT $1,id,'spawns',$2 FROM exploration_nodes WHERE exploration_id=$1 AND kind='fact' AND state='origin' ORDER BY id LIMIT 1 ON CONFLICT DO NOTHING`, s.expID, id); err != nil {
			return 0, false, err
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE work_request_nodes SET node_id=$2,creation_hash=$3 WHERE id=$1`, spec.ItemID, id, hash); err != nil {
		return 0, false, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE work_requests SET state='active',resolution='',updated_at=now() WHERE id=$1`, g.RequestID); err != nil {
		return 0, false, err
	}
	return id, true, tx.Commit()
}

// LinkWorkRequestNode attaches existing work/evidence when correcting or
// adopting work. The node remains distinct from fact/finding lineage parents.
func (s *ExplorationStore) LinkWorkRequestNode(ctx context.Context, g WorkRequestGrant, nodeID int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = s.lockWorkGrant(ctx, tx, g); err != nil {
		return err
	}
	var kind string
	var payload json.RawMessage
	if err = tx.QueryRowContext(ctx, `SELECT kind,payload FROM exploration_nodes WHERE exploration_id=$1 AND id=$2 FOR KEY SHARE`, s.expID, nodeID).Scan(&kind, &payload); err != nil {
		return err
	}
	if kind == KindFact || kind == KindFinding {
		kind = "evidence"
	}
	if kind != "goal" && kind != "intent" && kind != "evidence" {
		return errors.New("only goals, intents, facts and findings may be linked to a request")
	}
	key := fmt.Sprintf("node:%d", nodeID)
	var fields map[string]any
	_ = json.Unmarshal(payload, &fields)
	field := "summary"
	if kind == "goal" {
		field = "text"
	}
	summary, _ := fields[field].(string)
	if summary = strings.TrimSpace(summary); summary == "" {
		summary = key
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO work_request_nodes(request_id,request_revision,kind,slot_key,summary,allocation_key,node_id) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(request_id,node_id) DO NOTHING`, g.RequestID, g.Revision, kind, uuid.NewString(), summary, key, nodeID)
	if err != nil {
		return err
	}
	if kind == "goal" || kind == "intent" {
		if _, err = tx.ExecContext(ctx, `UPDATE work_requests SET state='active',resolution='',updated_at=now() WHERE id=$1`, g.RequestID); err != nil {
			return err
		}
	}
	return tx.Commit()
}
