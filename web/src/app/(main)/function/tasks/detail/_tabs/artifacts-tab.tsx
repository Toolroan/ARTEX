"use client";

import * as React from "react";

import { ArchiveIcon, DownloadIcon, FileIcon, FolderIcon, KeyRoundIcon, RefreshCwIcon, SearchIcon } from "lucide-react";
import { toast } from "sonner";

import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from "@/components/ui/sheet";
import { Spinner } from "@/components/ui/spinner";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { intlLocale, t } from "@/i18n";
import { ApiError, api } from "@/lib/api";
import { MOCK } from "@/lib/mock/enabled";
import {
  ARTIFACT_PREVIEW_BYTES,
  artifactBreadcrumbs,
  artifactDownloadHref,
  isArtifactPath,
  taskArtifactRoots,
} from "@/lib/task-artifacts";
import type { WorkspaceEntry, WorkspaceFile } from "@/lib/types";

import { ArtifactCollectForm } from "./artifact-collect-form";
import { CollectedArtifacts } from "./collected-artifacts";

function formatSize(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KiB`;
  if (bytes < 1024 * 1024 * 1024) return `${(bytes / 1024 / 1024).toFixed(1)} MiB`;
  return `${(bytes / 1024 / 1024 / 1024).toFixed(1)} GiB`;
}

type Preview = {
  entry: WorkspaceEntry;
  loading: boolean;
  file?: WorkspaceFile;
  error?: string;
};

export function ArtifactsTab({ taskId, onOpenCredentials }: { taskId: string; onOpenCredentials: () => void }) {
  const [source, setSource] = React.useState<"collected" | "files" | "output">("collected");
  return (
    <div className="flex flex-col gap-4">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h2 className="font-semibold text-base">{t("Artifacts")}</h2>
          <p className="mt-1 max-w-2xl text-muted-foreground text-sm">
            {t("Collected files in one place, with their source and download details.")}
          </p>
        </div>
        <Button size="sm" variant="outline" onClick={onOpenCredentials}>
          <KeyRoundIcon />
          {t("Credentials")}
        </Button>
      </div>
      <fieldset className="flex flex-wrap gap-1 border-b pb-3" aria-label={t("Artifact location")}>
        <Button
          size="sm"
          variant={source === "collected" ? "secondary" : "ghost"}
          aria-pressed={source === "collected"}
          onClick={() => setSource("collected")}
        >
          <ArchiveIcon />
          {t("Collected artifacts")}
        </Button>
        <Button
          size="sm"
          variant={source === "files" ? "secondary" : "ghost"}
          aria-pressed={source === "files"}
          onClick={() => setSource("files")}
        >
          <FolderIcon />
          {t("Workspace files")}
        </Button>
        <Button
          size="sm"
          variant={source === "output" ? "secondary" : "ghost"}
          aria-pressed={source === "output"}
          onClick={() => setSource("output")}
        >
          <FileIcon />
          {t("Saved tool output")}
        </Button>
      </fieldset>
      {source === "collected" ? (
        <CollectedArtifacts key={taskId} taskId={taskId} onOpenWorkspace={() => setSource("files")} />
      ) : (
        <WorkspaceArtifactBrowser
          key={`${taskId}-${source}`}
          taskId={taskId}
          source={source}
          onCollected={() => setSource("collected")}
        />
      )}
    </div>
  );
}

function WorkspaceArtifactBrowser({
  taskId,
  source,
  onCollected,
}: {
  taskId: string;
  source: "files" | "output";
  onCollected: () => void;
}) {
  const roots = taskArtifactRoots(taskId);
  const root = roots?.[source] ?? "";
  const [path, setPath] = React.useState(root);
  const [entries, setEntries] = React.useState<WorkspaceEntry[]>([]);
  const [filter, setFilter] = React.useState("");
  const [loading, setLoading] = React.useState(true);
  const [missing, setMissing] = React.useState(false);
  const [error, setError] = React.useState("");
  const [preview, setPreview] = React.useState<Preview | null>(null);
  const listRequest = React.useRef(0);
  const previewRequest = React.useRef(0);

  const load = React.useCallback(
    async (nextPath: string) => {
      if (!isArtifactPath(root, nextPath)) {
        setError(t("The artifact path is outside this task folder."));
        setLoading(false);
        return;
      }
      const request = ++listRequest.current;
      ++previewRequest.current;
      setPreview(null);
      setPath(nextPath);
      setEntries([]);
      setFilter("");
      setError("");
      setMissing(false);
      setLoading(true);
      try {
        const listing = await api.workspaceList(nextPath);
        if (request !== listRequest.current) return;
        if (!isArtifactPath(root, listing.path)) throw new Error(t("The artifact path is outside this task folder."));
        setPath(listing.path);
        setEntries(listing.entries.filter((entry) => isArtifactPath(root, entry.path)));
      } catch (cause) {
        if (request !== listRequest.current) return;
        if (cause instanceof ApiError && cause.status === 404 && nextPath === root) {
          setMissing(true);
        } else {
          setError((cause as Error).message);
        }
      } finally {
        if (request === listRequest.current) setLoading(false);
      }
    },
    [root],
  );

  React.useEffect(() => {
    void load(root);
    return () => {
      ++listRequest.current;
      ++previewRequest.current;
    };
  }, [load, root]);

  async function openFile(entry: WorkspaceEntry) {
    if (!isArtifactPath(root, entry.path)) return;
    const request = ++previewRequest.current;
    if (entry.size > ARTIFACT_PREVIEW_BYTES) {
      setPreview({
        entry,
        loading: false,
        file: { path: entry.path, size: entry.size, binary: true, too_large: true },
      });
      return;
    }
    setPreview({ entry, loading: true });
    try {
      const file = await api.workspaceRead(entry.path);
      if (request !== previewRequest.current) return;
      if (!isArtifactPath(root, file.path)) throw new Error(t("The artifact path is outside this task folder."));
      setPreview({ entry, loading: false, file });
    } catch (cause) {
      if (request === previewRequest.current) setPreview({ entry, loading: false, error: (cause as Error).message });
    }
  }

  function downloadMock(event: React.MouseEvent<HTMLAnchorElement>, filePath: string) {
    // The standalone demo has no Go download endpoint. Real downloads use the
    // native link and session cookie, streaming directly to the browser's file
    // manager instead of buffering a potentially large dump in JavaScript.
    if (!MOCK) return;
    event.preventDefault();
    void api.workspaceDownload(filePath).catch((cause) => {
      toast.error(t("Download failed: {error}", { error: (cause as Error).message }));
    });
  }

  const visibleEntries = entries.filter((entry) => entry.name.toLocaleLowerCase().includes(filter.toLocaleLowerCase()));
  const crumbs = artifactBreadcrumbs(root, path);
  const rootLabel = source === "files" ? t("Workspace files") : t("Saved tool output");
  const previewHref = preview ? artifactDownloadHref(root, preview.entry.path) : undefined;
  let emptyMessage = t("Empty directory");
  if (loading) emptyMessage = t("Loading…");
  else if (filter) emptyMessage = t("No matching files in this folder.");
  else if (missing) emptyMessage = t("No artifacts have been saved here yet.");

  return (
    <div className="flex flex-col gap-4">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <p className="text-muted-foreground text-sm">
          {source === "files"
            ? t("Scripts, working files, and uploads. Add selected files to the collected library when ready.")
            : t("Raw output saved by tools during this task.")}
        </p>
        <Button variant="outline" size="sm" disabled={loading} onClick={() => void load(path)}>
          {loading ? <Spinner /> : <RefreshCwIcon />}
          {t("Refresh")}
        </Button>
      </div>

      <div className="flex flex-wrap items-center justify-between gap-3">
        <nav aria-label={t("Artifact folders")} className="flex min-w-0 flex-wrap items-center gap-1 text-sm">
          <button
            type="button"
            className="rounded px-1 py-0.5 hover:bg-accent"
            onClick={() => void load(root)}
            aria-current={path === root ? "page" : undefined}
          >
            {rootLabel}
          </button>
          {crumbs.map((crumb, index) => (
            <React.Fragment key={crumb.path}>
              <span className="text-muted-foreground" aria-hidden="true">
                /
              </span>
              <button
                type="button"
                className="max-w-60 truncate rounded px-1 py-0.5 hover:bg-accent"
                title={crumb.name}
                aria-current={index === crumbs.length - 1 ? "page" : undefined}
                onClick={() => void load(crumb.path)}
              >
                {crumb.name}
              </button>
            </React.Fragment>
          ))}
        </nav>
        <div className="relative w-full sm:w-64">
          <SearchIcon className="pointer-events-none absolute top-2.5 left-3 size-4 text-muted-foreground" />
          <Input
            value={filter}
            onChange={(event) => setFilter(event.target.value)}
            className="pl-9"
            placeholder={t("Filter this folder…")}
            aria-label={t("Filter this folder…")}
          />
        </div>
      </div>

      {error ? (
        <Alert variant="destructive">
          <AlertDescription>{t("Failed to read directory: {error}", { error })}</AlertDescription>
        </Alert>
      ) : (
        <Card>
          <CardContent className="p-0">
            <Table aria-label={t("Task artifacts")} aria-busy={loading}>
              <TableHeader>
                <TableRow>
                  <TableHead>{t("Name")}</TableHead>
                  <TableHead className="w-28 text-right">{t("Size")}</TableHead>
                  <TableHead className="hidden w-44 sm:table-cell">{t("Modified")}</TableHead>
                  <TableHead className="w-24 text-right">{t("Download")}</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {visibleEntries.length === 0 && (
                  <TableRow>
                    <TableCell colSpan={4} className="py-12 text-center text-muted-foreground text-sm">
                      {emptyMessage}
                    </TableCell>
                  </TableRow>
                )}
                {visibleEntries.map((entry) => (
                  <TableRow key={entry.path}>
                    <TableCell>
                      <button
                        type="button"
                        onClick={() => (entry.dir ? void load(entry.path) : void openFile(entry))}
                        className="flex min-w-0 max-w-full items-center gap-2 text-left hover:underline"
                        title={entry.name}
                      >
                        {entry.dir ? (
                          <FolderIcon className="size-4 shrink-0 text-primary" />
                        ) : (
                          <FileIcon className="size-4 shrink-0 text-muted-foreground" />
                        )}
                        <span className="max-w-[50vw] truncate font-mono text-sm sm:max-w-lg">{entry.name}</span>
                      </button>
                    </TableCell>
                    <TableCell className="text-right font-mono text-muted-foreground text-xs">
                      {entry.dir ? "—" : formatSize(entry.size)}
                    </TableCell>
                    <TableCell className="hidden text-muted-foreground text-xs sm:table-cell">
                      {new Date(entry.mtime).toLocaleString(intlLocale)}
                    </TableCell>
                    <TableCell className="text-right">
                      {!entry.dir && (
                        <Button asChild variant="ghost" size="icon-sm">
                          <a
                            href={artifactDownloadHref(root, entry.path)}
                            download={entry.name}
                            onClick={(event) => downloadMock(event, entry.path)}
                            aria-label={t("Download {name}", { name: entry.name })}
                            title={t("Download")}
                          >
                            <DownloadIcon />
                          </a>
                        </Button>
                      )}
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </CardContent>
        </Card>
      )}

      <Sheet
        open={preview !== null}
        onOpenChange={(open) => {
          if (!open) {
            ++previewRequest.current;
            setPreview(null);
          }
        }}
      >
        <SheetContent side="right" className="flex w-full flex-col gap-0 p-0 sm:max-w-3xl">
          {preview && (
            <>
              <SheetHeader className="border-b p-4 pr-12">
                <SheetTitle className="break-all font-mono text-sm">{preview.entry.name}</SheetTitle>
                <SheetDescription className="break-all">
                  {preview.entry.path} · {formatSize(preview.file?.size ?? preview.entry.size)}
                </SheetDescription>
              </SheetHeader>
              <div className="flex items-center justify-between gap-3 border-b px-4 py-3">
                <span className="text-muted-foreground text-xs">{t("Read-only preview")}</span>
                <Button asChild size="sm" variant="outline">
                  <a
                    href={previewHref}
                    download={preview.entry.name}
                    onClick={(event) => downloadMock(event, preview.entry.path)}
                  >
                    <DownloadIcon />
                    {t("Download file")}
                  </a>
                </Button>
              </div>
              {source === "files" && (
                <ArtifactCollectForm
                  key={preview.entry.path}
                  taskId={taskId}
                  path={preview.entry.path}
                  onCollected={onCollected}
                />
              )}
              {preview.loading && (
                <div
                  className="flex flex-1 items-center justify-center gap-2 text-muted-foreground text-sm"
                  role="status"
                >
                  <Spinner />
                  {t("Loading…")}
                </div>
              )}
              {preview.error && (
                <div className="p-4">
                  <Alert variant="destructive">
                    <AlertDescription>{t("Failed to open file: {error}", { error: preview.error })}</AlertDescription>
                  </Alert>
                </div>
              )}
              {preview.file &&
                (preview.file.too_large || preview.file.binary ? (
                  <div className="flex flex-1 items-center justify-center p-8 text-center text-muted-foreground text-sm">
                    {preview.file.too_large
                      ? t("This file exceeds the 2 MiB preview limit. Download it to view the full contents.")
                      : t("Binary file. Download it to open in a suitable application.")}
                  </div>
                ) : (
                  // Deliberately inert: never render retrieved HTML, Markdown, scripts,
                  // SVG or links. React escapes the content as plain text.
                  <textarea
                    readOnly
                    spellCheck={false}
                    aria-label={t("File contents")}
                    className="min-h-0 flex-1 resize-none overflow-auto bg-transparent p-4 font-mono text-xs leading-relaxed"
                    value={preview.file.content ?? ""}
                  />
                ))}
            </>
          )}
        </SheetContent>
      </Sheet>
    </div>
  );
}
