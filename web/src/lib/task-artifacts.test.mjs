import { artifactBreadcrumbs, artifactDownloadHref, isArtifactPath, taskArtifactRoots } from "./task-artifacts.ts";
import assert from "node:assert/strict";
import test from "node:test";

test("artifact roots use the task id and keep saved output separate", () => {
  assert.deepEqual(taskArtifactRoots("42"), { files: "tasks/42", output: "spill/tasks/42" });
  assert.deepEqual(taskArtifactRoots("t-acme-web"), { files: "tasks/t-acme-web", output: "spill/tasks/t-acme-web" });
  for (const id of ["", "..", "../7", "42/other", "42?x=y", "some.id"]) assert.equal(taskArtifactRoots(id), null);
});

test("artifact navigation cannot leave the selected task folder", () => {
  const root = "tasks/42";
  for (const path of [root, `${root}/main`, `${root}/i7/export.sql`]) assert.equal(isArtifactPath(root, path), true);
  for (const path of [
    "tasks/420/file",
    "tasks/7/file",
    `${root}/../7/file`,
    `${root}/./file`,
    `${root}//file`,
    "/tasks/42/file",
    `${root}/a\\b`,
    `${root}/nul\0file`,
    "spill/tasks/42/file",
  ]) {
    assert.equal(isArtifactPath(root, path), false, path);
  }
  assert.deepEqual(artifactBreadcrumbs(root, `${root}/i7/export`), [
    { name: "i7", path: `${root}/i7` },
    { name: "export", path: `${root}/i7/export` },
  ]);
  assert.deepEqual(artifactBreadcrumbs(root, root), []);
  assert.deepEqual(artifactBreadcrumbs(root, "tasks/7"), []);
});

test("native download links encode untrusted file names and never include credentials", () => {
  const root = "tasks/42";
  const path = `${root}/main/dump & token=not-a-token #1.sql`;
  const href = artifactDownloadHref(root, path);
  const url = new URL(href, "https://artex.example");
  assert.equal(url.origin, "https://artex.example");
  assert.equal(url.pathname, "/api/workspace/download");
  assert.equal(url.searchParams.get("path"), path);
  assert.deepEqual([...url.searchParams.keys()], ["path"]);
  assert.equal(url.hash, "");
  assert.equal(artifactDownloadHref(root, root), undefined);
  assert.equal(artifactDownloadHref(root, "javascript:alert(1)"), undefined);
  assert.equal(artifactDownloadHref(root, "tasks/7/main/other.txt"), undefined);
});
