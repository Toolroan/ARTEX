package agent

import (
	"context"
	"fmt"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	"github.com/Autumn-27/norma/transcript"
)

// MainAgent is the thin human-interface orchestrator (docs §4.2 / §7). The human
// chats with it; it observes (read tools), and steers by injecting hints
// (→planner) or direct high-priority intents (→frontier). It does NOT run the
// autonomous intent-generation loop (that is the planner's job).
type MainAgent struct {
	findingRecorder FindingRecorder
	prov            llm.Provider
	model           string
	tx              *transcript.Store                      // raw LLM conversation persistence (nil = off)
	window          int                                    // context window in tokens (for compaction)
	windowFn        func() int                             // optional dynamic task-chain minimum
	maxTurns        int                                    // max agent turns per run (0 = unlimited)
	proxyAddr       string                                 // recording proxy for WebFetch (empty = direct)
	proxyCACert     string                                 // recording proxy's CA cert path (HTTPS verify)
	webSearch       WebSearchOpts                          // web_search tool backend selection (off by default)
	workDir         string                                 // shared work dir (surfaced in prompt as artifact-output target)
	steerWork       func(intentID int64, msg string) error // engine callback: steer a running work (nil = off)
	nonStreamingFn  func() bool                            // resolver: use non-streaming (Complete) path? (nil = streaming)
	noaEnabledFn    func() bool                            // resolver: use experimental noa compaction? (nil = off)
	maxTokensFn     func() int                             // resolver: per-reply output cap (nil/0 = send no cap)
}

// SetNoaEnabled retains the shared agent configuration API. Main always uses
// built-in compaction because NOA adds capabilities outside its role boundary.
func (m *MainAgent) SetNoaEnabled(fn func() bool) { m.noaEnabledFn = fn }

// SetNonStreaming wires a resolver deciding whether runs use the non-streaming
// model path (true = non-streaming). nil/unset = streaming (default).
func (m *MainAgent) SetNonStreaming(fn func() bool) { m.nonStreamingFn = fn }

func (m *MainAgent) nonStreaming() bool { return m.nonStreamingFn != nil && m.nonStreamingFn() }

// SetMaxTokens wires a resolver for the per-reply output cap. nil/unset or 0 =
// send no cap and let the endpoint decide. Read per run, like nonStreaming.
func (m *MainAgent) SetMaxTokens(fn func() int) { m.maxTokensFn = fn }

func (m *MainAgent) maxTokens() int {
	if m.maxTokensFn == nil {
		return 0
	}
	return m.maxTokensFn()
}

func NewMainAgent(prov llm.Provider, model, workDir string, tx *transcript.Store, window, maxTurns int) *MainAgent {
	return &MainAgent{prov: prov, model: model, workDir: workDir, tx: tx, window: window, maxTurns: maxTurns}
}

func (m *MainAgent) SetCompactionWindowResolver(fn func() int) { m.windowFn = fn }

func (m *MainAgent) compactionWindow() int {
	if m.windowFn != nil {
		return m.windowFn()
	}
	return m.window
}

// SetProxy retains the shared agent configuration API. Main delegates network
// operations and never exposes a proxy-backed execution or WebFetch tool.
func (m *MainAgent) SetProxy(addr, caCert string) { m.proxyAddr, m.proxyCACert = addr, caCert }

// SetWebSearch retains the shared configuration API; Main delegates web searches.
func (m *MainAgent) SetWebSearch(o WebSearchOpts) { m.webSearch = o }

// SetSteerWork wires the engine callback that lets the main agent's steer_work
// tool inject a mid-run course-correction into a running work (nil = tool off).
func (m *MainAgent) SetSteerWork(fn func(intentID int64, msg string) error) { m.steerWork = fn }

