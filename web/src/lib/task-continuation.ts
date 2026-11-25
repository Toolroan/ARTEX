import type { PlannerDelivery } from "./api";
import type { TaskStatus } from "./types";

export function isTerminalTask(status?: string): boolean {
  return status === "done" || status === "failed" || status === "timeout";
}

export function terminalTaskNotice(status?: TaskStatus): string | null {
  switch (status) {
    case "timeout":
      return "Task timed out or reached its run limit. Planner is stopped; saved instructions cannot run until you continue the task.";
    case "done":
      return "Task completed. Planner is stopped; saved instructions cannot run until you continue the task.";
    case "failed":
      return "Task failed. Planner is stopped; saved instructions cannot run until you continue the task.";
    default:
      return null;
  }
}

// A wake only nudges an admitted task with undelivered instructions. It must
// never be presented as an alternative to continuing a terminal or paused task.
export function canWakePlanner(
  status: TaskStatus | undefined,
  paused: boolean,
  queued: boolean,
  receipts: readonly PlannerDelivery[],
): boolean {
  return (
    status === "running" &&
    !paused &&
    !queued &&
    receipts.some(
      (receipt) =>
        (receipt.route === "action" || receipt.route === "correction" || receipt.route === "control") &&
        (receipt.delivery === "queued" || receipt.delivery === "waiting"),
    )
  );
}
