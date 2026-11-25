import type { PlannerDelivery, TaskControlAction, TaskControlResult } from "../api";
import type { Task } from "../types";

const terminal = (status: string) => ["done", "failed", "timeout"].includes(status);

export function applyMockTaskControl(task: Task, action: TaskControlAction, restartQueued = false): TaskControlResult {
  const isTerminal = terminal(task.status);
  const paused = task.status === "paused" || task.paused === true;
  const queued = task.status === "queued" || task.queued === true;
  let alreadyActive = false;
  if (action === "wake") {
    if (isTerminal || paused || queued || task.status !== "running")
      throw new Error("Only an admitted running task can wake Planner");
  } else if (action === "restart") {
    if (paused) throw new Error("Use Resume for a paused task");
    if (!isTerminal && task.status !== "running" && !queued) throw new Error("Only a terminal task can be continued");
    alreadyActive = !isTerminal;
    if (isTerminal) {
      task.status = restartQueued ? "queued" : "running";
      task.paused = false;
      task.queued = restartQueued;
      task.engine_mode = restartQueued ? "idle" : "exploring";
      task.completed_at = undefined;
      task.completed_unix = undefined;
    }
  } else if (action === "pause") {
    if (isTerminal) throw new Error("Cannot pause a terminal task");
    if (paused) throw new Error("Task is already paused");
    task.paused = true;
    task.queued = false;
    task.status = "paused";
    task.engine_mode = "paused";
  } else if (action === "resume") {
    if (isTerminal || !paused) throw new Error("Only a paused task can be resumed");
    task.paused = false;
    task.queued = false;
    task.status = "running";
    task.engine_mode = "exploring";
  } else {
    throw new Error("Unknown task control action");
  }
  return {
    id: task.id,
    paused: Boolean(task.paused),
    queued: Boolean(task.queued),
    status: task.status,
    action,
    ...(action === "wake" ? { wake_sent: true } : {}),
    ...(alreadyActive ? { already_active: true } : {}),
  };
}

export function mockReceiptForTask(receipt: PlannerDelivery, task?: Task): PlannerDelivery {
  if (!task || !["queued", "waiting", "blocked"].includes(receipt.delivery)) return receipt;
  const { task_status: _oldStatus, required_action: _oldAction, ...base } = receipt;
  if (terminal(task.status))
    return { ...base, delivery: "blocked", task_status: task.status, required_action: "restart" };
  if (task.paused || task.status === "paused")
    return { ...base, delivery: "waiting", task_status: "paused", required_action: "resume" };
  return {
    ...base,
    delivery: task.queued || task.status === "queued" ? "waiting" : "queued",
    task_status: task.status,
  };
}
