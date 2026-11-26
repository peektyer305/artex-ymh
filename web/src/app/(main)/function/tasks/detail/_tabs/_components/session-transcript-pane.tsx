"use client";
import { CircleXIcon, Loader2Icon } from "lucide-react";

import { Transcript } from "@/components/transcript";
import { Button } from "@/components/ui/button";
import { ScrollArea } from "@/components/ui/scroll-area";

import type { TaskSessionsController } from "../_hooks/use-task-sessions";

type Props = Pick<
  TaskSessionsController,
  | "active"
  | "activeKey"
  | "activeState"
  | "activity"
  | "approvalFocus"
  | "contentRef"
  | "focusHistory"
  | "isMain"
  | "loadSession"
  | "showLoader"
  | "taskId"
>;

export function SessionTranscriptPane({
  active,
  activeKey,
  activeState,
  activity,
  approvalFocus,
  contentRef,
  focusHistory,
  isMain,
  loadSession,
  showLoader,
  taskId,
}: Props) {
  return (
    <ScrollArea type="auto" className="min-h-0 min-w-0 flex-1 [&_[data-slot=scroll-area-viewport]>div]:block!">
      <div className="min-w-0 max-w-full p-4" ref={contentRef}>
        {activeState?.loadingMore && (
          <div className="flex items-center justify-center gap-2 pb-2 text-xs text-muted-foreground">
            <Loader2Icon className="size-3.5 animate-spin" />
            以前の履歴を読み込み中…
          </div>
        )}
        {showLoader ? (
          <div className="flex items-center gap-2 pl-9 text-xs text-muted-foreground">
            <Loader2Icon className="size-3.5 animate-spin" />
            アクティビティストリームを読み込み中…
          </div>
        ) : activeState?.error ? (
          <div className="flex items-center gap-2 pl-9 text-xs text-red-500">
            <CircleXIcon className="size-3.5" />
            読み込みに失敗しました：{activeState.error}
            <Button size="sm" variant="ghost" className="h-6 px-2 text-xs" onClick={() => loadSession(activeKey)}>
              再試行
            </Button>
          </div>
        ) : activity.length ? (
          <Transcript
            activity={activity}
            live={active.live}
            taskId={taskId}
            chat={isMain}
            focusedSeq={focusHistory.ready ? approvalFocus.state?.source?.seq : undefined}
          />
        ) : (
          <div className="pl-9 text-xs text-muted-foreground">
            {isMain
              ? "まだ会話がありません。下からメイン Agent にメッセージを送り、探索の方向づけやフローへの介入ができます。"
              : "アクティビティ記録はありません。"}
          </div>
        )}
      </div>
    </ScrollArea>
  );
}
