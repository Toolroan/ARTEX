// Navigation helpers only. The workspace endpoints remain responsible for
// authentication, canonical path resolution, and filesystem access checks.
export const ARTIFACT_PREVIEW_BYTES = 2 * 1024 * 1024;

export function taskArtifactRoots(taskId: string): { files: string; output: string } | null {
  if (!/^[A-Za-z0-9_-]+$/.test(taskId)) return null;
  return { files: `tasks/${taskId}`, output: `spill/tasks/${taskId}` };
}

export function isArtifactPath(root: string, path: string): boolean {
  if (!root || !path || path.includes("\\") || path.includes("\0")) return false;
  if (path.split("/").some((part) => !part || part === "." || part === "..")) return false;
  return path === root || path.startsWith(`${root}/`);
}

export function artifactDownloadHref(root: string, path: string): string | undefined {
  if (!isArtifactPath(root, path) || path === root) return undefined;
  return `/api/workspace/download?path=${encodeURIComponent(path)}`;
}

export function artifactBreadcrumbs(root: string, path: string): { name: string; path: string }[] {
  if (!isArtifactPath(root, path) || path === root) return [];
  let current = root;
  return path
    .slice(root.length + 1)
    .split("/")
    .map((name) => {
      current += `/${name}`;
      return { name, path: current };
    });
}
