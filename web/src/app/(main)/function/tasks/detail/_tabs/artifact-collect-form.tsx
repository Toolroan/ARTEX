"use client";

import * as React from "react";

import { ArchiveIcon } from "lucide-react";
import { toast } from "sonner";

import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Spinner } from "@/components/ui/spinner";
import { t } from "@/i18n";
import { api } from "@/lib/api";
import { ARTIFACT_CATEGORIES } from "@/lib/collected-artifacts";
import type { ArtifactCategory } from "@/lib/types";

import { artifactCategoryLabel } from "./collected-artifacts";

export function ArtifactCollectForm({
  taskId,
  path,
  onCollected,
}: {
  taskId: string;
  path: string;
  onCollected: () => void;
}) {
  const [open, setOpen] = React.useState(false);
  const [category, setCategory] = React.useState<ArtifactCategory>("other");
  const [source, setSource] = React.useState("");
  const [description, setDescription] = React.useState("");
  const [saving, setSaving] = React.useState(false);
  const [error, setError] = React.useState("");
  const id = React.useId();
  const mounted = React.useRef(true);
  React.useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
    };
  }, []);
  async function save(event: React.FormEvent) {
    event.preventDefault();
    if (saving) return;
    setSaving(true);
    setError("");
    try {
      await api.collectTaskArtifact(taskId, { path, category, source, description });
      if (!mounted.current) return;
      toast.success(t("Added to collected artifacts"));
      onCollected();
    } catch (cause) {
      if (mounted.current) setError((cause as Error).message);
    } finally {
      if (mounted.current) setSaving(false);
    }
  }
  if (!open)
    return (
      <div className="border-b px-4 py-3">
        <Button size="sm" variant="outline" onClick={() => setOpen(true)}>
          <ArchiveIcon />
          {t("Add to collected")}
        </Button>
      </div>
    );
  return (
    <form onSubmit={(event) => void save(event)} className="flex flex-col gap-3 border-b bg-muted/30 p-4">
      <p className="font-medium text-sm">{t("Add to collected artifacts")}</p>
      <p className="text-muted-foreground text-xs">
        {t("Save a separate copy in the collected library. The workspace file stays in place.")}
      </p>
      <div className="grid gap-3 sm:grid-cols-[11rem_1fr]">
        <div className="flex flex-col gap-1.5">
          <Label htmlFor={`${id}-category`}>{t("Artifact type")}</Label>
          <Select value={category} onValueChange={(value) => setCategory(value as ArtifactCategory)} disabled={saving}>
            <SelectTrigger id={`${id}-category`} className="w-full">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              {ARTIFACT_CATEGORIES.map((item) => (
                <SelectItem key={item} value={item}>
                  {artifactCategoryLabel(item)}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </div>
        <div className="flex flex-col gap-1.5">
          <Label htmlFor={`${id}-source`}>{t("Source (optional)")}</Label>
          <Input
            id={`${id}-source`}
            value={source}
            maxLength={1024}
            disabled={saving}
            onChange={(event) => setSource(event.target.value)}
            placeholder={t("Host or original location")}
          />
        </div>
      </div>
      <div className="flex flex-col gap-1.5">
        <Label htmlFor={`${id}-description`}>{t("Description (optional)")}</Label>
        <Input
          id={`${id}-description`}
          value={description}
          maxLength={2000}
          disabled={saving}
          onChange={(event) => setDescription(event.target.value)}
        />
      </div>
      {error && (
        <Alert variant="destructive">
          <AlertDescription>{t("Could not collect file: {error}", { error })}</AlertDescription>
        </Alert>
      )}
      <div className="flex gap-2">
        <Button type="submit" size="sm" disabled={saving}>
          {saving ? <Spinner /> : <ArchiveIcon />}
          {t("Add to collected")}
        </Button>
        <Button type="button" size="sm" variant="ghost" disabled={saving} onClick={() => setOpen(false)}>
          {t("Cancel")}
        </Button>
      </div>
    </form>
  );
}
