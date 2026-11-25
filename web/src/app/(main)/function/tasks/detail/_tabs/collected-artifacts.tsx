"use client";

import * as React from "react";

import { DownloadIcon, FileIcon, RefreshCwIcon, SearchIcon } from "lucide-react";

import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from "@/components/ui/sheet";
import { Spinner } from "@/components/ui/spinner";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { intlLocale, t } from "@/i18n";
import { api } from "@/lib/api";
import {
  ARTIFACT_CATEGORIES,
  artifactPageRange,
  COLLECTED_ARTIFACT_PAGE_SIZE,
  collectedArtifactDownloadHref,
  collectedArtifactQuery,
} from "@/lib/collected-artifacts";
import { MOCK } from "@/lib/mock/enabled";
import { setVisibleInterval } from "@/lib/polling";
import type {
  ArtifactCategory,
  CollectedArtifact,
  CollectedArtifactListing,
  CollectedArtifactPreview,
} from "@/lib/types";

export function artifactCategoryLabel(category: ArtifactCategory): string {
  switch (category) {
    case "database":
      return t("Databases");
    case "source":
      return t("Source code");
    case "config":
      return t("Configuration files");
    case "document":
      return t("Documents");
    case "evidence":
      return t("Evidence");
    default:
      return t("Other");
  }
}

