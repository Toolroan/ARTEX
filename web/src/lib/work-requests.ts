// Request outcome is independent of message delivery and of linked node state.
export type WorkRequestState = "pending" | "active" | "review" | "blocked" | "completed" | "cancelled";

export interface WorkRequestNode {
  id: number;
  node_id?: number;
  kind: string;
  summary: string;
  state: string;
  request_revision: number;
}

export interface WorkRequest {
  id: number;
  source_message_id: number;
  route: string;
  state: WorkRequestState;
  revision: number;
  acceptance: string;
  resolution: string;
  review_requested: boolean;
  created_at: string;
  updated_at: string;
  nodes: WorkRequestNode[];
}

export function workRequestIsUnresolved(request: Pick<WorkRequest, "state">): boolean {
  return request.state !== "completed" && request.state !== "cancelled";
}

export function orderedWorkRequests(requests: readonly WorkRequest[]): WorkRequest[] {
  return [...requests].sort(
    (a, b) => Number(workRequestIsUnresolved(b)) - Number(workRequestIsUnresolved(a)) || b.id - a.id,
  );
}

export function workRequestStateLabel(state: string): string {
  switch (state) {
    case "pending":
      return "Pending";
    case "active":
      return "In progress";
    case "review":
      return "Awaiting review";
    case "blocked":
      return "Blocked";
    case "completed":
      return "Completed";
    case "cancelled":
      return "Cancelled";
    default:
      return "Unknown status";
  }
}

export function workRequestNodeLabel(kind: string): string {
  switch (kind) {
    case "goal":
      return "Goal";
    case "intent":
      return "Worker task";
    case "fact":
      return "Fact";
    case "finding":
      return "Finding";
    default:
      return "Node";
  }
}
