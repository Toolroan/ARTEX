import { fixtureWorkRequests, reviewFixtureWorkRequest, workRequestFixtures } from "./mock/work-request-fixtures.ts";
import { orderedWorkRequests, workRequestIsUnresolved, workRequestStateLabel } from "./work-requests.ts";
import assert from "node:assert/strict";
import test from "node:test";

test("only request outcomes resolve work; delivery and linked node completion do not", () => {
  for (const state of ["pending", "active", "review", "blocked", "unexpected"]) {
    assert.equal(workRequestIsUnresolved({ state, delivery: "presented", nodes: [{ state: "done" }] }), true);
  }
  for (const state of ["completed", "cancelled"]) assert.equal(workRequestIsUnresolved({ state }), false);
  assert.equal(workRequestStateLabel("presented"), "Unknown status", "chat delivery is not a work status");
  assert.equal(workRequestStateLabel("review"), "Awaiting review");
  assert.equal(workRequestStateLabel("blocked"), "Blocked");
});

test("unresolved requests remain visible ahead of newer completed work without mutating input", () => {
  const rows = [
    { id: 10, state: "completed" },
    { id: 3, state: "pending" },
    { id: 5, state: "blocked" },
    { id: 20, state: "cancelled" },
  ];
  assert.deepEqual(
    orderedWorkRequests(rows).map((row) => row.id),
    [5, 3, 20, 10],
  );
  assert.deepEqual(
    rows.map((row) => row.id),
    [10, 3, 5, 20],
  );
});

test("synthetic fixtures cover all outcomes and older tasks remain unobtrusive", () => {
  assert.deepEqual(
    new Set(workRequestFixtures.map((row) => row.state)),
    new Set(["pending", "active", "review", "blocked", "completed", "cancelled"]),
  );
  assert.deepEqual(fixtureWorkRequests("older-no-records"), []);
  assert.equal(fixtureWorkRequests("t-timeout-fixture")[0].state, "pending");
  const copy = fixtureWorkRequests("t-acme-web");
  copy[0].state = "completed";
  assert.equal(fixtureWorkRequests("t-acme-web")[0].state, "pending");
  assert.equal(
    workRequestFixtures.some((row) => "operator_text" in row),
    false,
  );
});

test("review reuses the saved request and linked node IDs, and cannot reopen resolved requests", () => {
  const requests = fixtureWorkRequests("t-acme-web");
  const originalIDs = requests.map((row) => row.id);
  const linkedNodes = structuredClone(requests.find((row) => row.id === 9999).nodes);
  reviewFixtureWorkRequest(requests, 9999);
  reviewFixtureWorkRequest(requests, 9999);
  const reviewed = requests.find((row) => row.id === 9999);
  assert.equal(reviewed.state, "review");
  assert.equal(reviewed.review_requested, true);
  assert.equal(workRequestIsUnresolved(reviewed), true);
  assert.deepEqual(
    requests.map((row) => row.id),
    originalIDs,
  );
  assert.deepEqual(reviewed.nodes, linkedNodes);
  for (const id of [9996, 9995, 0]) assert.throws(() => reviewFixtureWorkRequest(requests, id));
});
