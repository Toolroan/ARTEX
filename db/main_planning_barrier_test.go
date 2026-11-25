package db

import (
	"encoding/json"
	"errors"
	"sync"
	"testing"
)

func TestMainPreparationWaitsForPlannerWithoutRevokingLiveLease(t *testing.T) {
	_, s := mainMessageFixture(t)
	older := workMessage(t, s, "planner-already-running", "planner")
	planner := claimWork(t, s, older)
	createWorkNode(t, s, planner, "intent", "An existing bounded fixture step")

	current := createMainMessage(t, s, mainMessageInput("continue-and-register"))
	current = finishMainRoute(t, s, current.ID, MainRouteDecision{Route: "control", Owner: "main", DecisionData: json.RawMessage(`{"kind":"control","control_tools":["continue_task","set_goals"]}`)})
	if m, err := s.ClaimMainDelivery(t.Context(), current.ID); err != nil || m != nil {
		t.Fatalf("Main overlapped the running Planner: %+v %v", m, err)
	}
	if err := s.CheckMainDelivery(t.Context(), planner.DeliveryToken); err != nil {
		t.Fatalf("queued Main request revoked Planner: %v", err)
	}
	if batch, err := s.ClaimPlannerMessages(t.Context(), 8); !errors.Is(err, ErrMainPreparationPending) || batch != nil {
		t.Fatalf("new Planner round bypassed queued Main: %+v %v", batch, err)
	}

	// A pure lifecycle request can still be delivered while Planner runs. It
	// does not acquire graph-write authority or invent a durable objective.
	pure := createMainMessage(t, s, mainMessageInput("continue-only"))
	pure = finishMainRoute(t, s, pure.ID, MainRouteDecision{Route: "control", Owner: "main", DecisionData: json.RawMessage(`{"kind":"control","control_tools":["continue_task"]}`)})
	if w, err := s.GetWorkRequest(t.Context(), pure.ID); err != nil || w != nil {
		t.Fatalf("pure Continue invented work: %+v %v", w, err)
	}
	for _, read := range []func() ([]int64, error){
		func() ([]int64, error) { return s.ReadyMainLifecycleDeliveryIDs(t.Context(), 1) },
		func() ([]int64, error) { return s.ReadyMainDeliveryIDs(t.Context(), 1) },
	} {
		ids, err := read()
		if err != nil || len(ids) != 1 || ids[0] != pure.ID {
			t.Fatalf("deferred preparation starved pure Continue: %v %v", ids, err)
		}
	}
	pure, err := s.ClaimMainDelivery(t.Context(), pure.ID)
	if err != nil || pure == nil {
		t.Fatalf("pure Continue blocked by Planner: %+v %v", pure, err)
	}
	if err := s.FinishMainDelivery(t.Context(), pure.ID, pure.DeliveryToken, true); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishPlannerMessages(t.Context(), planner.DeliveryToken, true); err != nil {
		t.Fatal(err)
	}
	ids, err := s.ReadyMainLifecycleDeliveryIDs(t.Context(), 1)
	if err != nil || len(ids) != 1 || ids[0] != current.ID {
		t.Fatalf("Main did not become ready after Planner finished: %v %v", ids, err)
	}

	main := claimWork(t, s, current)
	goalID := createWorkNode(t, s, main, "goal", "Verify the explicitly requested fixture outcome")
	if batch, err := s.ClaimPlannerMessages(t.Context(), 8); !errors.Is(err, ErrMainPreparationPending) || batch != nil {
		t.Fatalf("Planner raced Main goal preparation: %+v %v", batch, err)
	}
	if err := s.HandoffMainRequest(t.Context(), current.ID, main.DeliveryToken); err != nil {
		t.Fatal(err)
	}
	batch, err := s.ClaimPlannerMessages(t.Context(), 8)
	if err != nil || batch == nil {
		t.Fatalf("handoff did not release Planner: %+v %v", batch, err)
	}
	found := false
	for _, m := range batch.Messages {
		if m.ID == current.ID {
			found = true
		}
	}
	w, err := s.GetWorkRequest(t.Context(), current.ID)
	if err != nil || !found || w == nil || w.Revision != 1 || len(w.Nodes) != 1 || w.Nodes[0].NodeID != goalID {
		t.Fatalf("handoff replaced source or graph identity: %+v %v", w, err)
	}
	if err := s.CheckMainDelivery(t.Context(), main.DeliveryToken); !errors.Is(err, ErrMainMessageClaimLost) {
		t.Fatalf("Main lease remained valid after handoff: %v", err)
	}
}

func TestMainPreparationSerializesTrackedMainDeliveries(t *testing.T) {
	_, s := mainMessageFixture(t)
	first := workMessage(t, s, "first-main", "main")
	second := workMessage(t, s, "second-main", "main")
	grant := claimWork(t, s, first)
	if m, err := s.ClaimMainDelivery(t.Context(), second.ID); err != nil || m != nil {
		t.Fatalf("two Main preparations acquired overlapping leases: %+v %v", m, err)
	}
	if err := s.HandoffMainRequest(t.Context(), first.ID, grant.DeliveryToken); err != nil {
		t.Fatal(err)
	}
	if batch, err := s.ClaimPlannerMessages(t.Context(), 8); !errors.Is(err, ErrMainPreparationPending) || batch != nil {
		t.Fatalf("Planner skipped next queued Main preparation: %+v %v", batch, err)
	}
	claimWork(t, s, second)
}

func TestMainPreparationEnrollmentRacesPlannerClaimAtomically(t *testing.T) {
	_, s := mainMessageFixture(t)
	workMessage(t, s, "queued-planner", "planner")
	current := createMainMessage(t, s, mainMessageInput("new-main-plan"))
	current, err := s.ClaimMainRouting(t.Context(), current.ID)
	if err != nil || current == nil {
		t.Fatalf("routing claim: %+v %v", current, err)
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	var routed *MainMessage
	var routeErr, plannerErr error
	var batch *MainPlannerBatch
	wg.Go(func() {
		<-start
		routed, routeErr = s.FinishMainRouting(t.Context(), current.ID, current.RoutingToken, MainRouteDecision{Route: "action", Owner: "main"})
	})
	wg.Go(func() {
		<-start
		batch, plannerErr = s.ClaimPlannerMessages(t.Context(), 8)
	})
	close(start)
	wg.Wait()
	if routeErr != nil || routed == nil {
		t.Fatalf("Main enrollment failed: %+v %v", routed, routeErr)
	}
	main, err := s.ClaimMainDelivery(t.Context(), routed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if batch != nil {
		if plannerErr != nil || main != nil {
			t.Fatalf("Main overlapped Planner's winning lease: Main=%+v err=%v", main, plannerErr)
		}
		if err := s.CheckMainDelivery(t.Context(), batch.Token); err != nil {
			t.Fatalf("Main enrollment revoked a winning Planner lease: %v", err)
		}
	} else if !errors.Is(plannerErr, ErrMainPreparationPending) || main == nil {
		t.Fatalf("Main's winning enrollment did not defer Planner: Main=%+v err=%v", main, plannerErr)
	}
}
