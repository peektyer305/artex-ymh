"use client";
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

import type { TaskSessionsController } from "../_hooks/use-task-sessions";

type Props = Pick<
  TaskSessionsController,
  "confirmNewMain" | "createMainSession" | "creatingMain" | "setConfirmNewMain"
>;

export function NewMainSessionDialog({ confirmNewMain, createMainSession, creatingMain, setConfirmNewMain }: Props) {
  return (
    <AlertDialog open={confirmNewMain} onOpenChange={(open) => !open && setConfirmNewMain(false)}>
      <AlertDialogContent className="max-w-[min(32rem,calc(100vw-2rem))]">
        <AlertDialogHeader>
          <AlertDialogTitle>新しいセッションを開始しますか？</AlertDialogTitle>
          <AlertDialogDescription className="break-words whitespace-normal">
            新しい会話を始めます。これまでの会話はセッション一覧から開けます。タスクのグラフ、アセット、ゴールは引き継がれます。
          </AlertDialogDescription>
        </AlertDialogHeader>
        <AlertDialogFooter>
          <AlertDialogCancel disabled={creatingMain}>キャンセル</AlertDialogCancel>
          <AlertDialogAction disabled={creatingMain} onClick={() => void createMainSession()}>
            {creatingMain ? "開始中…" : "新しいセッションを開始"}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}
