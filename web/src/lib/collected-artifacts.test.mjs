import { artifactPageRange, collectedArtifactDownloadHref, collectedArtifactQuery } from "./collected-artifacts.ts";
import assert from "node:assert/strict";
import test from "node:test";

test("collected downloads use the snapshot id and never a caller-provided path", () => {
  const id = "a".repeat(64);
  assert.equal(collectedArtifactDownloadHref("42", id), `/api/tasks/42/artifacts/${id}/download`);
  for (const task of ["", "../42", "42/other", "42?token=secret", "javascript:alert(1)"])
    assert.equal(collectedArtifactDownloadHref(task, id), undefined);
  for (const artifact of [
    "",
    "a".repeat(63),
    "A".repeat(64),
    "../download",
    "https://example.test/file",
    `${id}?token=secret`,
  ])
    assert.equal(collectedArtifactDownloadHref("42", artifact), undefined);
});

test("search and category values cannot inject an additional query parameter", () => {
  const params = new URLSearchParams(collectedArtifactQuery("database", " config &category=source? ", 25));
  assert.equal(params.get("category"), "database");
  assert.equal(params.get("q"), "config &category=source?");
  assert.equal(params.get("offset"), "25");
  assert.equal(params.get("limit"), "25");
  assert.equal(params.getAll("category").length, 1);
});

test("filter query restricts categories and bounds pagination", () => {
  const params = new URLSearchParams(collectedArtifactQuery("../other", "  ", -10, 100000));
  assert.equal(params.has("category"), false);
  assert.equal(params.has("q"), false);
  assert.equal(params.get("offset"), "0");
  assert.equal(params.get("limit"), "100");
  const invalid = new URLSearchParams(collectedArtifactQuery("all", "", NaN, Infinity));
  assert.equal(invalid.get("offset"), "0");
  assert.equal(invalid.get("limit"), "25");
});

test("pagination distinguishes no results and caps the partial final page", () => {
  assert.deepEqual(artifactPageRange(0, 0, 0), { from: 0, to: 0 });
  assert.deepEqual(artifactPageRange(25, 0, 25), { from: 0, to: 0 });
  assert.deepEqual(artifactPageRange(25, 3, 28), { from: 26, to: 28 });
  assert.deepEqual(artifactPageRange(25, 25, 28), { from: 26, to: 28 });
});
