"use client";

import * as React from "react";

import { useRouter } from "next/navigation";

import { ActivityIcon, ArrowRightToLineIcon, Loader2Icon, TerminalIcon, Trash2Icon, XIcon } from "lucide-react";
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
import { safeHref } from "@/lib/safe-href";
import type { ShellSession } from "@/lib/types";
import { cn } from "@/lib/utils";

import { fmtTime } from "./format";

/** トポロジ図でホストをクリックした際に渡す絞り込み条件：host_asset_id はノード ID の場合も IP の場合もあります。 */
export interface HostFilter {
  id: string;
  ip: string;
}

function matchesHost(s: ShellSession, f: HostFilter | null): boolean {
  if (!f) return true;
  return s.host_asset_id === f.id || s.host_asset_id === f.ip;
}

function StatusBadge({ status }: { status: string }) {
  const alive = status === "alive";
  return (
    <Badge
      variant={alive ? "secondary" : "outline"}
      className={cn("gap-1.5 text-xs", alive ? "text-emerald-600" : "text-muted-foreground")}
    >
      <span className={cn("size-1.5 rounded-full", alive ? "bg-emerald-500" : "bg-neutral-400")} />
      {alive ? "稼働中" : "切断"}
    </Badge>
  );
}

// セッション（WebShell）台帳：WebShell マネージャ相当です。一覧データと 10s ポーリングはページ層が担当し、
// ここでは表示、ホスト絞り込み、ワークベンチ起動、削除のみを扱います。
export function SessionsTab({
  sessions,
  loading,
  hostFilter,
  onClearHostFilter,
  onOpenWorkbench,
  onChanged,
}: {
  sessions: ShellSession[];
  loading: boolean;
  hostFilter: HostFilter | null;
  onClearHostFilter: () => void;
  onOpenWorkbench: (s: ShellSession) => void;
  onChanged: () => void;
}) {
  const [pendingDelete, setPendingDelete] = React.useState<ShellSession | null>(null);
  const [deleting, setDeleting] = React.useState(false);
  const [probingId, setProbingId] = React.useState<string | null>(null);
  // イントラネットへの移管：確認ダイアログの対象と実行中状態。
  const [handoffTarget, setHandoffTarget] = React.useState<ShellSession | null>(null);
  const [handingOff, setHandingOff] = React.useState(false);
  const router = useRouter();

  const visible = sessions.filter((s) => matchesHost(s, hostFilter));

  // 手動疎通確認：成功時はハートビートを更新。失敗時はバックエンドが dead を記録済みなので、
  // ここで理由をトーストし一覧を更新します。
  const probe = (target: ShellSession) => {
    setProbingId(target.id);
    api
      .probeSession(target.id)
      .then((r) => {
        if (r.alive) {
          toast.success(`セッション #${target.id} の疎通確認に成功しました（${r.ms ?? "?"}ms）`);
        } else {
          toast.error(`セッション #${target.id} は切断済み、dead と記録：${r.error ?? "プローブ失敗"}`);
        }
        onChanged();
      })
      .catch((e: unknown) => toast.error(e instanceof Error ? e.message : "疎通確認に失敗しました"))
      .finally(() => setProbingId(null));
  };

  const confirmDelete = () => {
    const target = pendingDelete;
    if (!target) return;
    setDeleting(true);
    api
      .deleteSession(target.id)
      .then(() => {
        toast.success(`セッション #${target.id} を削除しました`);
        setPendingDelete(null);
        onChanged();
      })
      .catch((e: unknown) => toast.error(e instanceof Error ? e.message : "削除に失敗しました"))
      .finally(() => setDeleting(false));
  };

  // イントラネットへの移管：バックエンドがフットホールドのホストを初期 scope として新規タスクを作成し、
  // 成功後は新しいタスクの詳細ページへ遷移します。
  const confirmHandoff = () => {
    const target = handoffTarget;
    if (!target || handingOff) return;
    setHandingOff(true);
    api
      .handoffSession(target.id)
      .then((r) => {
        toast.success(`イントラネットタスク #${r.task_id} を作成しました`);
        setHandoffTarget(null);
        router.push(`/function/tasks/detail?id=${encodeURIComponent(r.task_id)}`);
      })
      .catch((e: unknown) => toast.error(e instanceof Error ? e.message : "移管に失敗しました"))
      .finally(() => setHandingOff(false));
  };

  return (
    <>
      {hostFilter && (
        <div className="flex items-center gap-2 px-1 pb-2 text-xs">
          <Badge variant="secondary" className="gap-1.5">
            ホスト：{hostFilter.ip || hostFilter.id}
            <button type="button" onClick={onClearHostFilter} className="hover:text-foreground" title="絞り込みを解除">
              <XIcon className="size-3" />
            </button>
          </Badge>
          <span className="text-muted-foreground">トポロジ図のクリックから · このホストのセッションのみ表示</span>
        </div>
      )}
      <Card className="flex min-h-0 flex-1 flex-col overflow-hidden py-0">
        <div className="min-h-0 flex-1 overflow-auto">
          <Table>
            <TableHeader className="sticky top-0 z-10 bg-card">
              <TableRow>
                <TableHead className="w-[70px]">ID</TableHead>
                <TableHead className="w-[90px]">タスク</TableHead>
                <TableHead className="w-[140px]">種別</TableHead>
                <TableHead>URL</TableHead>
                <TableHead className="w-[130px]">ホスト</TableHead>
                <TableHead className="w-[90px]">ステータス</TableHead>
                <TableHead className="w-[140px]">ハートビート</TableHead>
                <TableHead className="w-[300px]">操作</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {loading && visible.length === 0 ? (
                <TableRow>
                  <TableCell colSpan={8} className="py-12 text-center">
                    <Loader2Icon className="mx-auto h-5 w-5 animate-spin text-muted-foreground" />
                  </TableCell>
                </TableRow>
              ) : visible.length === 0 ? (
                <TableRow>
                  <TableCell colSpan={8} className="py-12 text-center text-sm text-muted-foreground">
                    {hostFilter ? "このホストにセッションはありません" : "セッションはありません"}
                  </TableCell>
                </TableRow>
              ) : (
                visible.map((s) => {
                  // s.url はターゲット側のデータです。http(s) のみ href に設定し、それ以外はプレーンテキストに降格します。
                  const href = safeHref(s.url);
                  return (
                    <TableRow key={s.id}>
                      <TableCell className="font-mono text-xs text-muted-foreground">#{s.id}</TableCell>
                      <TableCell className="font-mono text-xs text-muted-foreground">
                        {s.created_by_task ? `#${s.created_by_task}` : "—"}
                      </TableCell>
                      <TableCell>
                        <div className="flex items-center gap-1.5">
                          <Badge variant="secondary" className="font-mono text-xs">
                            {s.kind || "-"}
                          </Badge>
                          {s.lang && (
                            <Badge variant="outline" className="font-mono text-xs">
                              {s.lang}
                            </Badge>
                          )}
                        </div>
                      </TableCell>
                      <TableCell className="max-w-0">
                        {href ? (
                          <a
                            href={href}
                            target="_blank"
                            rel="noreferrer"
                            className="block truncate font-mono text-xs text-blue-600 hover:underline dark:text-blue-400"
                            title={s.url}
                          >
                            {s.url}
                          </a>
                        ) : (
                          <code className="block truncate font-mono text-xs" title={s.url}>
                            {s.url || "—"}
                          </code>
                        )}
                      </TableCell>
                      <TableCell>
                        <div className="flex flex-col">
                          <span className="truncate font-mono text-xs" title={s.host_asset_id}>
                            {s.host_asset_id || "—"}
                          </span>
                          {s.platform && <span className="text-[10px] text-muted-foreground">{s.platform}</span>}
                        </div>
                      </TableCell>
                      <TableCell>
                        <StatusBadge status={s.status} />
                      </TableCell>
                      <TableCell className="text-xs text-muted-foreground tabular-nums">
                        {fmtTime(s.last_beat)}
                      </TableCell>
                      <TableCell>
                        <div className="flex items-center gap-1">
                          <Button
                            variant="outline"
                            size="sm"
                            className="h-7 gap-1"
                            title="ターゲットに無害なプローブを送り、セッションの生存を確認"
                            disabled={probingId === s.id}
                            onClick={() => probe(s)}
                          >
                            {probingId === s.id ? (
                              <Loader2Icon className="size-3.5 animate-spin" />
                            ) : (
                              <ActivityIcon className="size-3.5" />
                            )}
                            疎通確認
                          </Button>
                          <Button variant="outline" size="sm" className="h-7 gap-1" onClick={() => onOpenWorkbench(s)}>
                            <TerminalIcon className="size-3.5" />
                            ワークベンチ
                          </Button>
                          {s.status === "alive" && (
                            <Button
                              variant="outline"
                              size="sm"
                              className="h-7 gap-1"
                              title="このセッションのフットホールドホストを初期 scope としてイントラネットタスクを作成"
                              onClick={() => setHandoffTarget(s)}
                            >
                              <ArrowRightToLineIcon className="size-3.5" />
                              イントラへ移管
                            </Button>
                          )}
                          <Button
                            variant="ghost"
                            size="icon"
                            className="size-7 text-muted-foreground hover:text-red-600"
                            title="セッションを削除"
                            onClick={() => setPendingDelete(s)}
                          >
                            <Trash2Icon className="size-3.5" />
                          </Button>
                        </div>
                      </TableCell>
                    </TableRow>
                  );
                })
              )}
            </TableBody>
          </Table>
        </div>
      </Card>

      <AlertDialog open={handoffTarget !== null} onOpenChange={(o) => !o && setHandoffTarget(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>セッション #{handoffTarget?.id} をイントラネットタスクへ移管しますか？</AlertDialogTitle>
            <AlertDialogDescription>
              新規のイントラネット探索タスクを作成します。初期のテスト範囲はこのセッションのフットホールドホスト（
              {handoffTarget?.host_asset_id || "不明"}
              ）のみで、以降で検出される隣接セグメントはタスクページでの承認後に許可されます。
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel disabled={handingOff}>キャンセル</AlertDialogCancel>
            <AlertDialogAction
              disabled={handingOff}
              onClick={(e) => {
                e.preventDefault();
                confirmHandoff();
              }}
            >
              {handingOff ? "作成中…" : "移管の確認"}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      <AlertDialog open={pendingDelete !== null} onOpenChange={(o) => !o && setPendingDelete(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>セッション #{pendingDelete?.id} を削除しますか？</AlertDialogTitle>
            <AlertDialogDescription>
              この WebShell セッション（{pendingDelete?.kind} · {pendingDelete?.host_asset_id}
              ）を切断して削除します。ターゲット上のマルウェアファイルは消去されません。この操作は取り消せません。
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel disabled={deleting}>キャンセル</AlertDialogCancel>
            <AlertDialogAction
              variant="destructive"
              disabled={deleting}
              onClick={(e) => {
                e.preventDefault();
                confirmDelete();
              }}
            >
              {deleting ? "削除中…" : "削除の確認"}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </>
  );
}
