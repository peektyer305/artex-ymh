"use client";

import * as React from "react";

import { Loader2Icon, RefreshCwIcon, UnplugIcon } from "lucide-react";
import { toast } from "sonner";

import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { api } from "@/lib/api";
import type { TunnelInfo } from "@/lib/types";
import { cn } from "@/lib/utils";

import { fmtTime } from "./format";

function StateBadge({ state }: { state: string }) {
  const cls =
    state === "alive"
      ? "text-emerald-600"
      : state === "error"
        ? "text-red-600 dark:text-red-400"
        : "text-muted-foreground";
  const dot = state === "alive" ? "bg-emerald-500" : state === "error" ? "bg-red-500" : "bg-neutral-400";
  const label = state === "alive" ? "稼働中" : state === "error" ? "失敗" : state === "stopped" ? "停止済み" : state || "—";
  return (
    <Badge variant={state === "alive" ? "secondary" : "outline"} className={cn("gap-1.5 text-xs", cls)}>
      <span className={cn("size-1.5 rounded-full", dot)} />
      {label}
    </Badge>
  );
}

// トンネル台帳：状態を色分けし、稼働中のトンネルは再確認のうえ回収（teardown）できます。
export function TunnelsTab({
  tunnels,
  loading,
  onRefresh,
}: {
  tunnels: TunnelInfo[];
  loading: boolean;
  onRefresh: () => void;
}) {
  const [pendingTeardown, setPendingTeardown] = React.useState<TunnelInfo | null>(null);
  const [tearingDown, setTearingDown] = React.useState(false);

  const confirmTeardown = () => {
    const target = pendingTeardown;
    if (!target) return;
    setTearingDown(true);
    api
      .teardownTunnel(target.id)
      .then(() => {
        toast.success(`トンネル #${target.id} を回収しました`);
        setPendingTeardown(null);
        onRefresh();
      })
      .catch((e: unknown) => toast.error(e instanceof Error ? e.message : "回収に失敗しました"))
      .finally(() => setTearingDown(false));
  };

  return (
    <>
      <div className="flex items-center justify-end gap-2 px-1 pb-2">
        <Button variant="outline" size="sm" className="h-7 gap-1" onClick={onRefresh} disabled={loading}>
          <RefreshCwIcon className={cn("size-3.5", loading && "animate-spin")} />
          更新
        </Button>
      </div>
      <Card className="flex min-h-0 flex-1 flex-col overflow-hidden py-0">
        <div className="min-h-0 flex-1 overflow-auto">
          <Table>
            <TableHeader className="sticky top-0 z-10 bg-card">
              <TableRow>
                <TableHead className="w-[70px]">ID</TableHead>
                <TableHead className="w-[90px]">タスク</TableHead>
                <TableHead className="w-[110px]">種類</TableHead>
                <TableHead className="w-[90px]">ステータス</TableHead>
                <TableHead className="w-[180px]">入口</TableHead>
                <TableHead>ターゲット</TableHead>
                <TableHead className="w-[110px]">経由セッション</TableHead>
                <TableHead className="w-[140px]">最終疎通確認</TableHead>
                <TableHead className="w-[160px]">エラー</TableHead>
                <TableHead className="w-[90px]">操作</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {loading && tunnels.length === 0 ? (
                <TableRow>
                  <TableCell colSpan={10} className="py-12 text-center">
                    <Loader2Icon className="mx-auto h-5 w-5 animate-spin text-muted-foreground" />
                  </TableCell>
                </TableRow>
              ) : tunnels.length === 0 ? (
                <TableRow>
                  <TableCell colSpan={10} className="py-12 text-center text-sm text-muted-foreground">
                    トンネルはありません
                  </TableCell>
                </TableRow>
              ) : (
                tunnels.map((t) => {
                  // バックエンドのフィールドは listen_host/listen_port/target_host/target_port です。
                  const entry = t.listen_host ? `${t.listen_host}:${t.listen_port}` : "—";
                  const target = t.target_host ? `${t.target_host}:${t.target_port}` : "—";
                  return (
                  <TableRow key={t.id}>
                    <TableCell className="font-mono text-xs text-muted-foreground">#{t.id}</TableCell>
                    <TableCell className="font-mono text-xs text-muted-foreground">
                      {t.task_id ? `#${t.task_id}` : "—"}
                    </TableCell>
                    <TableCell>
                      <Badge variant="secondary" className="font-mono text-xs">
                        {t.kind || "-"}
                      </Badge>
                    </TableCell>
                    <TableCell>
                      <StateBadge state={t.state} />
                    </TableCell>
                    <TableCell className="max-w-0">
                      <code className="block truncate font-mono text-xs" title={entry}>
                        {entry}
                      </code>
                    </TableCell>
                    <TableCell className="max-w-0">
                      <code className="block truncate font-mono text-xs" title={target}>
                        {target}
                      </code>
                    </TableCell>
                    <TableCell className="font-mono text-xs text-muted-foreground">
                      {t.via_session_id ? `#${t.via_session_id}` : "—"}
                    </TableCell>
                    <TableCell className="text-xs text-muted-foreground tabular-nums">
                      {fmtTime(t.last_check)}
                    </TableCell>
                    <TableCell className="max-w-0">
                      <span
                        className={cn(
                          "block truncate text-xs",
                          t.error ? "text-red-600 dark:text-red-400" : "text-muted-foreground",
                        )}
                        title={t.error ?? ""}
                      >
                        {t.error || "—"}
                      </span>
                    </TableCell>
                    <TableCell>
                      <Button
                        variant="outline"
                        size="sm"
                        className="h-7 gap-1"
                        disabled={t.state !== "alive"}
                        title={t.state === "alive" ? "このトンネルを回収" : "稼働中のトンネルだけ回収できます"}
                        onClick={() => setPendingTeardown(t)}
                      >
                        <UnplugIcon className="size-3.5" />
                        回収
                      </Button>
                    </TableCell>
                  </TableRow>
                  );
                })
              )}
            </TableBody>
          </Table>
        </div>
      </Card>

      <AlertDialog open={pendingTeardown !== null} onOpenChange={(o) => !o && setPendingTeardown(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>トンネル #{pendingTeardown?.id} を回収しますか？</AlertDialogTitle>
            <AlertDialogDescription>
              {pendingTeardown?.kind} トンネル（入口{" "}
              {pendingTeardown?.listen_host
                ? `${pendingTeardown.listen_host}:${pendingTeardown.listen_port}`
                : "—"}
              ）を撤去します。これを経由してプロキシ中の接続は即座に切断されます。
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel disabled={tearingDown}>キャンセル</AlertDialogCancel>
            <AlertDialogAction
              variant="destructive"
              disabled={tearingDown}
              onClick={(e) => {
                e.preventDefault();
                confirmTeardown();
              }}
            >
              {tearingDown ? "回収中…" : "回収の確認"}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </>
  );
}
