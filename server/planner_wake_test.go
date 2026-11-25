package server

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Autumn-27/artex/agent"
)

func TestPlannerTriggerBatchPrioritizesHandoffAndPreservesLaterArrivals(t *testing.T) {
	for _, origin := range []string{"human", "mainagent", "worker-1", "orchestrator", "system"} {
		t.Run(origin, func(t *testing.T) {
			task := &Task{notify: make(chan struct{}, 1)}
			task.pendingTriggers = []agent.TriggerEvent{{Kind: "done", IntentID: 1}}
			task.NotifyHint(origin, []string{"first correction"})
			batch, skipped := takePlannerTriggers(task, true)
			if skipped != (origin != "human" && origin != "mainagent") || len(batch) != 2 {
				t.Fatalf("batch=%#v skipped=%v", batch, skipped)
			}
			if batch[1].HintOrigin != origin {
				t.Fatal("scheduling changed hint provenance")
			}
			// An arrival after the atomic take is a separate batch, never
			// consumed by the Plan call that owns the first batch.
			task.NotifyHint("human", []string{"later correction"})
			if len(batch) != 2 {
				t.Fatal("detached trigger batch changed")
			}
			next, skipped := takePlannerTriggers(task, true)
			if skipped || len(next) != 1 || next[0].Hints[0] != "later correction" {
				t.Fatalf("later arrival lost: %#v", next)
			}
		})
	}
}

func TestPlannerQuietEdgesDoNotPostponeHeartbeat(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		notify := make(chan struct{}, 1)
		var heartbeatAt time.Time
		started := time.Now()
		go runPlannerWakeLoop(ctx, notify, 10*time.Minute, func(src string) bool {
			if src == "heartbeat" {
				heartbeatAt = time.Now()
				cancel()
				return false
			}
			return true
		})
		synctest.Wait()
		for range 9 {
			time.Sleep(time.Minute)
			notify <- struct{}{}
			synctest.Wait()
		}
		time.Sleep(time.Minute)
		synctest.Wait()
		if elapsed := heartbeatAt.Sub(started); elapsed != 10*time.Minute {
			t.Fatalf("quiet edges moved heartbeat to %v", elapsed)
		}
	})
}

func TestPlannerActualEdgeRestartsHeartbeat(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		notify := make(chan struct{}, 1)
		var heartbeatAt time.Time
		started := time.Now()
		go runPlannerWakeLoop(ctx, notify, 10*time.Minute, func(src string) bool {
			if src == "heartbeat" {
				heartbeatAt = time.Now()
				cancel()
			}
			return false
		})
		synctest.Wait()
		time.Sleep(5 * time.Minute)
		notify <- struct{}{}
		synctest.Wait()
		time.Sleep(10 * time.Minute)
		synctest.Wait()
		if elapsed := heartbeatAt.Sub(started); elapsed != 15*time.Minute {
			t.Fatalf("normal edge failed to restart heartbeat: %v", elapsed)
		}
	})
}
