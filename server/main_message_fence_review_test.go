package server

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

func TestMainMessageMutationSerializesWithPause(t *testing.T) {
	s, task := plannerBridgeFixture(t)
	m, _, err := task.Store.CreateMainMessage(t.Context(), db.MainMessageInput{
		Segment: 0, RequestKey: "pause-fence-fixture", Mode: "auto", InputSource: "main",
		RawText: "Register this fixture constraint", OperatorText: "Register this fixture constraint",
	})
	if err != nil {
		t.Fatal(err)
	}
	m, err = task.Store.ClaimMainRouting(t.Context(), m.ID)
	if err != nil || m == nil {
		t.Fatalf("claim routing: %v", err)
	}
	m, err = task.Store.FinishMainRouting(t.Context(), m.ID, m.RoutingToken, db.MainRouteDecision{
		Route: "control", Owner: "main", DecisionData: json.RawMessage(`{"control_tools":["set_constraints"]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	m, err = task.Store.ClaimMainDelivery(t.Context(), m.ID)
	if err != nil || m == nil {
		t.Fatalf("claim delivery: %v", err)
	}
	policy := s.mainMutationPolicy(task, m)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	releaseMutation := func() { once.Do(func() { close(release) }) }
	defer releaseMutation()
	mutationDone := make(chan error, 1)
	go func() {
		_, runErr := policy.Execute(t.Context(), "set_constraints", json.RawMessage(`{}`), func() (actool.Result, error) {
			close(entered)
			<-release
			return actool.Text("synthetic canonical mutation completed"), nil
		})
		mutationDone <- runErr
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("mutation did not enter")
	}
	pauseDone := make(chan error, 1)
	go func() { _, pauseErr := s.applyTaskControl(task, "pause"); pauseDone <- pauseErr }()
	select {
	case pauseErr := <-pauseDone:
		t.Fatalf("pause passed the admitted mutation: %v", pauseErr)
	case <-time.After(25 * time.Millisecond):
	}
	releaseMutation()
	select {
	case runErr := <-mutationDone:
		if runErr != nil {
			t.Fatal(runErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("mutation did not release scheduler lock")
	}
	select {
	case pauseErr := <-pauseDone:
		if pauseErr != nil {
			t.Fatal(pauseErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pause did not commit after mutation")
	}
	called := false
	result, err := policy.Execute(t.Context(), "set_constraints", json.RawMessage(`{}`), func() (actool.Result, error) {
		called = true
		return actool.Text("must not execute"), nil
	})
	if err != nil || !result.IsError || called || !task.lifecycleSnapshot().Paused {
		t.Fatalf("paused request passed the mutation fence: called=%v result=%+v err=%v", called, result, err)
	}
}
