"use client";
import { ClockIcon } from "lucide-react";

import { SideQuestionButton } from "@/components/side-question-workspace";
import { Badge } from "@/components/ui/badge";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { fmtDurationMs as fmtDuration } from "@/lib/format";

import type { TaskSessionsController } from "../_hooks/use-task-sessions";
import { resolutionLabel, TokenMetrics, WorkerAssetBadge } from "./sessions-display";

type Props = Pick<
  TaskSessionsController,
  | "active"
  | "activeAssets"
  | "activeResolution"
  | "activeState"
  | "activeTokens"
  | "isMain"
  | "runDuration"
  | "sessionMeta"
  | "side"
  | "tokenTotal"
>;

export function SessionHeader({
  active,
  activeAssets,
  activeResolution,
  activeState,
  activeTokens,
  isMain,
  runDuration,
  sessionMeta,
  side,
  tokenTotal,
}: Props) {
  return (
    <div className="flex min-w-0 flex-wrap items-center gap-x-2 gap-y-1 border-b px-3 py-2 sm:px-4 sm:py-2.5">
      {(() => {
        const isWorker = active.role === "worker";
        const meta = isWorker ? sessionMeta.get(active.id) : undefined;
        // Worker: the intent moved into the transcript as a message, so the
        // header shows a stable generic label (intent JSON stays on hover).
        const title = isWorker ? "Worker 実行セッション" : active.title;
        const titleEl = <span className="min-w-0 truncate text-sm font-medium">{title}</span>;
        return meta?.json ? (
          <Tooltip>
            <TooltipTrigger asChild>{titleEl}</TooltipTrigger>
            <TooltipContent side="bottom" align="start" className="max-h-80 max-w-sm overflow-auto p-0">
              <pre className="p-2 text-[10px] leading-relaxed">{JSON.stringify(meta.json, null, 2)}</pre>
            </TooltipContent>
          </Tooltip>
        ) : (
          titleEl
        );
      })()}
      <SideQuestionButton side={side} />
      {activeResolution && (
        <Tooltip>
          <TooltipTrigger asChild>
            <Badge
              variant="outline"
              className="max-w-28 shrink-0 font-normal"
              aria-label={
                activeResolution.available ? `現在の設定：${resolutionLabel(activeResolution)}` : "モデルは利用不可"
              }
            >
              <span className="truncate">
                {activeResolution.available ? resolutionLabel(activeResolution) : "モデルは利用不可"}
              </span>
            </Badge>
          </TooltipTrigger>
          <TooltipContent side="bottom" className="max-w-xs [overflow-wrap:anywhere]">
            {activeResolution.available
              ? [resolutionLabel(activeResolution), activeResolution.model].filter(Boolean).join(" / ")
              : activeResolution.reason || "利用可能な LLM 設定がありません"}
          </TooltipContent>
        </Tooltip>
      )}
      {activeAssets && activeAssets.length > 0 && <WorkerAssetBadge assets={activeAssets} />}
      {active.inherited && active.source_task_id && (
        <Badge variant="outline">ソースタスク #{active.source_task_id} · 読み取り専用の履歴</Badge>
      )}
      {active.live && (
        <span className="inline-flex items-center gap-1 rounded bg-blue-500/15 px-1.5 py-0.5 text-[10px] font-medium text-blue-600 dark:text-blue-400">
          <span className="size-1 animate-pulse rounded-full bg-blue-500" />
          リアルタイム
        </span>
      )}
      {activeState?.hasMore && (
        <span className="text-[10px] text-muted-foreground" title="上へスクロールして以前の履歴を読み込む">
          ↑ 以前の履歴
        </span>
      )}
      <div className="ml-auto flex min-w-0 max-w-full items-center justify-end gap-x-3 gap-y-1 text-xs text-muted-foreground max-sm:w-full max-sm:flex-wrap">
        {tokenTotal.any && (
          <Tooltip>
            {/* モバイルでは短いラベル（入/キャ/出）を使う：長いラベルはこの行を 2 行に広げ、記録領域をさらに圧迫する。 */}
            <TooltipTrigger asChild>
              <span className="inline-flex min-w-0 items-center">
                <TokenMetrics
                  input={tokenTotal.i}
                  cache={tokenTotal.cr}
                  output={tokenTotal.o}
                  labels="short"
                  className="sm:hidden"
                />
                <TokenMetrics
                  input={tokenTotal.i}
                  cache={tokenTotal.cr}
                  output={tokenTotal.o}
                  labels="long"
                  className="max-sm:hidden"
                />
              </span>
            </TooltipTrigger>
            <TooltipContent>
              入力 {activeTokens.input_tokens.toLocaleString()} · 出力 {activeTokens.output_tokens.toLocaleString()} ·
              キャッシュ読み込み {activeTokens.cache_read_tokens.toLocaleString()} · キャッシュ書き込み{" "}
              {activeTokens.cache_write_tokens.toLocaleString()}
            </TooltipContent>
          </Tooltip>
        )}
        {runDuration != null && (
          <span className="inline-flex items-center gap-1" title="実行時間（最初のステップ → 最後のステップ）">
            <ClockIcon className="size-3" />
            {fmtDuration(runDuration)}
          </span>
        )}
        {isMain && <span>操作可能</span>}
      </div>
    </div>
  );
}
