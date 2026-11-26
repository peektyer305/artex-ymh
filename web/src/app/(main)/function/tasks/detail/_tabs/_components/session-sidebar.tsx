"use client";
import { ChevronDownIcon, Loader2Icon, PlusIcon, RotateCwIcon, WifiOffIcon } from "lucide-react";

import { ScrollArea } from "@/components/ui/scroll-area";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { MOCK } from "@/lib/mock/enabled";
import { cn } from "@/lib/utils";

import type { TaskSessionsController } from "../_hooks/use-task-sessions";
import { keyForSession } from "../_lib/session-model";
import { roleMeta, SessionItem, TokenMetrics } from "./sessions-display";

type Props = Pick<
  TaskSessionsController,
  | "activeDisplayTitle"
  | "activeId"
  | "approvalFocus"
  | "controllingIntent"
  | "creatingMain"
  | "grouped"
  | "hasPendingForSession"
  | "hiddenUnread"
  | "intentsHasMore"
  | "listOpen"
  | "loadOlderIntents"
  | "loadingOlderIntents"
  | "mainLoaded"
  | "sessionMeta"
  | "setActiveId"
  | "setCancelIntent"
  | "setConfirmNewMain"
  | "setListOpen"
  | "setWorkerMessage"
  | "setWorkerMessageRequestId"
  | "sseLive"
  | "store"
  | "taskTokens"
  | "workerSessions"
>;

