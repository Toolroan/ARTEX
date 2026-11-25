import type { WorkRequest } from "../work-requests";

const timestamp = "2026-11-25T08:00:00Z";
const request = (id: number, state: WorkRequest["state"], extra: Partial<WorkRequest> = {}): WorkRequest => ({
  id,
  source_message_id: id,
  route: "action",
  state,
  revision: 1,
  acceptance: "",
  resolution: "",
  review_requested: false,
  created_at: timestamp,
  updated_at: timestamp,
  nodes: [],
  ...extra,
});

// All examples are synthetic. Delivered chat is intentionally paired with
// unresolved work, so the fixture cannot imply that delivery means completion.
export const workRequestFixtures: WorkRequest[] = [
  request(10002, "pending", { acceptance: "Review the saved fixture before starting new work" }),
  request(9999, "blocked", {
    acceptance: "Compare the two synthetic result files",
    resolution: "The second fixture file has not been provided.",
    nodes: [
      {
        id: 1,
        node_id: 42,
        kind: "intent",
        summary: "Compare synthetic result files",
        state: "blocked",
        request_revision: 1,
      },
    ],
  }),
  request(9998, "review", {
    acceptance: "Confirm the synthetic output matches the requested format",
    review_requested: true,
    nodes: [
      {
        id: 2,
        node_id: 43,
        kind: "goal",
        summary: "Confirm synthetic output format",
        state: "open",
        request_revision: 1,
      },
    ],
  }),
  request(9997, "active", {
    acceptance: "Organize the synthetic report",
    nodes: [
      {
        id: 3,
        node_id: 44,
        kind: "intent",
        summary: "Organize report sections",
        state: "running",
        request_revision: 1,
      },
    ],
  }),
  request(9996, "completed", {
    acceptance: "Create a synthetic checklist",
    resolution: "Checklist completed and checked against the fixture.",
    nodes: [
      { id: 4, node_id: 45, kind: "intent", summary: "Create synthetic checklist", state: "done", request_revision: 1 },
    ],
  }),
  request(9995, "cancelled", {
    acceptance: "An optional synthetic follow-up",
    resolution: "Cancelled by the operator.",
  }),
];

export function fixtureWorkRequests(taskId: string): WorkRequest[] {
  if (taskId === "t-acme-web") return structuredClone(workRequestFixtures);
  if (taskId === "t-timeout-fixture")
    return [request(10003, "pending", { acceptance: "Review saved synthetic instructions after continuation" })];
  if (taskId === "t-failed-fixture")
    return [request(10004, "pending", { acceptance: "Review saved synthetic instructions after continuation" })];
  return [];
}

export function reviewFixtureWorkRequest(requests: WorkRequest[], id: number): void {
  const request = requests.find((item) => item.id === id);
  if (!request) throw new Error("Work request not found");
  if (request.state === "completed" || request.state === "cancelled")
    throw new Error("This work request is already resolved");
  request.review_requested = true;
  request.state = "review";
  request.updated_at = new Date().toISOString();
}
