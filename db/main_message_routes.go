package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

const (
	MainRoutingMaxAttempts = 3
	MainRoutingLease       = 2 * time.Minute
	MainDeliveryLease      = 90 * time.Second
	mainContextMaxBytes    = 64 << 10
)

var (
	ErrMainMessageConflict    = errors.New("request key already belongs to a different Main message")
	ErrMainMessageClaimLost   = errors.New("Main message claim is no longer current")
	ErrMainMessageParentGone  = errors.New("Main message task is missing, archived, or deleted")
	ErrMainPreparationPending = errors.New("Main is preparing requested work before Planner can claim another round")
)

type MainMessageInput struct {
	Segment      int
	RequestKey   string
	Mode         string
	InputSource  string
	RawText      string
	OperatorText string          // original operator text plus server-validated attachment paths
	Context      json.RawMessage // optional immutable snapshot; nil builds bounded shared context
	UserActivity *Activity       // optional display metadata; ownership/author/text are set by the store
}

type MainRouteDecision struct {
	Route         string          `json:"route"`
	Summary       string          `json:"summary"`
	Clarification string          `json:"clarification"`
	Owner         string          `json:"owner"`
	DecisionData  json.RawMessage `json:"decision_data"` // model-derived, never operator authority
}

type MainMessage struct {
	ID             int64           `json:"id"`
	ExplorationID  int64           `json:"exploration_id"`
	Segment        int             `json:"segment"`
	RequestKey     string          `json:"request_key"`
	Mode           string          `json:"mode"`
	InputSource    string          `json:"input_source"`
	RawText        string          `json:"raw_text"`
	OperatorText   string          `json:"operator_text"`
	Context        json.RawMessage `json:"context"`
	ActivityID     int64           `json:"activity_id,omitempty"`
	UserActivity   *Activity       `json:"-"`
	AssistantText  string          `json:"assistant_text,omitempty"`
	Route          string          `json:"route"`
	RoutingStatus  string          `json:"routing_status"`
	RoutingToken   string          `json:"-"`
	RoutingUntil   *time.Time      `json:"routing_until,omitempty"`
	Attempts       int             `json:"attempts"`
	RetryAfter     time.Time       `json:"retry_after"`
	Summary        string          `json:"summary,omitempty"`
	Clarification  string          `json:"clarification,omitempty"`
	DecisionData   json.RawMessage `json:"decision_data"`
	DeliveryStatus string          `json:"delivery_status"`
	DeliveryOwner  string          `json:"delivery_owner"`
	DeliveryToken  string          `json:"-"`
	DeliveryUntil  *time.Time      `json:"delivery_until,omitempty"`
	Error          string          `json:"error,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
}

// Context IDs are stable and distinct: new user=2*message ID, assistant=2*ID+1,
// legacy activity=-activity ID. All entries are historical context, not fresh
// operator commands. Source distinguishes stored chat from legacy display text.
type MainConversationEntry struct {
	ID         int64  `json:"id"`
	MessageID  int64  `json:"message_id,omitempty"`
	ActivityID int64  `json:"activity_id,omitempty"`
	Segment    int    `json:"segment"`
	Role       string `json:"role"`
	Text       string `json:"text"`
	Source     string `json:"source"`
	Truncated  bool   `json:"truncated,omitempty"`
}

type MainPlannerBatch struct {
	Token    string        `json:"-"`
	Messages []MainMessage `json:"messages"`
}

const mainMessageCols = `id,exploration_id,main_seg,request_key,mode,input_source,raw_text,operator_text,context_snapshot,COALESCE(activity_id,0),assistant_text,route,routing_status,routing_token,routing_until,attempts,retry_after,summary,clarification,decision_data,delivery_status,delivery_owner,delivery_token,delivery_until,last_error,created_at,updated_at`

func scanMainMessage(row interface{ Scan(...any) error }) (*MainMessage, error) {
	m := new(MainMessage)
	err := row.Scan(&m.ID, &m.ExplorationID, &m.Segment, &m.RequestKey, &m.Mode, &m.InputSource, &m.RawText, &m.OperatorText, &m.Context, &m.ActivityID, &m.AssistantText, &m.Route, &m.RoutingStatus, &m.RoutingToken, &m.RoutingUntil, &m.Attempts, &m.RetryAfter, &m.Summary, &m.Clarification, &m.DecisionData, &m.DeliveryStatus, &m.DeliveryOwner, &m.DeliveryToken, &m.DeliveryUntil, &m.Error, &m.CreatedAt, &m.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return m, err
}

func validMainText(text string, maxBytes int) bool {
	return len(text) <= maxBytes && utf8.ValidString(text) && !strings.ContainsRune(text, '\x00')
}

func validateMainInput(in MainMessageInput) error {
	if in.Segment < 0 || len(in.RequestKey) == 0 || len(in.RequestKey) > 128 {
		return errors.New("invalid Main message segment or request key")
	}
	for _, r := range in.RequestKey {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_.:", r)) {
			return errors.New("invalid Main message request key")
		}
	}
	if in.Mode != "auto" && in.Mode != "context" && in.Mode != "instruction" {
		return errors.New("invalid Main message mode")
	}
	if in.InputSource != "main" && in.InputSource != "direct_planner" {
		return errors.New("invalid Main message source")
	}
	if strings.TrimSpace(in.OperatorText) == "" || !validMainText(in.RawText, 256<<10) || !validMainText(in.OperatorText, 256<<10) {
		return errors.New("invalid Main message text")
	}
	return nil
}

// Locking the parent serializes acceptance/claims with task archive/delete and
// concurrent acceptance from another app instance. No transaction spans an LLM.
func (s *ExplorationStore) lockMainParent(ctx context.Context, tx *sql.Tx) error {
	var id int64
	err := tx.QueryRowContext(ctx, `SELECT id FROM tasks WHERE exploration_id=$1 AND deleted_at IS NULL AND archived_at IS NULL FOR UPDATE`, s.expID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrMainMessageParentGone
	}
	return err
}

// CreateMainMessage atomically stores acceptance and its user activity. A replay
// returns the original snapshot before consulting current history. Only caller
// input (including segment/mode/source and validated paths) is compared.
func (s *ExplorationStore) CreateMainMessage(ctx context.Context, in MainMessageInput) (*MainMessage, bool, error) {
	if err := validateMainInput(in); err != nil {
		return nil, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	if err = s.lockMainParent(ctx, tx); err != nil {
		return nil, false, err
	}
	existing, err := scanMainMessage(tx.QueryRowContext(ctx, `SELECT `+mainMessageCols+` FROM main_message_routes WHERE exploration_id=$1 AND request_key=$2`, s.expID, in.RequestKey))
	if err != nil {
		return nil, false, err
	}
	if existing != nil {
		if existing.Segment != in.Segment || existing.Mode != in.Mode || existing.InputSource != in.InputSource || existing.RawText != in.RawText || existing.OperatorText != in.OperatorText {
			return nil, false, ErrMainMessageConflict
		}
		return existing, false, tx.Commit()
	}
	snapshot := in.Context
	if len(snapshot) == 0 {
		entries, e := s.recentMainConversation(ctx, tx, in.Segment, 0, 20)
		if e != nil {
			return nil, false, e
		}
		snapshot, err = json.Marshal(entries)
		if err != nil {
			return nil, false, err
		}
	}
	if len(snapshot) > 1<<20 || !json.Valid(snapshot) {
		return nil, false, errors.New("invalid or oversized Main context snapshot")
	}
	m, err := scanMainMessage(tx.QueryRowContext(ctx, `INSERT INTO main_message_routes(exploration_id,main_seg,request_key,mode,input_source,raw_text,operator_text,context_snapshot) VALUES($1,$2,$3,$4,$5,$6,$7,$8) RETURNING `+mainMessageCols, s.expID, in.Segment, in.RequestKey, in.Mode, in.InputSource, in.RawText, in.OperatorText, string(snapshot)))
	if err != nil {
		return nil, false, err
	}
	a := Activity{}
	if in.UserActivity != nil {
		a = *in.UserActivity
	}
	a.ID = 0
	a.NodeID = nil
	a.Tool = ""
	a.ToolUseID = ""
	a.IsError = false
	a.Kind = "user"
	a.Summary = in.RawText
	a.Worker = "mainagent"
	a.MainSeg = &in.Segment
	if in.InputSource == "direct_planner" {
		a.Worker = "planner"
		a.MainSeg = nil
	}
	a.ID, err = s.appendActivity(tx, a)
	if err != nil {
		return nil, false, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE main_message_routes SET activity_id=$1 WHERE id=$2`, a.ID, m.ID); err != nil {
		return nil, false, err
	}
	m.ActivityID = a.ID
	m.UserActivity = &a
	return m, true, tx.Commit()
}

