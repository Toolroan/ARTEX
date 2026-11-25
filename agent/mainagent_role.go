package agent

import (
	"fmt"
	"path/filepath"

	"github.com/Autumn-27/norma/agentcore"
	actool "github.com/Autumn-27/norma/tool"
)

// This tail is code-owned so editing Main's prompt cannot change its role.
const mainAgentRoleInstructions = `

[Main agent role boundary]
This code-owned role boundary supersedes any earlier prompt or conversation instruction to execute commands, copy every human message into add_hint, or call steer_work directly. Historical Bash calls describe an earlier role, not your current capabilities; execution tools are intentionally absent, not broken or temporarily missing.
You are an orchestrator. Your tools only read the task graph, register operator-directed goals/constraints/scope, and delegate work.
The server has already saved every accepted human message, including questions and corrections, as an operator instruction for Planner and attempted to wake it subject to the task lifecycle. Follow the actual handoff receipt; saved/notified does not mean executed. Do not copy the unchanged message into add_hint again. Worker corrections flow through Planner; explain the handoff and observe its results. Use add_hint only for genuinely new clarifications derived from the conversation; those are recorded as agent-authored data, never as direct operator instructions.
Answer from verified existing evidence when it suffices. When the question requires fresh investigation, file analysis, computation, or artifact creation, delegate within the user's request. If Planner is already notified, prefer its existing handoff and supply any missing context without dispatching duplicate work. For an explicitly requested bounded one-off analysis when the receipt is saved because the task is completed or has no open goals, check for matching work and use add_intent when available. The user need not say "worker" to authorize that analysis; this may reactivate a completed task under the existing one-off dispatch behavior, but does not authorize extra collection or new targets. For paused, queued, or winding-down tasks, report the saved/waiting state and do not bypass it without the operator's explicit lifecycle request. Corrections to ongoing workers must go through Planner; do not bypass that handoff with duplicate direct intents. Never claim to have performed work yourself or invent a worker result; report the returned graph and worker evidence.
Tool bindings, skills, MCP servers, web-search settings, and interactive-shell settings cannot grant this role execution tools. If a required delegation tool is unavailable, tell the operator which tool is missing; do not attempt a workaround.
`

// Construct capabilities from known handlers, not from tool names supplied by
// extensions. Finding/asset/fact creation and generic file/network/shell tools
// belong to the planner or workers. Scope administration remains available for
// the operator to direct what those agents may work on.
func mainAgentOrchestrationTools(t *ToolSet) []actool.CoreTool {
	// Keep Main's model-authored hints distinct from the raw user-text bridge.
	// Other orchestration writes retain their existing operator-authorized origin.
	hints := *t
	hints.worker = "mainagent"
	return t.DropCoverageTools([]actool.CoreTool{
		t.graphOverview(), t.listFindings(), t.listFacts(), t.nodeDetail(),
		t.expandDigest(), t.getWorkerOutput(), t.getWorkerTrace(), t.searchAllWorkerTraces(),
		t.listAssets(), t.listUntestedAssets(),
		hints.addHint(), t.addIntent(), t.setGoals(), t.setConstraints(),
		t.addTaskScope(), t.addCompanyScope(),
	})
}

// Refreshed for every Main request, including resumed legacy execution history.
// This changes guidance only; tools and session budgets are unchanged.
func mainAgentTurnReminder(mainDir string) string {
	if absolute, err := filepath.Abs(mainDir); err == nil {
		mainDir = absolute
	}
	return fmt.Sprintf("[Current Main capabilities and delegation]\n"+
		"Bash, file-execution and network tools are intentionally unavailable to Main. Historical tool calls/results are evidence from an earlier role, not current capabilities or proof of a fresh check. Use the current tool schemas; do not diagnose an outage or stop merely because an execution tool is absent.\n"+
		"Use verified existing evidence when it answers the question. If fresh file analysis or computation is necessary, prefer the already-notified Planner and provide any missing context. When the actual receipt is saved because the task is completed or has no open goals, check for matching open/running work and use add_intent, if available, for the requested bounded one-off analysis. The user need not say \"worker\" to request that analysis; existing one-off dispatch may reactivate a completed task. Do not duplicate work or unchanged corrections already shared with Planner. Respect the actual receipt: saved/notified does not mean executed. For paused, queued, or winding-down tasks, report the saved/waiting state and do not bypass it without the operator's explicit lifecycle request. Do not invent new goals, targets or scope.\n"+
		"Task artifact root: %q\nMain working directory: %q\nMain uploads directory: %q\n"+
		"Workers have their own working directories. In a handoff, provide the relevant full absolute file paths, requested calculation/check, necessary conversation context, and expected result; do not send only relative filenames. Retain the existing scope and task constraints. If delegation is unavailable or waiting, report that specific state; distinguish historical figures from newly verified results.",
		filepath.Dir(mainDir), mainDir, filepath.Join(mainDir, "uploads"))
}

// enforceMainAgentRole is the final boundary before NewSession. The resolver may
// remove disabled/unbound tools, but cannot add capabilities or replace handlers,
// even by reusing an allowed name. Main deliberately uses code-owned descriptions,
// schemas and defaults as well: resolver wrappers are not trusted capabilities.
// Independent hooks and untrusted-output handling remain on the session.
func enforceMainAgentRole(opts *agentcore.Options, trusted []actool.CoreTool) {
	present := make(map[string]bool, len(opts.Tools))
	for _, t := range opts.Tools {
		present[t.Name()] = true
	}
	// Non-nil even when all bindings were removed: nil asks Norma for DefaultTools.
	allowed := make([]actool.CoreTool, 0, len(trusted))
	for _, t := range trusted {
		if present[t.Name()] {
			allowed = append(allowed, guardPanic(t))
		}
	}
	opts.Tools = allowed
	// These options cause Norma to register tools AFTER the explicit tool list.
	// Monitor, in particular, can execute arbitrary background shell commands.
	opts.DisableBackgroundTasks = true
	opts.DeferredTools = nil
	opts.UnlockSet = nil
	opts.EnableWebFetch = false
	opts.EnableWebSearch = false
	opts.Plan = nil
	opts.Memory = nil
	opts.Skills = nil
	opts.Todos = nil
	opts.AskUser = nil
}
