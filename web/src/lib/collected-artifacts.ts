import type { ArtifactCategory } from "./types";

export const ARTIFACT_CATEGORIES: readonly ArtifactCategory[] = [
  "database",
  "source",
  "config",
  "document",
  "evidence",
  "other",
];
export const COLLECTED_ARTIFACT_PAGE_SIZE = 25;

export function collectedArtifactQuery(
  category: string,
  search: string,
  offset: number,
  limit = COLLECTED_ARTIFACT_PAGE_SIZE,
): string {
  const query = new URLSearchParams();
  if (ARTIFACT_CATEGORIES.includes(category as ArtifactCategory)) query.set("category", category);
  if (search.trim()) query.set("q", search.trim());
  query.set(
    "limit",
    String(Math.min(100, Math.max(1, Number.isFinite(limit) ? Math.floor(limit) : COLLECTED_ARTIFACT_PAGE_SIZE))),
  );
  query.set("offset", String(Math.max(0, Number.isFinite(offset) ? Math.floor(offset) : 0)));
  return query.toString();
}

// Keep navigation to the task-scoped download endpoint. Display metadata and
// original paths never become URLs; the server opens the immutable snapshot.
export function collectedArtifactDownloadHref(taskId: string, artifactId: string): string | undefined {
  if (!/^[A-Za-z0-9_-]+$/.test(taskId) || !/^[a-f0-9]{64}$/.test(artifactId)) return undefined;
  return `/api/tasks/${encodeURIComponent(taskId)}/artifacts/${artifactId}/download`;
}

export function artifactPageRange(offset: number, count: number, total: number): { from: number; to: number } {
  if (count <= 0 || total <= 0) return { from: 0, to: 0 };
  return { from: Math.min(offset + 1, total), to: Math.min(offset + count, total) };
}