// mainAgentDefaultTmpl is the built-in EDITABLE body (section [A]) of the main agent
// prompt, seeded into agent_prompts. Goal is a {{.Goal}} template var; the role
// boundary and untrusted-content tails remain code-owned after rendering.
const mainAgentDefaultTmpl = `You are the "main agent" of an authorized penetration-testing system, the interface to the human operator. You do not explore in person, nor do you autonomously generate intents in a loop (that is the planner's job). Your duties:

1. Observe: use graph_overview / list_findings / list_facts / list_assets / get_worker_output to answer the human's questions about current progress.
2. Steer (land the human's intent into the system):
   - Every accepted human message is already saved and forwarded to Planner by the server. For "change direction / emphasize a vulnerability class / focus an area", explain that handoff without adding the same hint again. Use add_hint only for a new clarification that has not already been forwarded.
   - The human wants to "test a specific target right now" → inject one high-priority intent directly with add_intent (priority 8-10). The system automatically pulls a completed task back to running so a worker claims and executes this intent, returning to completed when done.
     **When all task goals are met** (goals in graph_overview are all met): before dispatching, judge whether this intent implies "a new result to achieve". If it does, restate your guessed goal to the human in one sentence and **ask whether to register it as a formal goal** — if yes → register it with set_goals (the task then enters normal planning and the planner advances autonomously); if no / just a quick probe → only add_intent this one, and the task returns to completed once the worker finishes (it will not continue autonomously). If the intent is clearly a one-off check with no new goal implied, just add_intent directly — no need to ask every time.
   - The human wants to "course-correct a running intent (work) in realtime (stop doing X, focus on Y)" → Planner receives that message automatically and coordinates the affected workers. Main has no steer_work capability. Observe progress with get_worker_output and report evidence; do not bypass Planner by dispatching a duplicate intent.
   - The human wants to "add a new final goal to achieve" → add the goal with set_goals. The system writes the goal into the task graph and **automatically pulls the completed/paused task back to running** (the planner will re-judge completion accordingly), no manual resume needed.
   - The human wants to "add/change test constraints (allow/forbid certain operations, e.g. 'only test the current port', 'no brute forcing', 'passive recon only')" → register them with set_constraints (type=allow / type=deny). Constraints are injected into the planner/worker prompts at the next planning round to bound exploration; they can also be managed in the overview's "Constraint management".
3. Reply concisely in plain language, stating what you did.

Current task goal: {{.Goal}}

Do not fabricate findings; answer only from real data returned by tools.`

func mainAgentSystem(goal, dataDir, workDir string) string {
	body := renderSystem("mainagent", mainAgentDefaultTmpl, MainVars{Goal: goal, DataDir: dataDir, Now: nowStr()})
	return body + mainAgentRoleInstructions + untrustedContentTail(body) + noiseProfileBlock()
}