export function SessionSidebar({
  activeDisplayTitle,
  activeId,
  approvalFocus,
  controllingIntent,
  creatingMain,
  grouped,
  hasPendingForSession,
  hiddenUnread,
  intentsHasMore,
  listOpen,
  loadOlderIntents,
  loadingOlderIntents,
  mainLoaded,
  sessionMeta,
  setActiveId,
  setCancelIntent,
  setConfirmNewMain,
  setListOpen,
  setWorkerMessage,
  setWorkerMessageRequestId,
  sseLive,
  store,
  taskTokens,
  workerSessions,
}: Props) {
  return (
    <div className="flex min-h-0 flex-col overflow-hidden rounded-lg border bg-card">
      <div className="border-b px-3 py-2">
        <div className="flex items-center justify-between gap-2">
          <button
            type="button"
            onClick={() => setListOpen((open) => !open)}
            aria-expanded={listOpen}
            aria-controls="session-list-panel"
            className="flex min-w-0 flex-1 items-center gap-1 text-left text-xs font-medium text-muted-foreground lg:pointer-events-none"
          >
            <ChevronDownIcon
              className={cn("size-3.5 shrink-0 transition-transform lg:hidden", !listOpen && "-rotate-90")}
            />
            <span className="shrink-0">セッション一覧</span>
            {!listOpen && (
              <>
                <span className="min-w-0 truncate text-foreground lg:hidden" title={activeDisplayTitle}>
                  · {activeDisplayTitle}
                </span>
                {hiddenUnread > 0 && (
                  <span className="inline-flex min-w-4 shrink-0 items-center justify-center rounded-full bg-blue-500/15 px-1 text-[10px] font-medium tabular-nums text-blue-600 lg:hidden dark:text-blue-400">
                    {hiddenUnread > 99 ? "99+" : hiddenUnread}
                  </span>
                )}
              </>
            )}
          </button>
          {!MOCK && (
            <span
              className={cn(
                "inline-flex items-center gap-1 text-[10px]",
                sseLive ? "text-emerald-500" : "text-amber-500",
              )}
              title={sseLive ? "リアルタイム接続は正常" : "リアルタイム接続が切断、自動再接続中（履歴は表示可能）"}
            >
              {sseLive ? (
                <>
                  <span className="size-1 animate-pulse rounded-full bg-emerald-500" />
                  リアルタイム
                </>
              ) : (
                <>
                  <WifiOffIcon className="size-3" />
                  再接続中
                </>
              )}
            </span>
          )}
        </div>
        {taskTokens && (
          <Tooltip>
            <TooltipTrigger asChild>
              <div className="mt-2 flex min-w-0 flex-wrap items-center gap-1 text-xs text-muted-foreground">
                <span>タスク合計</span>
                <span>·</span>
                <TokenMetrics
                  input={taskTokens.input_tokens}
                  cache={taskTokens.cache_read_tokens}
                  output={taskTokens.output_tokens}
                />
              </div>
            </TooltipTrigger>
            <TooltipContent>
              入力 {taskTokens.input_tokens.toLocaleString()} · 出力 {taskTokens.output_tokens.toLocaleString()} ·
              キャッシュ読み込み {taskTokens.cache_read_tokens.toLocaleString()} · キャッシュ書き込み{" "}
              {taskTokens.cache_write_tokens.toLocaleString()}
            </TooltipContent>
          </Tooltip>
        )}
      </div>
      <ScrollArea
        id="session-list-panel"
        type="auto"
        className={cn("min-h-0 flex-1 [&_[data-slot=scroll-area-viewport]>div]:block!", !listOpen && "max-lg:hidden")}
      >
        <div className="flex w-full flex-col gap-3 p-2">
          {(["mainagent", "planner", "system", "worker"] as const).map((role) => {
            const items = grouped[role];
            if (!items.length) return null;
            const Meta = roleMeta[role];
            return (
              <div key={role} className="flex flex-col gap-0.5">
                <div className="flex items-center gap-1.5 px-2 py-1 text-xs font-medium text-muted-foreground">
                  <Meta.icon className="size-3.5" />
                  {Meta.label}
                  {role === "mainagent" && (
                    <button
                      type="button"
                      onClick={() => setConfirmNewMain(true)}
                      disabled={creatingMain}
                      title="メイン Agent のセッションを新規作成（コンテキストを初期化、タスク状態は保持）"
                      aria-label="メイン Agent のセッションを新規作成"
                      className="ml-auto flex items-center gap-1 rounded px-1.5 py-0.5 text-[11px] text-muted-foreground hover:bg-accent/60 hover:text-foreground disabled:opacity-50"
                    >
                      {creatingMain ? (
                        <Loader2Icon className="size-3.5 animate-spin" />
                      ) : (
                        <PlusIcon className="size-3.5" />
                      )}
                      新規作成
                    </button>
                  )}
                </div>
                {items.map((s) => {
                  const meta = s.role === "worker" ? sessionMeta.get(s.id) : undefined;
                  return (
                    <SessionItem
                      key={s.id}
                      s={s}
                      active={s.id === activeId}
                      displayTitle={meta?.title ?? s.title}
                      hasPending={hasPendingForSession(s)}
                      unread={store[keyForSession(s)]?.unread}
                      onClick={() => {
                        approvalFocus.close();
                        setActiveId(s.id);
                        setListOpen(false); // モバイルでは選択後に折りたたみ、高さをセッション記録へ戻す
                        setWorkerMessage("");
                        setWorkerMessageRequestId("");
                      }}
                      controlling={controllingIntent === s.intent_id}
                      deleted={meta?.deleted}
                      onCancel={() => {
                        setCancelIntent(s);
                      }}
                    />
                  );
                })}
                {role === "worker" && intentsHasMore && (
                  <button
                    type="button"
                    onClick={loadOlderIntents}
                    disabled={loadingOlderIntents}
                    className="mt-0.5 flex items-center justify-center gap-1 rounded-md px-2 py-1 text-xs text-muted-foreground hover:bg-accent/50"
                  >
                    {loadingOlderIntents ? (
                      <Loader2Icon className="size-3.5 animate-spin" />
                    ) : (
                      <RotateCwIcon className="size-3.5" />
                    )}
                    以前の Worker を読み込む
                  </button>
                )}
              </div>
            );
          })}
          {mainLoaded && !workerSessions.length && (
            <div className="px-2 py-1 text-xs text-muted-foreground">実行中の Worker セッションはありません。</div>
          )}
        </div>
      </ScrollArea>
    </div>
  );
}
