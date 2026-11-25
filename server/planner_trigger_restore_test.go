package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

func TestPlannerMainPreparationDeferralPreservesTriggersOnce(t *testing.T) {
	_, task := plannerBridgeFixture(t)
	message := makeMainOwnedFixture(t, task)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	task.NotifyGoal([]string{"Verify the synthetic report"})
	triggers, skipped := takePlannerTriggers(task, false)
	if skipped || len(triggers) != 1 {
		t.Fatalf("trigger not taken: %+v %v", triggers, skipped)
	}
	// This event arrives after the round drained its batch, before restoration.
	task.NotifyFinding(42, "Synthetic evidence arrived during the claim")
	batch, err := claimPlannerRoundMessages(ctx, task, false, triggers)
	if !errors.Is(err, db.ErrMainPreparationPending) || batch != nil {
		t.Fatalf("queued Main did not defer Planner: %+v %v", batch, err)
	}
	triggers, skipped = takePlannerTriggers(task, false)
	if skipped || len(triggers) != 2 || triggers[0].Kind != "goal" || triggers[1].Kind != "finding" || triggers[1].IntentID != 42 {
		t.Fatalf("deferred triggers lost or reordered: %+v", triggers)
	}
	message, err = task.Store.ClaimMainDelivery(ctx, message.ID)
	if err != nil || message == nil {
		t.Fatalf("Main claim failed: %+v %v", message, err)
	}
	if err := task.Store.HandoffMainRequest(ctx, message.ID, message.DeliveryToken); err != nil {
		t.Fatal(err)
	}
	batch, err = claimPlannerRoundMessages(ctx, task, true, triggers)
	if err != nil || batch == nil || len(batch.Messages) != 1 || batch.Messages[0].ID != message.ID {
		t.Fatalf("handoff not claimed: %+v %v", batch, err)
	}
	if repeated := task.drainTriggers(); len(repeated) != 0 {
		t.Fatalf("successful claim duplicated triggers: %+v", repeated)
	}
	if err := task.Store.FinishPlannerMessages(ctx, batch.Token, true); err != nil {
		t.Fatal(err)
	}
}

func TestPlannerUnavailableClaimRestoresTriggers(t *testing.T) {
	for _, reason := range []string{"busy_delivery", "cancelled_read"} {
		t.Run(reason, func(t *testing.T) {
			_, task := plannerBridgeFixture(t)
			message := makeMainOwnedFixture(t, task)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			message, err := task.Store.ClaimMainDelivery(ctx, message.ID)
			if err != nil || message == nil {
				t.Fatal(err)
			}
			if err := task.Store.HandoffMainRequest(ctx, message.ID, message.DeliveryToken); err != nil {
				t.Fatal(err)
			}
			if reason == "busy_delivery" {
				if batch, err := task.Store.ClaimPlannerMessages(ctx, 8); err != nil || batch == nil {
					t.Fatalf("first Planner claim failed: %+v %v", batch, err)
				}
			} else {
				cancel()
			}
			task.NotifyGoal([]string{"Preserve this synthetic trigger"})
			triggers, _ := takePlannerTriggers(task, false)
			batch, err := claimPlannerRoundMessages(ctx, task, true, triggers)
			if batch != nil || (reason == "cancelled_read" && err == nil) || (reason == "busy_delivery" && err != nil) {
				t.Fatalf("unexpected claim result: %+v %v", batch, err)
			}
			if restored := task.drainTriggers(); len(restored) != 1 || restored[0].Kind != "goal" {
				t.Fatalf("unavailable claim lost trigger: %+v", restored)
			}
			if duplicate := task.drainTriggers(); len(duplicate) != 0 {
				t.Fatalf("trigger restored twice: %+v", duplicate)
			}
		})
	}
}
