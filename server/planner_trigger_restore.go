package server

import (
	"context"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

// A delivery claim can defer a round after its quiet-mode decision has consumed
// triggers. Preserve those events until a round actually acquires its inbox.
// Do not notify here: Main handoff or the ordinary heartbeat retries the claim,
// without creating a busy loop while Main is preparing its request.
func claimPlannerRoundMessages(ctx context.Context, t *Task, queued bool, triggers []agent.TriggerEvent) (*db.MainPlannerBatch, error) {
	batch, err := t.Store.ClaimPlannerMessages(ctx, 8)
	if err != nil || (queued && batch == nil) {
		t.restorePlannerTriggers(triggers)
	}
	return batch, err
}

// Older consumed events precede events that arrived while the claim was being
// checked. The caller transfers the drained slice back exactly once.
func (t *Task) restorePlannerTriggers(triggers []agent.TriggerEvent) {
	if len(triggers) == 0 {
		return
	}
	t.trigMu.Lock()
	defer t.trigMu.Unlock()
	restored := make([]agent.TriggerEvent, 0, len(triggers)+len(t.pendingTriggers))
	restored = append(restored, triggers...)
	t.pendingTriggers = append(restored, t.pendingTriggers...)
}
