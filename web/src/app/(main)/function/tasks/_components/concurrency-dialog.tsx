"use client";

import * as React from "react";
import { Loader2Icon, SlidersHorizontalIcon } from "lucide-react";
import { toast } from "sonner";
import { Button } from "@/components/ui/button";
import { Dialog, DialogClose, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle, DialogTrigger } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { api } from "@/lib/api";

export function ConcurrencySettingsDialog() {
  const [open, setOpen] = React.useState(false);
  const [enabled, setEnabled] = React.useState(false);
  const [limit, setLimit] = React.useState("5");
  const [loading, setLoading] = React.useState(false);
  const [saving, setSaving] = React.useState(false);

  React.useEffect(() => {
    if (!open) return;
    setLoading(true);
    api
      .settings()
      .then((settings) => {
        setEnabled(!!settings.task_concurrency_enabled);
        setLimit(String(settings.task_concurrency_limit ?? 5));
      })
      .catch(() => {
        // Keep the dialog usable with defaults; reopening retries the request.
      })
      .finally(() => setLoading(false));
  }, [open]);

  async function save() {
    const nextLimit = Math.max(1, Math.floor(Number(limit) || 5));
    setSaving(true);
    try {
      await api.setSettings({ task_concurrency_enabled: enabled, task_concurrency_limit: nextLimit });
      toast.success(enabled ? `同時実行の制限をオンにしました：最大 ${nextLimit} タスク` : "タスクの同時実行制限をオフにしました");
      setOpen(false);
    } catch (error) {
      toast.error(`保存に失敗しました：${(error as Error).message}`);
    } finally {
      setSaving(false);
    }
  }

  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger asChild>
        <Button size="sm" variant="outline" className="ml-auto" aria-label="タスク同時実行の設定">
          <SlidersHorizontalIcon /> 同時実行の設定
        </Button>
      </DialogTrigger>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>タスクの同時実行制限</DialogTitle>
          <DialogDescription>
            同時に実行するタスク数を制限します。上限に達すると、新しいタスクは作成順に待機し、空きが出ると自動で開始します。
          </DialogDescription>
        </DialogHeader>
        <div className="grid gap-5 py-2">
          <div className="flex items-center justify-between gap-4">
            <div className="grid gap-1">
              <Label htmlFor="task-concurrency-enabled">タスクの同時実行制限をオン</Label>
              <span className="text-muted-foreground text-xs">デフォルトはオフ</span>
            </div>
            <Switch id="task-concurrency-enabled" checked={enabled} onCheckedChange={setEnabled} disabled={loading} />
          </div>
          {enabled && (
            <div className="grid gap-2">
              <Label htmlFor="task-concurrency-limit">同時実行の上限</Label>
              <Input
                id="task-concurrency-limit"
                type="number"
                min={1}
                className="w-32"
                value={limit}
                onChange={(event) => setLimit(event.target.value)}
                disabled={loading}
              />
            </div>
          )}
        </div>
        <DialogFooter>
          <DialogClose asChild>
            <Button variant="outline">キャンセル</Button>
          </DialogClose>
          <Button onClick={save} disabled={loading || saving}>
            {saving && <Loader2Icon className="animate-spin" />} 保存
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
