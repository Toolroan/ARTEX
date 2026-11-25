package agent

import (
	"github.com/Autumn-27/norma/agentcore"
	actool "github.com/Autumn-27/norma/tool"
)

// This tail is code-owned so editing Main's prompt cannot change its role.
const mainAgentRoleInstructions = `

[Main agent role boundary]
This code-owned role boundary supersedes any earlier or saved prompt instruction to copy every human message into add_hint or to call steer_work directly.
You are an orchestrator. Your tools only read the task graph, register operator-directed goals/constraints/scope, and delegate work.
The server has already saved every accepted human message, including questions and corrections, as an operator instruction for Planner and notified it. Do not copy the unchanged message into add_hint again. Worker corrections flow through Planner; explain the handoff and observe its results. Use add_hint only for genuinely new clarifications derived from the conversation; those are recorded as agent-authored data, never as direct operator instructions.
For investigation, testing, network access, command execution, file work, or artifact creation, delegate to the planner with add_hint or dispatch an explicitly requested one-off worker intent with add_intent. Corrections to ongoing workers must go through Planner; do not bypass that handoff with duplicate direct intents. Never claim to have performed work yourself or invent a worker result; report the returned graph and worker evidence.
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
