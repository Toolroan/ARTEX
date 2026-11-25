package server

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

func statusFixtureMessage(t *testing.T, task *Task, key, route, owner string, segment int) *db.MainMessage {
	t.Helper()
	msg, _, err := task.Store.CreateMainMessage(t.Context(), db.MainMessageInput{Segment: segment, RequestKey: key, Mode: "auto", InputSource: "main", RawText: "PRIVATE SYNTHETIC BODY", OperatorText: "PRIVATE SYNTHETIC BODY"})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := task.Store.ClaimMainRouting(t.Context(), msg.ID)
	if err != nil || claimed == nil {
		t.Fatalf("claim: %v %v", claimed, err)
	}
	msg, err = task.Store.FinishMainRouting(t.Context(), msg.ID, claimed.RoutingToken, db.MainRouteDecision{Route: route, Owner: owner, DecisionData: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	return msg
}

func TestInstructionStatusReaderTracksDeliveryWithoutMutating(t *testing.T) {
	s, task := plannerBridgeFixture(t)
	s.engine.SetAuthoritativeAgentResolver(func(*Task) (*agent.Planner, *agent.Worker) {
		t.Fatal("status read called the unbounded provider resolver")
		return nil, nil
	})
	msg := statusFixtureMessage(t, task, "status-action", "action", "planner", 0)
	current := statusFixtureMessage(t, task, "status-question", "context", "", 0)
	reader := s.instructionStatusReader(task, 0, current.ID)
	check := func(want string) agent.InstructionStatusPage {
		t.Helper()
		page, err := reader(t.Context(), []int64{msg.ID})
		if err != nil || len(page.Items) != 1 || page.Items[0].Delivery != want || !page.ApplicationUnverified {
			t.Fatalf("want=%s page=%+v err=%v", want, page, err)
		}
		if page.CurrentMessageID != current.ID || page.ObservedAt.Location() != time.UTC || time.Since(page.ObservedAt) > time.Second || !page.EngineStarted || page.PlannerAvailability != "unknown" {
			t.Fatalf("stale or wrong task metadata: %+v", page)
		}
		raw, _ := json.Marshal(page)
		if strings.Contains(string(raw), "PRIVATE SYNTHETIC BODY") {
			t.Fatal("status read included conversation content")
		}
		if strings.Contains(string(raw), `"planner_available"`) {
			t.Fatal("unknown provider availability was serialized as a known boolean")
		}
		return page
	}
	check("queued")
	batch, err := task.Store.ClaimPlannerMessages(t.Context(), 8)
	if err != nil || batch == nil {
		t.Fatalf("claim: %v %v", batch, err)
	}
	check("reviewing")
	if err := task.Store.FinishPlannerMessages(t.Context(), batch.Token, true); err != nil {
		t.Fatal(err)
	}
	check("presented")
	page, err := reader(t.Context(), nil)
	if err != nil || len(page.Items) != 2 || page.Items[0].MessageID != current.ID || page.Items[1].MessageID != msg.ID {
		t.Fatalf("default omitted current/prior action: %+v %v", page, err)
	}
	stored, err := task.Store.GetMainMessage(t.Context(), msg.ID)
	if err != nil || stored.DeliveryStatus != "presented" || stored.DeliveryToken != "" {
		t.Fatalf("read changed delivery: %+v %v", stored, err)
	}
}

func TestInstructionStatusReaderReportsWaitingAndExpiredLease(t *testing.T) {
	s, task := plannerBridgeFixture(t)
	msg := statusFixtureMessage(t, task, "status-wait", "correction", "planner", 0)
	reader := s.instructionStatusReader(task, 0, msg.ID)
	task.updateLifecycle(func(state *taskLifecycleState) { state.Paused = true })
	page, err := reader(t.Context(), nil)
	if err != nil || !page.TaskPaused || page.Items[0].Delivery != "waiting" || page.WaitingReason == "" {
		t.Fatalf("pause omitted: %+v %v", page, err)
	}
	task.updateLifecycle(func(state *taskLifecycleState) { state.Paused = false })
	batch, err := task.Store.ClaimPlannerMessages(t.Context(), 8)
	if err != nil || batch == nil {
		t.Fatalf("claim: %v %v", batch, err)
	}
	_, err = s.m.pg.Exec(`UPDATE main_message_routes SET delivery_until=now()-interval '1 second' WHERE exploration_id=$1 AND id=$2`, task.ExpID, msg.ID)
	if err != nil {
		t.Fatal(err)
	}
	page, err = reader(t.Context(), nil)
	if err != nil || page.Items[0].Delivery != "needs_review" || !page.ApplicationUnverified {
		t.Fatalf("expired lease claimed reviewing: %+v %v", page, err)
	}
	stored, err := task.Store.GetMainMessage(t.Context(), msg.ID)
	if err != nil || stored.DeliveryStatus != "inflight" {
		t.Fatalf("read changed persisted delivery: %+v %v", stored, err)
	}
	stored.DeliveryUntil = nil
	if receipt := s.messageReceipt(task, stored); receipt.Delivery != "needs_review" {
		t.Fatalf("missing lease claimed reviewing: %+v", receipt)
	}
	other := statusFixtureMessage(t, task, "status-other-segment", "context", "", 1)
	if _, err := reader(t.Context(), []int64{other.ID}); err == nil {
		t.Fatal("read leaked another Main segment")
	}
}

func TestInstructionStatusReaderDeadlineCoversPoolWaitAndEligibility(t *testing.T) {
	for _, blocked := range []string{"pool", "eligibility"} {
		t.Run(blocked, func(t *testing.T) {
			s, task := plannerBridgeFixture(t)
			msg := statusFixtureMessage(t, task, "deadline-fixture", "action", "planner", 0)
			reader := s.instructionStatusReader(task, 0, msg.ID)
			s.engine.SetAuthoritativeAgentResolver(func(*Task) (*agent.Planner, *agent.Worker) {
				t.Fatal("status tried to resolve a provider while checking metadata")
				return nil, nil
			})
			if blocked == "pool" {
				previousLimit := s.m.pg.Stats().MaxOpenConnections
				s.m.pg.SetMaxOpenConns(1)
				defer s.m.pg.SetMaxOpenConns(previousLimit)
				held, err := s.m.pg.Conn(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				defer held.Close()
			} else {
				// The metadata row query can complete, then the eligibility query
				// must honor its deadline while blocked on exploration_nodes.
				// A separate safety deadline releases the fixture lock even if the
				// implementation regresses to a contextless query.
				lockCtx, stopLock := context.WithTimeout(t.Context(), 2*time.Second)
				defer stopLock()
				locked, err := s.m.pg.BeginTx(lockCtx, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer locked.Rollback()
				if _, err := locked.ExecContext(lockCtx, `LOCK TABLE exploration_nodes IN ACCESS EXCLUSIVE MODE`); err != nil {
					t.Fatal(err)
				}
			}
			ctx, stop := context.WithTimeout(t.Context(), 75*time.Millisecond)
			defer stop()
			began := time.Now()
			_, err := reader(ctx, nil)
			if err == nil || !errors.Is(ctx.Err(), context.DeadlineExceeded) || time.Since(began) > time.Second {
				t.Fatalf("unbounded %s read: elapsed=%s err=%v context=%v", blocked, time.Since(began), err, ctx.Err())
			}
		})
	}
}
