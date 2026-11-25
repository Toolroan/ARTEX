package agent

import (
	"context"
	"iter"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
)

type workerControlKey struct{}

type workerControl struct {
	before   func(llm.CompletionRequest) llm.CompletionRequest
	finish   func(canContinue bool) (string, bool)
	lastStop string
}

// WithWorkerControl connects the engine's correction queue to worker model and
// completion boundaries. It is worker-only: compaction and side questions must
// not consume a correction intended for the executing worker.
func WithWorkerControl(ctx context.Context, before func(llm.CompletionRequest) llm.CompletionRequest, finish func(bool) (string, bool)) context.Context {
	return context.WithValue(ctx, workerControlKey{}, &workerControl{before: before, finish: finish})
}

// Install after side capture so its existing request callbacks remain intact.
// QueryDeps wraps actual worker requests, not the provider's compaction calls.
func attachWorkerControl(ctx context.Context, opts *agentcore.Options) *workerControl {
	c, _ := ctx.Value(workerControlKey{}).(*workerControl)
	if c == nil {
		return nil
	}
	stream, complete := opts.Deps.CallModel, opts.Deps.CallModelSync
	if opts.Provider != nil {
		if stream == nil {
			stream = opts.Provider.Stream
		}
		if complete == nil {
			complete = opts.Provider.Complete
		}
	}
	if stream != nil {
		opts.Deps.CallModel = func(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
			return func(yield func(llm.StreamEvent, error) bool) {
				if c.before != nil {
					req = c.before(req)
				}
				c.lastStop = ""
				for ev, err := range stream(ctx, req) {
					if ev.StopReason != "" {
						c.lastStop = ev.StopReason
					}
					if !yield(ev, err) {
						return
					}
				}
			}
		}
	}
	if complete != nil {
		opts.Deps.CallModelSync = func(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
			if c.before != nil {
				req = c.before(req)
			}
			msg, stop, usage, err := complete(ctx, req)
			c.lastStop = stop
			return msg, stop, usage, err
		}
	}
	return c
}

func captureWorkerSession(ctx context.Context, s *agentcore.Session, input string, emit func(db.Activity), control *workerControl, bounded bool) (harness.TerminalReason, error) {
	for {
		_, reason, err := captureRunSession(ctx, s, input, emit)
		// Errors may be retried by the engine. Leave its queue open until that
		// retry policy ends. Only normal completion closes admission here.
		if control == nil || control.finish == nil || reason != harness.ReasonCompleted || err != nil {
			return reason, err
		}
		canContinue := !bounded && ctx.Err() == nil && !IsRefusalStop(control.lastStop)
		pending, again := control.finish(canContinue)
		if !canContinue || !again || pending == "" {
			return reason, err
		}
		// Norma skips Stop on the text-only turn immediately after a blocking
		// Stop hook. Recover a correction arriving in that final gap. Reusing
		// Prompt resets SDK per-prompt budgets, so bounded runs never take this
		// path; their late, unapplied queue is reported by the engine instead.
		input = "[Planner correction received before completion]\n" + pending +
			"\nApply this to the current work before finishing, within the existing task scope and constraints."
	}
}
