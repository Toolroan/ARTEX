package db

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"
)

func plannerClaimMessage(t *testing.T, store *ExplorationStore, key string) *MainMessage {
	t.Helper()
	m := createMainMessage(t, store, mainMessageInput(key))
	return finishMainRoute(t, store, m.ID, MainRouteDecision{Route: "correction", Owner: "planner", DecisionData: json.RawMessage(`{}`)})
}

func TestPoolIntentClaimWaitsForQueuedAndInflightPlannerReview(t *testing.T) {
	d, store := mainMessageFixture(t)
	id := addClaimIntent(t, store)
	plannerClaimMessage(t, store, "pool-order")
	assertBlocked := func(phase string) {
		t.Helper()
		if token, err := store.ClaimIntentTokenAfterPlannerReview(t.Context(), id, "pool"); err != nil || token != "" {
			t.Fatalf("%s allowed pool claim: %q %v", phase, token, err)
		}
		if row := readClaimRow(t, d, id); row.state != "open" || row.token.Valid || row.heartbeatAge.Valid {
			t.Fatalf("%s changed unclaimed work: %+v", phase, row)
		}
	}
	assertBlocked("queued")
	batch, err := store.ClaimPlannerMessages(t.Context(), 8)
	if err != nil || batch == nil {
		t.Fatalf("claim inbox: %v %v", batch, err)
	}
	assertBlocked("inflight")
	if err := store.FinishPlannerMessages(t.Context(), batch.Token, true); err != nil {
		t.Fatal(err)
	}
	token, err := store.ClaimIntentTokenAfterPlannerReview(t.Context(), id, "pool")
	if err != nil || token == "" {
		t.Fatalf("reviewed instruction kept pool blocked: %q %v", token, err)
	}
	row := readClaimRow(t, d, id)
	if row.state != "running" || row.token.String != token || !row.heartbeatAge.Valid {
		t.Fatalf("reviewed claim did not retain ordinary lease semantics: %+v", row)
	}
	if err := store.HeartbeatIntentClaim(id, token); err != nil {
		t.Fatal(err)
	}
}

func TestPoolIntentClaimDoesNotBlockOnNeedsReviewOrOtherTask(t *testing.T) {
	_, store := mainMessageFixture(t)
	_, other := mainMessageFixture(t)
	plannerClaimMessage(t, other, "other-task-pending")
	id := addClaimIntent(t, store)
	if token, err := store.ClaimIntentTokenAfterPlannerReview(t.Context(), id, "pool"); err != nil || token == "" {
		t.Fatalf("another task blocked claim: %q %v", token, err)
	}
	plannerClaimMessage(t, store, "uncertain-review")
	batch, err := store.ClaimPlannerMessages(t.Context(), 8)
	if err != nil || batch == nil {
		t.Fatalf("claim inbox: %v %v", batch, err)
	}
	if err := store.FinishPlannerMessages(t.Context(), batch.Token, false); err != nil {
		t.Fatal(err)
	}
	id = addClaimIntent(t, store)
	if token, err := store.ClaimIntentTokenAfterPlannerReview(t.Context(), id, "pool"); err != nil || token == "" {
		t.Fatalf("needs_review became a permanent pool barrier: %q %v", token, err)
	}
}

func TestPoolIntentClaimLeavesExplicitOperatorClaimUnchanged(t *testing.T) {
	_, store := mainMessageFixture(t)
	plannerClaimMessage(t, store, "pending-before-direct-control")
	id := addClaimIntent(t, store)
	if token, err := store.ClaimIntentTokenAfterPlannerReview(t.Context(), id, "pool"); err != nil || token != "" {
		t.Fatalf("pending correction did not block pool: %q %v", token, err)
	}
	if token, err := store.ClaimIntentToken(id, "explicit-operator"); err != nil || token == "" {
		t.Fatalf("explicit operator claim changed: %q %v", token, err)
	}
}

func TestPoolIntentClaimIsExclusiveWithoutPendingInbox(t *testing.T) {
	_, store := mainMessageFixture(t)
	id := addClaimIntent(t, store)
	var wg sync.WaitGroup
	tokens := make(chan string, 12)
	errs := make(chan error, 12)
	for range 12 {
		wg.Go(func() {
			token, err := store.ClaimIntentTokenAfterPlannerReview(t.Context(), id, "pool")
			if err != nil {
				errs <- err
			}
			if token != "" {
				tokens <- token
			}
		})
	}
	wg.Wait()
	close(tokens)
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if len(tokens) != 1 {
		t.Fatalf("concurrent claims=%d, want exactly one", len(tokens))
	}
}

func TestPoolIntentClaimContextBoundsLockedRow(t *testing.T) {
	d, store := mainMessageFixture(t)
	id := addClaimIntent(t, store)
	tx, err := d.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(t.Context(), `SELECT id FROM exploration_nodes WHERE id=$1 FOR UPDATE`, id); err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer stop()
	start := time.Now()
	token, err := store.ClaimIntentTokenAfterPlannerReview(ctx, id, "pool")
	if err == nil || token != "" || ctx.Err() == nil || time.Since(start) > time.Second {
		t.Fatalf("locked row ignored deadline: token=%q err=%v ctx=%v elapsed=%v", token, err, ctx.Err(), time.Since(start))
	}
}