func (s *ExplorationStore) GetMainMessage(ctx context.Context, id int64) (*MainMessage, error) {
	return scanMainMessage(s.db.QueryRowContext(ctx, `SELECT `+mainMessageCols+` FROM main_message_routes WHERE exploration_id=$1 AND id=$2`, s.expID, id))
}

func (s *ExplorationStore) ExistingMainMessage(ctx context.Context, requestKey string) (*MainMessage, error) {
	return scanMainMessage(s.db.QueryRowContext(ctx, `SELECT `+mainMessageCols+` FROM main_message_routes WHERE exploration_id=$1 AND request_key=$2`, s.expID, requestKey))
}

func mainPageLimit(limit int) int {
	if limit <= 0 {
		return 20
	}
	return min(limit, 100)
}

func (s *ExplorationStore) ListMainMessages(ctx context.Context, seg int, before int64, limit int) ([]MainMessage, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+mainMessageCols+` FROM main_message_routes WHERE exploration_id=$1 AND main_seg=$2 AND ($3::bigint=0 OR id<$3) ORDER BY id DESC LIMIT $4`, s.expID, seg, before, mainPageLimit(limit))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MainMessage{}
	for rows.Next() {
		m, e := scanMainMessage(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

type mainContextQuery interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// RecentMainConversation returns ascending user/final-assistant entries. before
// refers to a message row ID. Legacy text is read only before the first new row
// in this segment, preventing duplicate imported context or cross-segment reads.
func (s *ExplorationStore) RecentMainConversation(ctx context.Context, seg int, before int64, limit int) ([]MainConversationEntry, error) {
	return s.recentMainConversation(ctx, s.db, seg, before, limit)
}

func (s *ExplorationStore) recentMainConversation(ctx context.Context, q mainContextQuery, seg int, before int64, limit int) ([]MainConversationEntry, error) {
	limit = mainPageLimit(limit)
	rows, err := q.QueryContext(ctx, `SELECT id,operator_text,assistant_text,input_source FROM main_message_routes WHERE exploration_id=$1 AND main_seg=$2 AND ($3::bigint=0 OR id<$3) ORDER BY id DESC LIMIT $4`, s.expID, seg, before, limit)
	if err != nil {
		return nil, err
	}
	var reversed []MainConversationEntry
	for rows.Next() {
		var id int64
		var user, assistant, source string
		if err = rows.Scan(&id, &user, &assistant, &source); err != nil {
			rows.Close()
			return nil, err
		}
		if assistant != "" {
			reversed = append(reversed, MainConversationEntry{ID: 2*id + 1, MessageID: id, Segment: seg, Role: "assistant", Text: assistant, Source: source})
		}
		reversed = append(reversed, MainConversationEntry{ID: 2 * id, MessageID: id, Segment: seg, Role: "user", Text: user, Source: source})
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	// Only fill missing space from the genuinely legacy prefix. New activity
	// rows are already represented above and never included by this fallback.
	if len(reversed) < 2*limit {
		var cutoff int64
		err = q.QueryRowContext(ctx, `SELECT COALESCE(MIN(activity_id),0) FROM main_message_routes WHERE exploration_id=$1 AND main_seg=$2`, s.expID, seg).Scan(&cutoff)
		if err != nil {
			return nil, err
		}
		legacy, e := q.QueryContext(ctx, `WITH recent AS (
SELECT id,kind,COALESCE(NULLIF(detail,''),summary,'') AS text FROM activity
WHERE exploration_id=$1 AND worker='mainagent' AND COALESCE(main_seg,0)=$2
AND kind IN ('user','result','text') AND is_error=false AND ($3::bigint=0 OR id<$3)
AND COALESCE(tool,'')='' ORDER BY id DESC LIMIT $4
), turns AS (
SELECT *,SUM(CASE WHEN kind='user' THEN 1 ELSE 0 END) OVER (ORDER BY id) AS turn FROM recent
), ranked AS (
SELECT *,row_number() OVER (PARTITION BY turn,(kind='user') ORDER BY id DESC) AS rank FROM turns
)
SELECT id,kind,text FROM ranked WHERE kind='user' OR rank=1 ORDER BY id DESC LIMIT $5`, s.expID, seg, cutoff, 8*limit, 2*limit-len(reversed))
		if e != nil {
			return nil, e
		}
		for legacy.Next() {
			var id int64
			var kind, text string
			if e = legacy.Scan(&id, &kind, &text); e != nil {
				legacy.Close()
				return nil, e
			}
			role := "assistant"
			if kind == "user" {
				role = "user"
			}
			reversed = append(reversed, MainConversationEntry{ID: -id, ActivityID: id, Segment: seg, Role: role, Text: text, Source: "legacy_main"})
		}
		e = legacy.Err()
		legacy.Close()
		if e != nil {
			return nil, e
		}
	}
	// Bound both number and bytes. Retain the newest useful tail and flag an
	// individually truncated entry rather than pretending it is complete.
	budget := mainContextMaxBytes
	keep := reversed[:0]
	for _, entry := range reversed {
		if budget <= 0 {
			break
		}
		if len(entry.Text) > budget {
			entry.Text = entry.Text[:budget]
			for !utf8.ValidString(entry.Text) {
				entry.Text = entry.Text[:len(entry.Text)-1]
			}
			entry.Truncated = true
		}
		budget -= len(entry.Text)
		keep = append(keep, entry)
	}
	out := make([]MainConversationEntry, len(keep))
	for i := range keep {
		out[len(keep)-1-i] = keep[i]
	}
	return out, nil
}

func (s *ExplorationStore) ClaimMainRouting(ctx context.Context, id int64) (*MainMessage, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err = s.lockMainParent(ctx, tx); err != nil {
		return nil, err
	}
	m, err := scanMainMessage(tx.QueryRowContext(ctx, `UPDATE main_message_routes SET routing_status='running',routing_token=$3,routing_until=now()+$4*INTERVAL '1 second',attempts=attempts+1,updated_at=now() WHERE exploration_id=$1 AND id=$2 AND routing_status IN ('pending','failed') AND attempts<$5 AND retry_after<=now() RETURNING `+mainMessageCols, s.expID, id, uuid.NewString(), MainRoutingLease.Seconds(), MainRoutingMaxAttempts))
	if err != nil {
		return nil, err
	}
	return m, tx.Commit()
}

func validateMainDecision(d *MainRouteDecision) error {
	switch d.Route {
	case "context", "clarify":
		d.Owner = ""
	case "action", "correction":
		if d.Owner == "" {
			d.Owner = "planner"
		}
		if d.Owner != "planner" && d.Owner != "main" {
			return errors.New("invalid action owner")
		}
		if d.Route == "correction" && d.Owner != "planner" {
			return errors.New("correction must belong to Planner")
		}
	case "control":
		if d.Owner == "" {
			d.Owner = "main"
		}
		if d.Owner != "main" {
			return errors.New("control must belong to Main")
		}
	default:
		return errors.New("invalid Main route")
	}
	if !validMainText(d.Summary, 32<<10) || !validMainText(d.Clarification, 32<<10) {
		return errors.New("invalid routing summary")
	}
	if len(d.DecisionData) == 0 {
		d.DecisionData = json.RawMessage(`{}`)
	}
	if len(d.DecisionData) > 64<<10 || !json.Valid(d.DecisionData) {
		return errors.New("invalid routing decision data")
	}
	return nil
}

func (s *ExplorationStore) FinishMainRouting(ctx context.Context, id int64, token string, d MainRouteDecision) (*MainMessage, error) {
	if err := validateMainDecision(&d); err != nil {
		return nil, err
	}
	delivery := "none"
	if d.Owner != "" {
		delivery = "queued"
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err = s.lockMainParent(ctx, tx); err != nil {
		return nil, err
	}
	m, err := scanMainMessage(tx.QueryRowContext(ctx, `UPDATE main_message_routes SET routing_status='routed',routing_token='',routing_until=NULL,route=$4,summary=$5,clarification=$6,decision_data=$7,delivery_owner=$8,delivery_status=$9,last_error='',updated_at=now() WHERE exploration_id=$1 AND id=$2 AND routing_status='running' AND routing_token=$3 AND routing_until>now() AND (mode<>'context' OR $4='context') RETURNING `+mainMessageCols, s.expID, id, token, d.Route, d.Summary, d.Clarification, string(d.DecisionData), d.Owner, delivery))
	if err == nil && m == nil {
		err = ErrMainMessageClaimLost
	}
	if err != nil {
		return m, err
	}
	if err = s.createRoutedWorkRequest(ctx, tx, m, d); err != nil {
		return nil, err
	}
	return m, tx.Commit()
}

func mainClaimResult(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrMainMessageClaimLost
	}
	return nil
}

func (s *ExplorationStore) FailMainRouting(ctx context.Context, id int64, token, reason string) error {
	if !validMainText(reason, 32<<10) {
		return errors.New("invalid routing failure")
	}
	res, err := s.db.ExecContext(ctx, `UPDATE main_message_routes SET routing_status='failed',routing_token='',routing_until=NULL,last_error=$4,retry_after=now()+INTERVAL '5 seconds',updated_at=now() WHERE exploration_id=$1 AND id=$2 AND routing_status='running' AND routing_token=$3 AND routing_until>now()`, s.expID, id, token, reason)
	return mainClaimResult(res, err)
}

// RetryMainRouting is an explicit operator retry of tool-free classification.
// It cannot reopen a routed decision or any uncertain effect-bearing delivery.
func (s *ExplorationStore) RetryMainRouting(ctx context.Context, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = s.lockMainParent(ctx, tx); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE main_message_routes SET routing_status='pending',routing_token='',routing_until=NULL,attempts=0,retry_after=now(),last_error='',updated_at=now() WHERE exploration_id=$1 AND id=$2 AND routing_status='failed' AND delivery_status='none'`, s.expID, id)
	if err = mainClaimResult(res, err); err != nil {
		return err
	}
	return tx.Commit()
}

// Final replies are write-once. A replay of the same text is harmless; a stale
// or duplicate model run cannot overwrite the conversation with another answer.
func (s *ExplorationStore) SaveMainReply(ctx context.Context, id int64, text string) error {
	if strings.TrimSpace(text) == "" || !validMainText(text, 2<<20) {
		return errors.New("invalid Main reply")
	}
	res, err := s.db.ExecContext(ctx, `UPDATE main_message_routes SET assistant_text=$3,updated_at=now() WHERE exploration_id=$1 AND id=$2 AND (assistant_text='' OR assistant_text=$3)`, s.expID, id, text)
	return mainClaimResult(res, err)
}

func (s *ExplorationStore) PendingMainRoutingIDs(ctx context.Context, limit int) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM main_message_routes WHERE exploration_id=$1 AND routing_status IN ('pending','failed') AND attempts<$2 AND retry_after<=now() ORDER BY id LIMIT $3`, s.expID, MainRoutingMaxAttempts, mainPageLimit(limit))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *ExplorationStore) HasQueuedPlannerMessages(ctx context.Context) (bool, error) {
	var found bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM main_message_routes WHERE exploration_id=$1 AND delivery_status='queued' AND delivery_owner='planner')`, s.expID).Scan(&found)
	return found, err
}

// Filter deferred tracked requests before LIMIT, so a Planner-owned round does
// not hide an eligible pure Continue command behind an older work request.
const mainDeliveryReadySQL = `NOT (EXISTS(SELECT 1 FROM work_requests w WHERE w.id=m.id AND w.state NOT IN ('completed','cancelled')) AND EXISTS(SELECT 1 FROM main_message_routes other WHERE other.exploration_id=m.exploration_id AND other.delivery_status='inflight' AND (other.delivery_owner='planner' OR (other.delivery_owner='main' AND other.id<>m.id AND EXISTS(SELECT 1 FROM work_requests w WHERE w.id=other.id AND w.state NOT IN ('completed','cancelled'))))))`

func (s *ExplorationStore) ReadyMainDeliveryIDs(ctx context.Context, limit int) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT m.id FROM main_message_routes m WHERE m.exploration_id=$1 AND m.delivery_status='queued' AND m.delivery_owner='main' AND `+mainDeliveryReadySQL+` ORDER BY m.id LIMIT $2`, s.expID, mainPageLimit(limit))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *ExplorationStore) ClaimPlannerMessages(ctx context.Context, limit int) (*MainPlannerBatch, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err = s.lockMainParent(ctx, tx); err != nil {
		return nil, err
	}
	// Main prepares the graph before Planner receives that same request. Return
	// an explicit deferral, not an empty inbox: callers must not treat this as
	// permission to begin an autonomous Planner round without a delivery batch.
	var preparing bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM main_message_routes m JOIN work_requests w ON w.id=m.id WHERE m.exploration_id=$1 AND m.delivery_owner='main' AND m.delivery_status IN ('queued','inflight') AND w.state NOT IN ('completed','cancelled'))`, s.expID).Scan(&preparing); err != nil {
		return nil, err
	}
	if preparing {
		return nil, ErrMainPreparationPending
	}
	// One Planner owns a task's delivery at a time. Engine lifecycle, goal,
	// budget, and scope gates remain the caller's responsibility.
	var busy bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM main_message_routes WHERE exploration_id=$1 AND delivery_owner='planner' AND delivery_status='inflight')`, s.expID).Scan(&busy); err != nil {
		return nil, err
	}
	if busy {
		return nil, tx.Commit()
	}
	batch := &MainPlannerBatch{Token: uuid.NewString(), Messages: []MainMessage{}}
	rows, err := tx.QueryContext(ctx, `UPDATE main_message_routes SET delivery_status='inflight',delivery_token=$2,delivery_until=now()+$3*INTERVAL '1 second',updated_at=now() WHERE id IN (SELECT id FROM main_message_routes m WHERE exploration_id=$1 AND delivery_owner='planner' AND (delivery_status='queued' OR (delivery_status='presented' AND EXISTS(SELECT 1 FROM work_requests w WHERE w.id=m.id AND w.state='active'))) ORDER BY (delivery_status='queued') DESC,id LIMIT $4 FOR UPDATE) RETURNING `+mainMessageCols, s.expID, batch.Token, MainDeliveryLease.Seconds(), mainPageLimit(limit))
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		m, e := scanMainMessage(rows)
		if e != nil {
			rows.Close()
			return nil, e
		}
		batch.Messages = append(batch.Messages, *m)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if len(batch.Messages) == 0 {
		return nil, tx.Commit()
	}
	ids := make([]int64, 0, len(batch.Messages))
	for _, m := range batch.Messages {
		ids = append(ids, m.ID)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE work_requests SET review_requested=false,updated_at=now() WHERE id=ANY($1::bigint[])`, ids); err != nil {
		return nil, err
	}
	sort.Slice(batch.Messages, func(i, j int) bool { return batch.Messages[i].ID < batch.Messages[j].ID })
	return batch, tx.Commit()
}

func (s *ExplorationStore) FinishPlannerMessages(ctx context.Context, token string, success bool) error {
	return s.finishPlannerWorkRequests(ctx, token, success)
}

func (s *ExplorationStore) ClaimMainDelivery(ctx context.Context, id int64) (*MainMessage, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err = s.lockMainParent(ctx, tx); err != nil {
		return nil, err
	}
	// A new tracked Main request waits for an already-running Planner round.
	// It does not revoke that round's grant. Pure lifecycle/admin messages have
	// no tracked work and remain usable while Planner is active.
	var busy bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM work_requests WHERE exploration_id=$1 AND id=$2 AND state NOT IN ('completed','cancelled')) AND EXISTS(SELECT 1 FROM main_message_routes m WHERE m.exploration_id=$1 AND m.delivery_status='inflight' AND (m.delivery_owner='planner' OR (m.delivery_owner='main' AND m.id<>$2 AND EXISTS(SELECT 1 FROM work_requests w WHERE w.id=m.id AND w.state NOT IN ('completed','cancelled')))))`, s.expID, id).Scan(&busy); err != nil {
		return nil, err
	}
	if busy {
		return nil, tx.Commit()
	}
	m, err := scanMainMessage(tx.QueryRowContext(ctx, `UPDATE main_message_routes SET delivery_status='inflight',delivery_token=$3,delivery_until=now()+$4*INTERVAL '1 second',updated_at=now() WHERE exploration_id=$1 AND id=$2 AND delivery_owner='main' AND delivery_status='queued' RETURNING `+mainMessageCols, s.expID, id, uuid.NewString(), MainDeliveryLease.Seconds()))
	if err != nil {
		return nil, err
	}
	return m, tx.Commit()
}

