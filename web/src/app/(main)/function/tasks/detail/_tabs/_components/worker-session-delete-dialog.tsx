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
          <AlertDialogTitle>ワーカー #{cancelIntent?.intent_id} を削除しますか？</AlertDialogTitle>
          <AlertDialogDescription className="break-words whitespace-normal">
            {deleteMode === "hard" ? (
              <>
                <strong>関連データも削除</strong>を選ぶと、この実行指示と、<strong>この実行指示だけから得られた</strong>
                記録を削除します。共有している記録、ゴール、タスク開始時の情報は残ります。
                <strong>この操作は元に戻せません。</strong>プランナーは削除通知を受け取り、計画を立て直します。
              </>
            ) : (
              <>
                <strong>記録を残して削除</strong>を選ぶと、この実行指示を「削除済み」にし、理由を記録します。実行指示、実行履歴、登録済みの事実と脆弱性は<strong>すべて保持されます</strong>。プランナーは削除理由を受け取り、計画を立て直します。
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
              <div className="font-medium">記録を残して削除</div>
              <div className="text-xs text-muted-foreground">実行履歴と調査結果を残します</div>
            </button>
            <button
              type="button"
              onClick={() => setDeleteMode("hard")}
              className={cn(
                "rounded-md border px-3 py-2 text-left text-sm transition-colors",
                deleteMode === "hard" ? "border-destructive bg-destructive/5" : "hover:bg-accent",
              )}
            >
              <div className="font-medium">関連データも削除</div>
              <div className="text-xs text-muted-foreground">この指示だけの記録も削除します。元に戻せません</div>
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
              placeholder="削除する理由を入力してください（例：調査方針の変更、ゴールの変更、別の実行指示との重複）"
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
            {deleteMode === "hard" ? "関連データも削除する" : "削除する"}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}
