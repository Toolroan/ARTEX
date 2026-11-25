import type { MainRoutingMode, PlannerDelivery } from "./api";
import type { ChatAttachment } from "./types";

export interface RoutingRequestPayload {
  taskId: string;
  seg: number;
  channel: "main" | "planner";
  text: string;
  mode: MainRoutingMode;
  attachments?: readonly ChatAttachment[];
}

export interface RoutingRequest {
  requestId: string;
  fingerprint: string;
}

// Hold this record after a transport failure. Reusing the id only for the same
// complete payload lets the server return its existing receipt without replay.
export function routingRequestFor(
  previous: RoutingRequest | null,
  payload: RoutingRequestPayload,
  makeId: () => string = () => globalThis.crypto.randomUUID(),
): RoutingRequest {
  const fingerprint = JSON.stringify({
    task: payload.taskId,
    seg: payload.seg,
    channel: payload.channel,
    text: payload.text,
    mode: payload.mode,
    attachments: (payload.attachments ?? []).map(({ name, path, size, abs }) => ({ name, path, size, abs })),
  });
  return previous?.fingerprint === fingerprint ? previous : { requestId: makeId(), fingerprint };
}

// A delivery receipt describes admission/review, never successful application by
// a Worker. Keep these labels explicit even when the backend reports no reason.
export function routingDeliveryLabel(receipt: PlannerDelivery): string {
  switch (receipt.delivery) {
    case "routing":
      return "Routing message…";
    case "routing_failed":
      return "Routing failed — not sent to Planner";
    case "context":
      return "Discussion context — Planner not woken";
    case "clarify":
      return "Clarification needed — not sent to Planner";
    case "queued":
      return "Queued for Planner — not applied";
    case "waiting":
      return "Waiting for Planner — not applied";
    case "blocked":
      if (receipt.task_status === "timeout") return "Task timed out or reached its run limit — instruction blocked";
      if (receipt.task_status === "done") return "Task completed — instruction blocked";
      if (receipt.task_status === "failed") return "Task failed — instruction blocked";
      return "Instruction blocked — task action required";
    case "reviewing":
      return "Planner reviewing — not applied";
    case "presented":
      return "Request reviewed — not proof a Worker applied it";
    case "needs_review":
      return "Delivery uncertain — review existing work before resending";
    case "main":
      return "Sent to Main — no Planner delivery confirmed";
    default:
      return "Saved — delivery unconfirmed";
  }
}

export function recentRoutingReceipts(receipts: readonly PlannerDelivery[]): PlannerDelivery[] {
  const unique = new Map<number, PlannerDelivery>();
  for (const receipt of receipts) {
    const id = receipt.message_id || receipt.hint_id;
    if (id && !unique.has(id)) unique.set(id, receipt);
  }
  return [...unique.values()]
    .sort((a, b) => (b.message_id || b.hint_id || 0) - (a.message_id || a.hint_id || 0))
    .slice(0, 3);
}
