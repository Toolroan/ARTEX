package agent

import (
	"context"
	"errors"
	"iter"
	"strings"
	"testing"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
)

func TestWorkerControlPreservesModelDepsBothPaths(t *testing.T) {
	for _, syncMode := range []bool{false, true} {
		t.Run(map[bool]string{false: "stream", true: "complete"}[syncMode], func(t *testing.T) {
			beforeCalls, modelCalls := 0, 0
			ctx := WithWorkerControl(context.Background(), func(req llm.CompletionRequest) llm.CompletionRequest {
				beforeCalls++
				req.Messages = append(req.Messages, llm.UserText("queued correction"))
				return req
			}, nil)
			check := func(req llm.CompletionRequest) {
				modelCalls++
				if !strings.Contains(req.Messages[len(req.Messages)-1].Text(), "queued correction") {
					t.Fatal("existing model callback did not receive correction")
				}
			}
			opts := agentcore.Options{Deps: harness.QueryDeps{
				NewID: func() string { return "preserved-id" },
				CallModel: func(_ context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
					check(req)
					return func(yield func(llm.StreamEvent, error) bool) {
						yield(llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: "end_turn"}, nil)
					}
				},
				CallModelSync: func(_ context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
					check(req)
					return llm.Message{}, "end_turn", llm.Usage{}, nil
				},
			}}
			control := attachWorkerControl(ctx, &opts)
			if syncMode {
				_, _, _, _ = opts.Deps.CallModelSync(ctx, llm.CompletionRequest{})
			} else {
				for range opts.Deps.CallModel(ctx, llm.CompletionRequest{}) {
				}
			}
			if beforeCalls != 1 || modelCalls != 1 || control.lastStop != "end_turn" || opts.Deps.NewID() != "preserved-id" {
				t.Fatal("control replaced dependencies or failed to wrap request")
			}
		})
	}
}

func TestWorkerControlCompletionRespectsBoundsAndRefusal(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		bounded               bool
		stop                  string
		modelErr              error
		wantCalls, wantFinish int
	}{
		{"late correction", false, "end_turn", nil, 2, 2},
		{"bounded run", true, "end_turn", nil, 1, 1},
		{"refusal", false, "refusal", nil, 1, 1},
		{"model error", false, "", errors.New("synthetic failure"), 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls, finishes := 0, 0
			ctx := WithWorkerControl(context.Background(), nil, func(canContinue bool) (string, bool) {
				finishes++
				if tc.bounded || tc.stop == "refusal" {
					if canContinue {
						t.Fatal("bounded or refused run allowed continuation")
					}
					return "late correction", false
				}
				return "late correction", finishes == 1
			})
			opts := agentcore.Options{NonStreaming: true, Deps: harness.QueryDeps{
				CallModelSync: func(_ context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
					calls++
					if calls > 2 {
						t.Fatal("unbounded completion retry")
					}
					if calls == 2 && !strings.Contains(req.Messages[len(req.Messages)-1].Text(), "late correction") {
						t.Fatal("late correction missing from continuation")
					}
					return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("finished")}}, tc.stop, llm.Usage{}, tc.modelErr
				},
			}}
			control := attachWorkerControl(ctx, &opts)
			session := agentcore.NewSession(opts)
			defer session.Close()
			_, _ = captureWorkerSession(ctx, session, "synthetic task", nil, control, tc.bounded)
			if calls != tc.wantCalls || finishes != tc.wantFinish {
				t.Fatalf("calls=%d finishes=%d, want %d %d", calls, finishes, tc.wantCalls, tc.wantFinish)
			}
		})
	}
}

func TestWorkerControlCancellationDoesNotContinue(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx = WithWorkerControl(ctx, nil, func(canContinue bool) (string, bool) {
		if canContinue {
			t.Fatal("cancelled worker was resumed")
		}
		return "correction", false
	})
	opts := agentcore.Options{NonStreaming: true, Deps: harness.QueryDeps{
		CallModelSync: func(_ context.Context, _ llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
			cancel()
			return llm.Message{}, "", llm.Usage{}, context.Canceled
		},
	}}
	control := attachWorkerControl(ctx, &opts)
	s := agentcore.NewSession(opts)
	defer s.Close()
	_, err := captureWorkerSession(ctx, s, "synthetic task", nil, control, false)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
}