// Chat handles one human message and returns the assistant reply. emit, if
// non-nil, receives each execution step (thinking / tool_use / tool_result /
// text / result) so the main-agent session shows its work — exactly like the
// worker/planner sessions — not just the final answer. hooks (the task's
// target-only Guard) sees every tool call; may be nil.
func (m *MainAgent) Chat(ctx context.Context, taskID int64, mainSeg int, as *db.AssetStore, ts *db.ExplorationStore, goal, message string, hooks harness.HookRunner, emit func(db.Activity), notify, resume func(), notifyGoal func([]string), notifyHint func(string, []string)) (string, error) {
	tsx := NewToolSet(ts, "human")
	tsx.SetFindingRecorder(m.findingRecorder)
	if as != nil {
		tsx.SetAssetStore(as, as.Companies())
	}
	tsx.SetTaskID(taskID)
	tsx.SetCoverageEnabled(coverageOn(as, taskID))
	tsx.SetNotify(notify)         // generic wake-up (write ops without a dedicated callback go through it, debounced)
	tsx.SetResumeTask(resume)     // set_goals adds a goal → pull a completed/paused task back to running
	tsx.SetNotifyGoal(notifyGoal) // set_goals adds a goal → record a "human added a goal: …" trigger for the planner
	tsx.SetNotifyHint(notifyHint) // model-authored add_hint uses origin "mainagent"; raw operator messages are forwarded by the server
	tsx.steerWork = m.steerWork   // enable steer_work tool (nil = unavailable)
	ctx = WithRunInfo(ctx, RunInfo{TaskID: taskID, ExplorationID: explorationID(ts)})
	// Main's role is enforced by trusted handlers, not editable bindings or prompt
	// guidance. Do not start skill/MCP clients for this orchestration-only session.
	trusted := mainAgentOrchestrationTools(tsx)
	tools := append(trusted[:0:0], trusted...)
	if resolve := ToolResolve(); resolve != nil {
		tools = resolve(ctx, "mainagent", tools)
	}
	// the main run directory <workDir>/tasks/<taskID>/main, created up front.
	mainDir := ensureRunDir(m.workDir, taskID, 0)
	ctx = intercept.WithReviewWorkingDirectory(ctx, mainDir)
	opts := agentcore.Options{
		Provider:       m.prov,
		SystemPrompt:   []string{mainAgentSystem(goal, m.workDir, mainDir)},
		Tools:          tools,
		PermissionMode: permission.ModeBypass,
		WorkingDir:     mainDir,
		ToolOutputDir:  TaskToolOutputDir(m.workDir, taskID, mainDir),
		MaxTurns:       m.maxTurns,                             // 0 = unlimited (configurable in agent management)
		Compaction:     compactionConfig(m.compactionWindow()), // long chats stay within the window
		// hitting the budget (steps) → the SDK runs wrap-up: output a one-line progress summary to the user. Prompt and wrap-up rounds are editable in the backend (default 10 rounds).
		Settlement:   wrapupSettlement("mainagent", nil),
		NonStreaming: m.nonStreaming(), // when this profile selects non-streaming, use Provider.Complete
		MaxTokens:    m.maxTokens(),    // 0 = send no cap, the server's default decides

		// On by default; ARTEX_UNTRUSTED_BOUNDARY=0 opts out: nonce-frame tool output as untrusted data.
		UntrustedOutputBoundary: untrustedBoundaryEnabled(),
	}
	if hooks != nil { // typed-nil guard: only set when concrete (avoids harness panic)
		opts.Hooks = hooks
	}
	opts.Hooks = SpillHooks(opts.Hooks, opts.ToolOutputDir)
	// Segment 0 keeps the legacy "exp%d-main" name so existing transcripts still
	// load; each new session (seg>=1) gets its own file for a clean context. The id
	// is the provider-side identity (session header, usage metering) with or without
	// persistence; Norma only puts it on ctx when it writes a transcript.
	opts.SessionID = fmt.Sprintf("exp%d-main", ts.ID())
	if mainSeg > 0 {
		opts.SessionID = fmt.Sprintf("exp%d-main-s%d", ts.ID(), mainSeg)
	}
	ctx = transcript.WithSessionID(ctx, opts.SessionID)
	if m.tx != nil { // persist raw human↔AI conversation; one accumulating file per segment
		opts.Transcript = m.tx
	}
	// Use standard compaction: NOA adds a file-writing Compress tool. Apply the
	// boundary last so neither resolver additions nor SDK auto-tools escape it.
	ctx = attachSideCapture(ctx, &opts)
	enforceMainAgentRole(&opts, trusted)
	s := agentcore.NewSession(opts)
	defer s.Close()
	// reload the prior conversation from the transcript so the agent has context
	// across turns (each Chat is a fresh session; without this it can't see earlier
	// messages). First turn: no file yet → Resume loads nothing and proceeds.
	// A corrupt or unreadable transcript stops the turn before any model or tool call.
	if m.tx != nil {
		if err := resumeSession(s, opts.SessionID); err != nil {
			return "", err
		}
	}
	text, _, err := captureRunSession(ctx, s, message, func(r db.Activity) {
		if emit != nil {
			r.Worker = "mainagent"
			emit(r)
		}
	})
	return text, err
}
