"use client";

import * as React from "react";

import { ClipboardListIcon, ExternalLinkIcon, RefreshCwIcon } from "lucide-react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle, SheetTrigger } from "@/components/ui/sheet";
import { intlLocale, t } from "@/i18n";
import { api } from "@/lib/api";
import { setVisibleInterval } from "@/lib/polling";
import { cn } from "@/lib/utils";
import {
  orderedWorkRequests,
  type WorkRequest,
  workRequestIsUnresolved,
  workRequestNodeLabel,
  workRequestStateLabel,
} from "@/lib/work-requests";

function RequestState({ state }: { state: string }) {
  return (
    <Badge
      variant="secondary"
      className={cn("shrink-0 text-[10px]", {
        "bg-amber-500/10 text-amber-700 dark:text-amber-400": state === "blocked" || state === "review",
        "bg-blue-500/10 text-blue-700 dark:text-blue-400": state === "active",
        "bg-emerald-500/10 text-emerald-700 dark:text-emerald-400": state === "completed",
      })}
    >
      {t(workRequestStateLabel(state))}
    </Badge>
  );
}

export function RequestedWork({ taskId, onOpenGraph }: { taskId: string; onOpenGraph?: () => void }) {
  const [open, setOpen] = React.useState(false);
  const [showResolved, setShowResolved] = React.useState(false);
  const [snapshot, setSnapshot] = React.useState<{ taskId: string; requests: WorkRequest[] } | null>(null);
  const [error, setError] = React.useState(false);
  const [loading, setLoading] = React.useState(false);
  const [reviewing, setReviewing] = React.useState<number | null>(null);
  const [reviewNotice, setReviewNotice] = React.useState<{ id: number; text: string } | null>(null);
  const reviewInFlight = React.useRef(false);
  const generation = React.useRef(0);
  const inFlight = React.useRef(false);
  const load = React.useCallback(async () => {
    if (inFlight.current) return;
    inFlight.current = true;
    const version = generation.current;
    setLoading(true);
    try {
      const result = await api.workRequests(taskId);
      if (version !== generation.current) return;
      setSnapshot({ taskId, requests: orderedWorkRequests(result.requests ?? []) });
      setError(false);
    } catch {
      if (version === generation.current) setError(true);
    } finally {
      if (version === generation.current) {
        inFlight.current = false;
        setLoading(false);
      }
    }
  }, [taskId]);
  React.useEffect(() => {
    generation.current++;
    inFlight.current = false;
    setError(false);
    setOpen(false);
    setShowResolved(false);
    setReviewing(null);
    setReviewNotice(null);
    reviewInFlight.current = false;
    void load();
    const stop = setVisibleInterval(() => void load(), 5000);
    return () => {
      generation.current++;
      inFlight.current = false;
      stop();
    };
  }, [load]);
  async function reviewRequest(request: WorkRequest) {
    if (reviewInFlight.current || !workRequestIsUnresolved(request)) return;
    const version = generation.current;
    reviewInFlight.current = true;
    setReviewing(request.id);
    setReviewNotice(null);
    try {
      const result = await api.reviewWorkRequest(taskId, request.id);
      if (version !== generation.current) return;
      const message = result.waiting_reason || t("Request queued for Planner review.");
      setReviewNotice({ id: request.id, text: message });
      toast.info(message);
      void load();
    } catch (error) {
      if (version !== generation.current) return;
      const message = t("Review could not be queued: {error}", { error: (error as Error).message });
      setReviewNotice({ id: request.id, text: message });
      toast.error(message);
    } finally {
      if (version === generation.current) {
        reviewInFlight.current = false;
        setReviewing(null);
      }
    }
  }
  const requests = snapshot?.taskId === taskId ? snapshot.requests : [];
  const unresolved = requests.filter(workRequestIsUnresolved);
  const visible = showResolved || unresolved.length === 0 ? requests : unresolved;
  if (!requests.length && !error && !open) return null;

  return (
    <Sheet open={open} onOpenChange={setOpen}>
      <SheetTrigger asChild>
        <button
          type="button"
          className="mt-2 flex w-full items-center gap-2 rounded-md px-1 py-1.5 text-left text-xs hover:bg-accent focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
        >
          <ClipboardListIcon className="size-3.5 shrink-0 text-muted-foreground" />
          <span className="flex-1">{t("Requested work")}</span>
          <span className={cn("text-[10px] text-muted-foreground", error && "text-amber-600 dark:text-amber-400")}>
            {error ? t("Status unavailable") : t("{count} unresolved", { count: unresolved.length })}
          </span>
        </button>
      </SheetTrigger>
      <SheetContent className="flex w-full flex-col gap-0 overflow-hidden sm:max-w-xl">
        <SheetHeader className="border-b">
          <SheetTitle>{t("Requested work")}</SheetTitle>
          <SheetDescription>
            {t(
              "Tracks request outcomes separately from chat delivery. A reviewed message does not mean its work is complete.",
            )}
          </SheetDescription>
        </SheetHeader>
        <div className="flex items-center gap-2 border-b px-4 py-2 text-xs">
          <span className="flex-1 text-muted-foreground">{t("{count} unresolved", { count: unresolved.length })}</span>
          {unresolved.length > 0 && unresolved.length < requests.length && (
            <Button size="sm" variant="ghost" className="h-7 text-xs" onClick={() => setShowResolved(!showResolved)}>
              {showResolved ? t("Show unresolved") : t("Include resolved")}
            </Button>
          )}
          <Button
            size="icon-sm"
            variant="ghost"
            aria-label={t("Refresh work status")}
            onClick={() => void load()}
            disabled={loading}
          >
            <RefreshCwIcon className={cn("size-3.5", loading && "animate-spin")} />
          </Button>
        </div>
        {error && (
          <p role="status" className="border-b px-4 py-2 text-xs text-amber-700 dark:text-amber-400">
            {t("Work status could not be refreshed. Previously loaded records may be out of date.")}
          </p>
        )}
        <div className="min-h-0 flex-1 overflow-y-auto px-4">
          {!visible.length && !error && (
            <p className="py-6 text-sm text-muted-foreground">{t("No tracked work requests yet.")}</p>
          )}
          {visible.map((request) => (
            <details key={request.id} className="group border-b py-3 last:border-b-0">
              <summary className="cursor-pointer rounded-sm text-sm focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring">
                <span className="ml-1 inline-flex max-w-[calc(100%-1.5rem)] items-center gap-2 align-middle">
                  <span className="shrink-0 text-xs text-muted-foreground">#{request.id}</span>
                  <span className="min-w-0 flex-1 truncate">
                    {request.acceptance || request.nodes?.[0]?.summary || t("Request #{id}", { id: request.id })}
                  </span>
                  <RequestState state={request.state} />
                </span>
              </summary>
              <div className="mt-3 space-y-3 pl-4 text-xs">
                <p className="text-muted-foreground">
                  {t("Message #{id} · Revision {revision}", {
                    id: request.source_message_id,
                    revision: request.revision,
                  })}
                </p>
                {request.review_requested && (
                  <p className="text-amber-700 dark:text-amber-400">{t("Planner review requested")}</p>
                )}
                {workRequestIsUnresolved(request) && (
                  <div className="space-y-1.5">
                    <Button
                      size="sm"
                      variant="outline"
                      className="h-7 text-xs"
                      disabled={reviewing !== null}
                      onClick={() => void reviewRequest(request)}
                    >
                      <RefreshCwIcon className={cn("size-3", reviewing === request.id && "animate-spin")} />
                      {t("Review request")}
                    </Button>
                    <p className="text-muted-foreground">{t("Review saved progress and remaining work.")}</p>
                    {reviewNotice?.id === request.id && <p role="status">{reviewNotice.text}</p>}
                  </div>
                )}
                {request.acceptance && (
                  <div>
                    <p className="mb-1 font-medium">{t("Acceptance criteria")}</p>
                    <p className="whitespace-pre-wrap break-words text-muted-foreground">{request.acceptance}</p>
                  </div>
                )}
                {request.resolution && (
                  <div>
                    <p className="mb-1 font-medium">{t("Outcome or blocker")}</p>
                    <p className="whitespace-pre-wrap break-words text-muted-foreground">{request.resolution}</p>
                  </div>
                )}
                <div>
                  <p className="mb-1.5 font-medium">{t("Linked work")}</p>
                  {request.nodes?.length ? (
                    <ul className="space-y-2">
                      {request.nodes.map((node) => (
                        <li key={node.id} className="space-y-0.5">
                          <div className="flex flex-wrap items-center gap-x-2 text-muted-foreground">
                            <span>
                              {t(workRequestNodeLabel(node.kind))}{" "}
                              {node.node_id ? `#${node.node_id}` : t("Not created yet")}
                            </span>
                            <code className="text-[10px]">{node.state}</code>
                          </div>
                          <p className="whitespace-pre-wrap break-words">{node.summary}</p>
                        </li>
                      ))}
                    </ul>
                  ) : (
                    <p className="text-muted-foreground">{t("No linked work yet. This request remains tracked.")}</p>
                  )}
                </div>
                <p className="text-[10px] text-muted-foreground">
                  {t("Updated {time}", { time: new Date(request.updated_at).toLocaleString(intlLocale) })}
                </p>
              </div>
            </details>
          ))}
        </div>
        <div className="space-y-2 border-t p-4">
          {onOpenGraph && (
            <Button
              variant="outline"
              size="sm"
              className="w-full"
              onClick={() => {
                setOpen(false);
                onOpenGraph();
              }}
            >
              <ExternalLinkIcon className="size-3.5" />
              {t("View exploration graph")}
            </Button>
          )}
          <p className="text-[10px] text-muted-foreground">
            {t("Up to 200 recent requests. Unresolved work is shown first.")}
          </p>
        </div>
      </SheetContent>
    </Sheet>
  );
}