export function artifactSize(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KiB`;
  if (bytes < 1024 * 1024 * 1024) return `${(bytes / 1024 / 1024).toFixed(1)} MiB`;
  return `${(bytes / 1024 / 1024 / 1024).toFixed(1)} GiB`;
}

function mockDownload(event: React.MouseEvent<HTMLAnchorElement>, entry: CollectedArtifact) {
  if (!MOCK) return;
  event.preventDefault();
  const url = URL.createObjectURL(
    new Blob([`Synthetic artifact: ${entry.name}\nNo production content.\n`], { type: "text/plain" }),
  );
  const link = document.createElement("a");
  link.href = url;
  link.download = entry.name;
  link.click();
  URL.revokeObjectURL(url);
}

export function CollectedArtifacts({ taskId, onOpenWorkspace }: { taskId: string; onOpenWorkspace: () => void }) {
  const [category, setCategory] = React.useState("all");
  const [search, setSearch] = React.useState("");
  const [offset, setOffset] = React.useState(0);
  const [page, setPage] = React.useState<CollectedArtifactListing>({
    items: [],
    total: 0,
    limit: COLLECTED_ARTIFACT_PAGE_SIZE,
    offset: 0,
  });
  const [loading, setLoading] = React.useState(true);
  const [error, setError] = React.useState("");
  const [selected, setSelected] = React.useState<CollectedArtifact | null>(null);
  const [preview, setPreview] = React.useState<CollectedArtifactPreview | null>(null);
  const [previewLoading, setPreviewLoading] = React.useState(false);
  const [previewError, setPreviewError] = React.useState("");
  const listRequest = React.useRef(0);
  const previewRequest = React.useRef(0);

  const load = React.useCallback(async () => {
    const request = ++listRequest.current;
    setLoading(true);
    setError("");
    try {
      const result = await api.taskArtifacts(taskId, collectedArtifactQuery(category, search, offset));
      if (request !== listRequest.current) return;
      // A concurrent deletion can leave a now-empty final page. Return to the
      // beginning rather than showing an empty catalogue with a nonzero count.
      if (offset > 0 && result.items.length === 0 && result.total <= offset) {
        setOffset(0);
        return;
      }
      setPage(result);
    } catch (cause) {
      if (request === listRequest.current) {
        setError((cause as Error).message);
        setPage({ items: [], total: 0, limit: COLLECTED_ARTIFACT_PAGE_SIZE, offset });
      }
    } finally {
      if (request === listRequest.current) setLoading(false);
    }
  }, [taskId, category, search, offset]);

  React.useEffect(() => {
    // Debounce typing, invalidate previous responses immediately, and avoid
    // showing old query results as if they belonged to the new filter.
    ++listRequest.current;
    setLoading(true);
    setPage({ items: [], total: 0, limit: COLLECTED_ARTIFACT_PAGE_SIZE, offset });
    const timer = window.setTimeout(() => void load(), 200);
    return () => {
      window.clearTimeout(timer);
      ++listRequest.current;
    };
  }, [load, offset]);

  React.useEffect(() => setVisibleInterval(load, 10_000), [load]);

  React.useEffect(
    () => () => {
      ++previewRequest.current;
    },
    [],
  );

  function selectArtifact(entry: CollectedArtifact | null) {
    ++previewRequest.current;
    setSelected(entry);
    setPreview(null);
    setPreviewLoading(false);
    setPreviewError("");
  }

  async function loadPreview(entry: CollectedArtifact) {
    const request = ++previewRequest.current;
    setPreviewLoading(true);
    setPreviewError("");
    try {
      const file = await api.taskArtifactPreview(taskId, entry.id);
      if (request !== previewRequest.current) return;
      if (file.id !== entry.id) throw new Error(t("The artifact preview did not match the selected file."));
      setPreview(file);
    } catch (cause) {
      if (request === previewRequest.current) setPreviewError((cause as Error).message);
    } finally {
      if (request === previewRequest.current) setPreviewLoading(false);
    }
  }

  const range = artifactPageRange(page.offset, page.items.length, page.total);
  const filtered = category !== "all" || search.trim().length > 0;
  return (
    <div className="flex flex-col gap-3">
      <div className="flex flex-wrap items-center gap-2">
        <Select
          value={category}
          onValueChange={(value) => {
            setCategory(value);
            setOffset(0);
          }}
        >
          <SelectTrigger className="w-44" aria-label={t("Artifact type")}>
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="all">{t("All types")}</SelectItem>
            {ARTIFACT_CATEGORIES.map((item) => (
              <SelectItem key={item} value={item}>
                {artifactCategoryLabel(item)}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        <div className="relative min-w-48 flex-1 sm:max-w-md">
          <SearchIcon className="pointer-events-none absolute top-2.5 left-3 size-4 text-muted-foreground" />
          <Input
            className="pl-9"
            aria-label={t("Search collected artifacts")}
            placeholder={t("Search name or source…")}
            value={search}
            onChange={(event) => {
              setSearch(event.target.value);
              setOffset(0);
            }}
          />
        </div>
        <Button size="sm" variant="outline" className="sm:ml-auto" disabled={loading} onClick={() => void load()}>
          {loading ? <Spinner /> : <RefreshCwIcon />}
          {t("Refresh")}
        </Button>
      </div>
      {error ? (
        <Alert variant="destructive">
          <AlertDescription>{t("Failed to load collected artifacts: {error}", { error })}</AlertDescription>
        </Alert>
      ) : (
        <div className="overflow-hidden rounded-lg border">
          <Table aria-label={t("Collected artifacts")} aria-busy={loading}>
            <TableHeader>
              <TableRow>
                <TableHead>{t("Name")}</TableHead>
                <TableHead className="hidden w-36 md:table-cell">{t("Type")}</TableHead>
                <TableHead className="hidden lg:table-cell">{t("Source")}</TableHead>
                <TableHead className="w-24 text-right">{t("Size")}</TableHead>
                <TableHead className="hidden w-40 xl:table-cell">{t("Collected")}</TableHead>
                <TableHead className="w-20 text-right">{t("Download")}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {page.items.length === 0 ? (
                <TableRow>
                  <TableCell colSpan={6} className="py-14 text-center">
                    {loading ? (
                      <div className="flex items-center justify-center gap-2 text-muted-foreground">
                        <Spinner />
                        {t("Loading…")}
                      </div>
                    ) : (
                      <div className="mx-auto flex max-w-lg flex-col items-center gap-2 text-sm">
                        <p className="font-medium">
                          {filtered ? t("No matching collected artifacts.") : t("No collected artifacts yet.")}
                        </p>
                        <p className="text-muted-foreground">
                          {filtered
                            ? t("Try a different type or search term.")
                            : t(
                                "Agents save database exports, source code, configuration files, and evidence here. Existing files can be added from Workspace files.",
                              )}
                        </p>
                        {!filtered && (
                          <Button variant="outline" size="sm" className="mt-2" onClick={onOpenWorkspace}>
                            {t("Open workspace files")}
                          </Button>
                        )}
                      </div>
                    )}
                  </TableCell>
                </TableRow>
              ) : (
                page.items.map((entry) => (
                  <TableRow key={entry.id}>
                    <TableCell>
                      <button
                        type="button"
                        className="flex max-w-full items-start gap-2 rounded-sm text-left hover:underline focus-visible:outline-2 focus-visible:outline-ring"
                        onClick={() => selectArtifact(entry)}
                      >
                        <FileIcon className="mt-0.5 size-4 shrink-0 text-muted-foreground" />
                        <span className="min-w-0">
                          <span className="block max-w-[42vw] truncate font-medium sm:max-w-md" title={entry.name}>
                            {entry.name}
                          </span>
                          <span className="mt-0.5 block max-w-[42vw] truncate text-muted-foreground text-xs lg:hidden">
                            {artifactCategoryLabel(entry.category)}
                            {entry.source ? ` · ${entry.source}` : ""}
                          </span>
                        </span>
                      </button>
                    </TableCell>
                    <TableCell className="hidden text-muted-foreground md:table-cell">
                      {artifactCategoryLabel(entry.category)}
                    </TableCell>
                    <TableCell
                      className="hidden max-w-56 truncate text-muted-foreground lg:table-cell"
                      title={entry.source}
                    >
                      {entry.source || t("Not specified")}
                    </TableCell>
                    <TableCell className="text-right text-muted-foreground text-xs tabular-nums">
                      {artifactSize(entry.size)}
                    </TableCell>
                    <TableCell className="hidden text-muted-foreground text-xs xl:table-cell">
                      {new Date(entry.created_at).toLocaleString(intlLocale)}
                    </TableCell>
                    <TableCell className="text-right">
                      <Button asChild variant="ghost" size="icon-sm">
                        <a
                          href={collectedArtifactDownloadHref(taskId, entry.id)}
                          download={entry.name}
                          onClick={(event) => mockDownload(event, entry)}
                          aria-label={t("Download {name}", { name: entry.name })}
                        >
                          <DownloadIcon />
                        </a>
                      </Button>
                    </TableCell>
                  </TableRow>
                ))
              )}
            </TableBody>
          </Table>
        </div>
      )}
      {!error && page.total > 0 && (
        <div className="flex items-center justify-between gap-3 text-muted-foreground text-xs">
          <span>{t("{from}–{to} of {total} artifacts", { ...range, total: page.total })}</span>
          <div className="flex gap-2">
            <Button
              size="sm"
              variant="outline"
              disabled={loading || offset === 0}
              onClick={() => setOffset(Math.max(0, offset - COLLECTED_ARTIFACT_PAGE_SIZE))}
            >
              {t("Previous")}
            </Button>
            <Button
              size="sm"
              variant="outline"
              disabled={loading || page.offset + page.items.length >= page.total}
              onClick={() => setOffset(offset + COLLECTED_ARTIFACT_PAGE_SIZE)}
            >
              {t("Next")}
            </Button>
          </div>
        </div>
      )}
      <Sheet
        open={selected !== null}
        onOpenChange={(open) => {
          if (!open) selectArtifact(null);
        }}
      >
        <SheetContent side="right" className="flex w-full flex-col gap-0 overflow-y-auto p-0 sm:max-w-3xl">
          {selected && (
            <>
              <SheetHeader className="border-b p-4 pr-12">
                <SheetTitle className="break-all text-base">{selected.name}</SheetTitle>
                <SheetDescription>
                  {artifactCategoryLabel(selected.category)} · {artifactSize(selected.size)}
                </SheetDescription>
              </SheetHeader>
              <dl className="grid grid-cols-[auto_1fr] gap-x-5 gap-y-3 border-b p-4 text-sm">
                <dt className="text-muted-foreground">{t("Source")}</dt>
                <dd className="break-all">{selected.source || t("Not specified")}</dd>
                <dt className="text-muted-foreground">{t("Collected")}</dt>
                <dd>{new Date(selected.created_at).toLocaleString(intlLocale)}</dd>
                <dt className="text-muted-foreground">{t("Saved by")}</dt>
                <dd>
                  {selected.worker}
                  {selected.intent_id > 0 ? ` (${t("Intent #{id}", { id: selected.intent_id })})` : ""}
                </dd>
                {selected.description && (
                  <>
                    <dt className="text-muted-foreground">{t("Description")}</dt>
                    <dd className="whitespace-pre-wrap break-words">{selected.description}</dd>
                  </>
                )}
                <dt className="text-muted-foreground">{t("Original file")}</dt>
                <dd className="break-all font-mono text-xs">{selected.original_path}</dd>
                <dt className="text-muted-foreground">SHA-256</dt>
                <dd className="break-all font-mono text-xs">{selected.sha256}</dd>
              </dl>
              <div className="flex flex-wrap items-center justify-between gap-2 border-b px-4 py-3">
                <Button
                  variant="outline"
                  size="sm"
                  disabled={previewLoading || preview !== null}
                  onClick={() => void loadPreview(selected)}
                >
                  {previewLoading ? <Spinner /> : <FileIcon />}
                  {t("Preview contents")}
                </Button>
                <Button asChild variant="outline" size="sm">
                  <a
                    href={collectedArtifactDownloadHref(taskId, selected.id)}
                    download={selected.name}
                    onClick={(event) => mockDownload(event, selected)}
                  >
                    <DownloadIcon />
                    {t("Download file")}
                  </a>
                </Button>
              </div>
              {previewError && (
                <div className="p-4">
                  <Alert variant="destructive">
                    <AlertDescription>{t("Failed to open file: {error}", { error: previewError })}</AlertDescription>
                  </Alert>
                </div>
              )}
              {preview &&
                (preview.too_large || preview.binary ? (
                  <p className="p-8 text-center text-muted-foreground text-sm">
                    {preview.too_large
                      ? t("This file exceeds the 2 MiB preview limit. Download it to view the full contents.")
                      : t("Binary file. Download it to open in a suitable application.")}
                  </p>
                ) : (
                  <textarea
                    readOnly
                    spellCheck={false}
                    aria-label={t("File contents")}
                    className="min-h-64 flex-1 resize-none bg-transparent p-4 font-mono text-xs leading-relaxed"
                    value={preview.content ?? ""}
                  />
                ))}
            </>
          )}
        </SheetContent>
      </Sheet>
    </div>
  );
}
