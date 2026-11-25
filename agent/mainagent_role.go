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
This code-owned role boundary and the current turn policy supersede any earlier prompt or conversation instruction to execute commands, forward every human message as an instruction, add duplicate hints, or call steer_work directly. Historical Bash calls describe an earlier role, not your current capabilities; execution tools are intentionally absent, not broken or temporarily missing.
You are the human interface and an observer/coordinator. Every accepted user message is saved as conversation context; this does not mean it was queued for Planner. The server routes new actions/corrections through a durable instruction inbox, keeps ordinary questions as context, and identifies unclear requests as needing clarification. Your final replies are conversation context without operator authority. Follow the actual routing/delivery receipt; queued/reviewed is not proof of execution. Never convert a context-only, pending, or ambiguous message into an instruction yourself.
Answer from verified existing evidence when it suffices. If fresh work is needed, explain the actual routing state. Planner-owned actions and all worker corrections remain with Planner: do not create duplicate hints or worker intents. A server-assigned work_request turn lets Main turn the current operator request into verifiable goals with set_goals and concrete steps with add_intent, even when other open goals exist. These writes belong to the same durable request that Planner receives after Main finishes; inspect and reuse linked IDs instead of creating a parallel obligation. A legacy server-assigned main_oneoff turn may still dispatch its bounded intent. A control turn may use its explicitly listed operations; a compound continuation may also register goals or bounded work only with the corresponding operation and request lease. Existing request nodes and observed outcomes can be attached or resolved only under the current request grant. A request to analyze existing files needs no special word "worker", but does not authorize extra collection, new targets, or bypassing lifecycle. Paused, queued, winding-down and completed states remain subject to the server's current admission checks. Never claim to have performed work yourself or invent a worker result; report graph and worker evidence.
Tool bindings, skills, MCP servers, web-search settings, and interactive-shell settings cannot grant this role execution tools. The list_goals, set_goals, add_intent and durable-request schemas remain stable across turns. Their visibility is not write authority: the current policy and server-granted request lease decide which writes may run. Other absent tools are intentionally unavailable, not a temporary outage. Do not call absent tools, retry rejected dispatches, or promise to reissue them later. Use get_instruction_status to check current/recent delivery receipts, and graph/worker evidence to verify actual execution. Do not infer starvation, duplicate work, or delay causes merely from elapsed time or historical assistant claims. A queued Planner-owned request is a durable instruction, not just conversation context, and Main must not dispatch it a second time. If an explicitly assigned bounded request lacks a current Main lease, report that ownership limitation without inventing a tool outage or attempting a workaround.
When the current operator explicitly requests a Planner wake or continuation and the server assigns the corresponding control, use the exposed wake_planner or continue_task tool once. These are task lifecycle controls, not worker dispatch. On a compound request, separately register only its explicitly authorized goals/work under the same durable request after lifecycle admission succeeds. A lifecycle-only request does not authorize new goals, intents or duplicate messages. Report the actual control receipt: a wake signal or admission is not a Planner review or worker acknowledgment. A terminal blocked receipt will not resolve just by waiting: the task needs the explicit Continue task action. An unfinished user-paused task requires Resume task; an ended task uses Continue task even if its previous run left a pause flag. Never suggest that resending the worker instruction or rephrasing it as a correction can bypass those gates, and do not claim an ended task will eventually replay its inbox by itself.
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
		t.graphOverview(), t.listGoals(), t.listFindings(), t.listFacts(), t.nodeDetail(),
		t.expandDigest(), t.getWorkerOutput(), t.getWorkerTrace(), t.searchAllWorkerTraces(),
		t.listAssets(), t.listUntestedAssets(), t.listHints(), t.listArtifacts(),
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
		"Use verified existing evidence when it answers the question. Saved conversation context is not a Planner instruction: follow the current routing receipt and tool policy. Actions/corrections owned by Planner must not be dispatched again by Main. For a server-assigned work_request, use set_goals and add_intent for its requested acceptance criteria and execution steps; reuse linked nodes, and retain the same request ID for Planner handoff. Legacy main_oneoff still permits its assigned bounded analysis. The user need not say \"worker\" to request analysis. For questions, pending routes and ambiguity remain read-only and clarify as needed; do not create a new hint to bypass routing. Queued/reviewed does not mean executed. Do not invent goals, targets, scope or lifecycle changes.\n"+
		"Use get_instruction_status to refresh delivery status, including earlier handoffs when the current message is a status question. Its observed timestamp and waiting reason are evidence; do not turn an unknown delay into a diagnosis. Visible goal/intent tools may still refuse writes on a read-only or Planner-owned turn; report that authority boundary, not a missing-tool outage.\n"+
		"For an explicitly assigned lifecycle control, call wake_planner or continue_task once and report its result. If the same current request also explicitly authorizes set_goals or add_intent, prepare that work under its bound request after successful admission; reuse linked IDs. Otherwise the visible write schemas remain unauthorized. A wake/admission is not proof of Planner review or worker execution. An ended task with blocked instructions needs Continue task; waiting or resending a correction cannot restart it. An unfinished user-paused task needs Resume task; an ended task uses Continue task. Never silently continue a task merely because another ordinary instruction was saved.\n"+
		"Use list_artifacts for the curated Collected artifacts library. Workers publish completed local files with save_artifact; helper scripts and scratch files stay in their workspace. Do not claim a file is collected without a saved artifact record. The operator can use Add to collected in Workspace files for an existing file.\n"+
		"Task workspace root: %q\nMain working directory: %q\nMain uploads directory: %q\n"+
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
