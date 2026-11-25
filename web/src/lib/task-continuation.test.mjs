import { routingDeliveryLabel } from "./main-message-routing.ts";
import { applyMockTaskControl, mockReceiptForTask } from "./mock/task-continuation.ts";
import { canWakePlanner, isTerminalTask, terminalTaskNotice } from "./task-continuation.ts";
import assert from "node:assert/strict";
import test from "node:test";

const fixture = (status) => ({ id: "synthetic", status, paused: status === "paused", queued: status === "queued" });
const instruction = { message_id: 7, route: "correction", delivery: "queued", text: "Synthetic correction" };

test("each terminal state explicitly blocks instructions until continuation", () => {
  const labels = new Set();
  for (const status of ["timeout", "done", "failed"]) {
    assert.equal(isTerminalTask(status), true);
    assert.match(terminalTaskNotice(status), /cannot run until you continue/);
    const receipt = mockReceiptForTask(instruction, fixture(status));
    assert.equal(receipt.delivery, "blocked");
    assert.equal(receipt.required_action, "restart");
    labels.add(routingDeliveryLabel(receipt));
    assert.match(routingDeliveryLabel(receipt), /instruction blocked/);
  }
  assert.equal(labels.size, 3);
  assert.match(routingDeliveryLabel({ ...instruction, delivery: "blocked" }), /task action required/);
  for (const status of ["running", "created", "queued", "paused", undefined]) {
    assert.equal(isTerminalTask(status), false);
    assert.equal(terminalTaskNotice(status), null);
  }
});

test("continue preserves instruction identity and honors queue admission; retries are idempotent", () => {
  const task = fixture("timeout");
  const saved = mockReceiptForTask(instruction, task);
  const result = applyMockTaskControl(task, "restart", true);
  assert.equal(result.status, "queued");
  assert.equal(result.queued, true);
  const after = mockReceiptForTask(saved, task);
  assert.equal(after.message_id, instruction.message_id);
  assert.equal(after.text, instruction.text);
  assert.equal(after.delivery, "waiting");
  assert.equal(after.required_action, undefined);
  assert.equal(saved.delivery, "blocked", "old receipts are immutable");
  assert.equal(applyMockTaskControl(task, "restart").already_active, true);
  assert.equal(task.status, "queued", "retry cannot bypass admission");
  const running = fixture("done");
  applyMockTaskControl(running, "restart");
  assert.equal(mockReceiptForTask(instruction, running).delivery, "queued");
  assert.equal(applyMockTaskControl(running, "restart").already_active, true);
});

test("wake is only offered for pending instructions in an admitted running task", () => {
  assert.equal(canWakePlanner("running", false, false, [instruction]), true);
  for (const status of ["done", "failed", "timeout", "paused", "queued", "created", undefined]) {
    assert.equal(canWakePlanner(status, false, false, [instruction]), false);
  }
  assert.equal(canWakePlanner("running", true, false, [instruction]), false);
  assert.equal(canWakePlanner("running", false, true, [instruction]), false);
  for (const delivery of [
    "context",
    "clarify",
    "reviewing",
    "presented",
    "needs_review",
    "routing_failed",
    "blocked",
  ]) {
    assert.equal(canWakePlanner("running", false, false, [{ ...instruction, delivery }]), false);
  }
  for (const status of ["timeout", "paused", "queued"])
    assert.throws(() => applyMockTaskControl(fixture(status), "wake"));
  assert.equal(applyMockTaskControl(fixture("running"), "wake").wake_sent, true);
});

test("paused Resume stays distinct from terminal continuation and final receipts stay final", () => {
  for (const status of ["timeout", "done", "failed", "running"])
    assert.throws(() => applyMockTaskControl(fixture(status), "resume"));
  assert.throws(() => applyMockTaskControl(fixture("paused"), "restart"));
  assert.equal(applyMockTaskControl(fixture("paused"), "resume").status, "running");
  for (const delivery of ["presented", "needs_review", "context", "clarify", "reviewing"]) {
    const receipt = { ...instruction, delivery };
    assert.equal(mockReceiptForTask(receipt, fixture("timeout")), receipt);
  }
});