func (s *ExplorationStore) FinishMainDelivery(ctx context.Context, id int64, token string, success bool) error {
	state, reason := "presented", ""
	if !success {
		state, reason = "needs_review", "Main delivery was interrupted; review existing effects before retrying"
	}
	res, err := s.db.ExecContext(ctx, `UPDATE main_message_routes SET delivery_status=$4,delivery_token='',delivery_until=NULL,last_error=$5,updated_at=now() WHERE exploration_id=$1 AND id=$2 AND delivery_owner='main' AND delivery_status='inflight' AND delivery_token=$3 AND delivery_until>now()`, s.expID, id, token, state, reason)
	return mainClaimResult(res, err)
}

// RenewMainDelivery maintains a live Main or Planner lease. Callers heartbeat
// during execution and cancel their run when this fence can no longer renew.
func (s *ExplorationStore) RenewMainDelivery(ctx context.Context, token string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE main_message_routes SET delivery_until=now()+$3*INTERVAL '1 second',updated_at=now() WHERE exploration_id=$1 AND delivery_token=$2 AND delivery_status='inflight' AND delivery_until>now()`, s.expID, token, MainDeliveryLease.Seconds())
	return mainClaimResult(res, err)
}

// CheckMainDelivery checks ownership immediately before an effect is admitted.
// Database time avoids trusting the app instance's clock. Already-running
// external effects cannot be revoked by this check or by lease cancellation.
func (s *ExplorationStore) CheckMainDelivery(ctx context.Context, token string) error {
	var current bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM main_message_routes WHERE exploration_id=$1 AND delivery_token=$2 AND delivery_status='inflight' AND delivery_until>now())`, s.expID, token).Scan(&current)
	if err != nil {
		return err
	}
	if !current {
		return ErrMainMessageClaimLost
	}
	return nil
}

// Only tool-free routing is automatically retried. Expired effect-bearing
// delivery is explicitly uncertain, never silently requeued after a restart.
func (s *ExplorationStore) RecoverMainRouting(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = s.lockMainParent(ctx, tx); err != nil {
		return err
	}
	if err = s.blockInterruptedWorkRequests(ctx, tx); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE main_message_routes SET routing_status=CASE WHEN attempts<$2 THEN 'pending' ELSE 'failed' END,routing_token='',routing_until=NULL,retry_after=now(),last_error='Routing lease expired before a decision was committed',updated_at=now() WHERE exploration_id=$1 AND routing_status='running' AND routing_until<=now()`, s.expID, MainRoutingMaxAttempts); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE main_message_routes SET delivery_status='needs_review',delivery_token='',delivery_until=NULL,last_error='Delivery lease expired; review existing effects before retrying',updated_at=now() WHERE exploration_id=$1 AND delivery_status='inflight' AND delivery_until<=now()`, s.expID); err != nil {
		return err
	}
	return tx.Commit()
}
