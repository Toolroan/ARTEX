import { recentRoutingReceipts, routingDeliveryLabel, routingRequestFor } from "./main-message-routing.ts";
import assert from "node:assert/strict";
import test from "node:test";

const payload = {
  taskId: "fixture-task",
  seg: 0,
  channel: "main",
  text: "Use the existing scope",
  mode: "auto",
  attachments: [{ name: "fixture.txt", path: "uploads/fixture.txt", size: 12 }],
};

test("transport retries reuse the request id only for an identical complete payload", () => {
  let generated = 0;
  const makeId = () => `fixture-${++generated}`;
  const first = routingRequestFor(null, payload, makeId);
  assert.equal(routingRequestFor(first, structuredClone(payload), makeId), first);
  assert.equal(generated, 1);
  for (const changed of [
    { taskId: "other-task" },
    { seg: 1 },
    { channel: "planner" },
    { text: "A changed correction" },
    { mode: "context" },
    { mode: "instruction" },
    { attachments: [] },
    { attachments: [{ ...payload.attachments[0], path: "uploads/other.txt" }] },
  ]) {
    assert.notEqual(routingRequestFor(first, { ...payload, ...changed }, makeId).requestId, first.requestId);
  }
  assert.notEqual(
    routingRequestFor(null, payload, makeId).requestId,
    first.requestId,
    "a later intentional send is new",
  );
});

test("routing statuses never imply a queued or reviewed correction was applied", () => {
  const receipt = { message_id: 1, route: "instruction" };
  assert.match(routingDeliveryLabel({ ...receipt, delivery: "context" }), /Planner not woken/);
  for (const delivery of ["queued", "waiting", "reviewing"]) {
    assert.match(routingDeliveryLabel({ ...receipt, delivery }), /not applied/);
  }
  assert.match(routingDeliveryLabel({ ...receipt, delivery: "presented" }), /not proof/);
  assert.match(routingDeliveryLabel({ ...receipt, delivery: "needs_review" }), /review existing work before resending/);
  assert.match(routingDeliveryLabel({ ...receipt, delivery: "routing_failed" }), /not sent to Planner/);
  assert.match(routingDeliveryLabel({ ...receipt, delivery: "unknown" }), /unconfirmed/);
});

test("recent receipts are bounded, newest first, and preserve the latest version of a message", () => {
  const old = { message_id: 4, route: "instruction", delivery: "queued" };
  const current = { ...old, delivery: "presented" };
  const receipts = recentRoutingReceipts([
    current,
    old,
    { ...old, message_id: 1 },
    { ...old, message_id: 3 },
    { ...old, message_id: 2 },
  ]);
  assert.deepEqual(
    receipts.map((r) => r.message_id),
    [4, 3, 2],
  );
  assert.equal(receipts[0].delivery, "presented");
});
