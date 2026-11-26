"use client";
import { Loader2Icon, Trash2Icon } from "lucide-react";

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
import { Textarea } from "@/components/ui/textarea";
import { cn } from "@/lib/utils";

import type { TaskSessionsController } from "../_hooks/use-task-sessions";

type Props = Pick<
  TaskSessionsController,
  | "cancelIntent"
  | "cancelReason"
  | "controlWorker"
  | "controllingIntent"
  | "deleteMode"
  | "setCancelIntent"
  | "setCancelReason"
  | "setDeleteMode"
>;

export function WorkerSessionDeleteDialog({
  cancelIntent,
  cancelReason,
  controlWorker,
  controllingIntent,
  deleteMode,
  setCancelIntent,
  setCancelReason,
  setDeleteMode,
}: Props) {
  return (
    <AlertDialog
      open={cancelIntent !== null}
      onOpenChange={(open) => {
        if (!open) {
          setCancelIntent(null);
          setCancelReason("");
          setDeleteMode("soft");
        }
      }}
    >
      <AlertDialogContent className="max-w-[min(32rem,calc(100vw-2rem))]">
        <AlertDialogHeader>
          <AlertDialogTitle>Worker #{cancelIntent?.intent_id} を削除しますか？</AlertDialogTitle>
          <AlertDialogDescription className="break-words whitespace-normal">
            {deleteMode === "hard" ? (
              <>
                <strong>物理削除</strong>はこの意図と、<strong>これだけに支えられた</strong>
                下流ノードを物理的に削除します（葉まで連鎖し、孤立データを残さない）。共有ノード、ゴール、タスクのルート事実は保持されます。
                <strong>この操作は元に戻せません。</strong>プランナーは削除通知を受け取り、それに基づき再計画します。
              </>
            ) : (
              <>
                <strong>論理削除</strong>はこの意図を「削除済み」にし、削除理由を記録します。意図ノード、実行記録、
                登録済みの事実と脆弱性は<strong>すべて保持されます</strong>。プランナーは「この意図はユーザーが削除 +
                理由」を受け取り、それに基づき再計画します。
              </>
            )}
          </AlertDialogDescription>
        </AlertDialogHeader>
        <div className="grid gap-3 py-1">
          <div className="grid grid-cols-2 gap-2">
            <button
              type="button"
              onClick={() => setDeleteMode("soft")}
              className={cn(
                "rounded-md border px-3 py-2 text-left text-sm transition-colors",
                deleteMode === "soft" ? "border-primary bg-primary/5" : "hover:bg-accent",
              )}
            >
              <div className="font-medium">論理削除</div>
              <div className="text-xs text-muted-foreground">データを保持し、追跡可能</div>
            </button>
            <button
              type="button"
              onClick={() => setDeleteMode("hard")}
              className={cn(
                "rounded-md border px-3 py-2 text-left text-sm transition-colors",
                deleteMode === "hard" ? "border-destructive bg-destructive/5" : "hover:bg-accent",
              )}
            >
              <div className="font-medium">物理削除</div>
              <div className="text-xs text-muted-foreground">連鎖削除、元に戻せません</div>
            </button>
          </div>
          <div className="grid gap-2">
            <label htmlFor="cancel-reason" className="text-sm font-medium">
              削除理由（必須）
            </label>
            <Textarea
              id="cancel-reason"
              value={cancelReason}
              onChange={(e) => setCancelReason(e.target.value)}
              placeholder="この意図を削除する理由を記入（例：方向判断の誤り / ゴールが失効 / 他の意図と重複…）"
              rows={3}
              autoFocus
            />
          </div>
        </div>
        <AlertDialogFooter>
          <AlertDialogCancel>戻る</AlertDialogCancel>
          <AlertDialogAction
            variant="destructive"
            disabled={!cancelIntent || controllingIntent !== null || !cancelReason.trim()}
            onClick={() => cancelIntent && void controlWorker(cancelIntent, "cancel", cancelReason, deleteMode)}
          >
            {controllingIntent ? <Loader2Icon className="animate-spin" /> : <Trash2Icon />}
            {deleteMode === "hard" ? "完全に削除する" : "削除の確認"}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}
