"use client";
import { Trash2Icon } from "lucide-react";

import { ApprovalExecutionFocus } from "@/components/approval-execution-focus";
import { SideQuestionWorkspace } from "@/components/side-question-workspace";
import { TooltipProvider } from "@/components/ui/tooltip";
import { cn } from "@/lib/utils";

import { NewMainSessionDialog } from "./_components/new-main-session-dialog";
import { SessionComposer } from "./_components/session-composer";
import { SessionHeader } from "./_components/session-header";
import { SessionSidebar } from "./_components/session-sidebar";
import { SessionTranscriptPane } from "./_components/session-transcript-pane";
import { WorkerSessionDeleteDialog } from "./_components/worker-session-delete-dialog";
import { useTaskSessions } from "./_hooks/use-task-sessions";

export function SessionsTab({ taskId }: { taskId: string }) {
  const {
    active,
    activeAssets,
    activeDisplayTitle,
    activeId,
    activeKey,
    activeResolution,
    activeState,
    activeTokens,
    activity,
    approvalFocus,
    attachments,
    cancelIntent,
    cancelReason,
    confirmNewMain,
    contentRef,
    controlWorker,
    controllingIntent,
    createMainSession,
    creatingMain,
    deleteMode,
    fileInputRef,
    focusHistory,
    grouped,
    hasPendingForSession,
    hiddenUnread,
    input,
    intentsHasMore,
    isMain,
    latestTodoSeq,
    listOpen,
    loadOlderIntents,
    loadSession,
    loadingOlderIntents,
    mainBusy,
    mainLoaded,
    pickFiles,
    runDuration,
    send,
    sendMode,
    sendWorkerChat,
    sending,
    sessionMeta,
    setActiveId,
    setAttachments,
    setCancelIntent,
    setCancelReason,
    setConfirmNewMain,
    setDeleteMode,
    setInput,
    setListOpen,
    setWorkerMessage,
    setWorkerMessageRequestId,
    showLoader,
    side,
    sseLive,
    stop,
    stopping,
    store,
    taskTokens,
    tokenTotal,
    uploading,
    workerMessage,
    workerMessageSending,
    workerSessions,
  } = useTaskSessions(taskId);
  return (
    <TooltipProvider delayDuration={300}>
      {/* 高さの余白：ページ先頭（タイトル行 + ゴール + Tabs ≈ 7.5rem）+ 内容のパディング。モバイルは p-4、
        デスクトップは lg:p-6、さらにデスクトップはスクロール余裕も要るので、2 段階で 10rem / 13rem を確保する。モバイルで
        13rem を流用すると 3rem の記録領域を無駄に食う。 */}
      <div
        className={cn(
          "grid h-[calc(100svh-10rem)] min-h-0 grid-cols-1 gap-4",
          listOpen ? "grid-rows-[minmax(0,40svh)_minmax(0,1fr)]" : "grid-rows-[auto_minmax(0,1fr)]",
          "lg:h-[calc(100svh-13rem)] lg:grid-cols-[18rem_1fr] lg:grid-rows-[minmax(0,1fr)]",
        )}
      >
        {/* Left: session list */}
        <SessionSidebar
          activeDisplayTitle={activeDisplayTitle}
          activeId={activeId}
          approvalFocus={approvalFocus}
          controllingIntent={controllingIntent}
          creatingMain={creatingMain}
          grouped={grouped}
          hasPendingForSession={hasPendingForSession}
          hiddenUnread={hiddenUnread}
          intentsHasMore={intentsHasMore}
          listOpen={listOpen}
          loadOlderIntents={loadOlderIntents}
          loadingOlderIntents={loadingOlderIntents}
          mainLoaded={mainLoaded}
          sessionMeta={sessionMeta}
          setActiveId={setActiveId}
          setCancelIntent={setCancelIntent}
          setConfirmNewMain={setConfirmNewMain}
          setListOpen={setListOpen}
          setWorkerMessage={setWorkerMessage}
          setWorkerMessageRequestId={setWorkerMessageRequestId}
          sseLive={sseLive}
          store={store}
          taskTokens={taskTokens}
          workerSessions={workerSessions}
        />

        {/* Right: transcript */}
        <SideQuestionWorkspace
          side={side}
          label={active.role === "worker" ? `Worker #${active.intent_id} · ${activeDisplayTitle}` : activeDisplayTitle}
        >
          <div className="flex min-h-0 flex-1 min-w-0 flex-col overflow-hidden rounded-lg border bg-card">
            <SessionHeader
              active={active}
              activeAssets={activeAssets}
              activeResolution={activeResolution}
              activeState={activeState}
              activeTokens={activeTokens}
              isMain={isMain}
              runDuration={runDuration}
              sessionMeta={sessionMeta}
              side={side}
              tokenTotal={tokenTotal}
            />
            {(() => {
              const dm = active.role === "worker" ? sessionMeta.get(active.id) : undefined;
              if (!dm?.deleted) return null;
              return (
                <div className="flex items-start gap-2 border-b border-destructive/30 bg-destructive/5 px-4 py-2.5 text-xs">
                  <Trash2Icon className="mt-0.5 size-3.5 shrink-0 text-destructive" />
                  <div className="min-w-0">
                    <span className="font-medium text-destructive">この意図はユーザーにより削除されました</span>
                    <span className="text-muted-foreground">
                      （実行は停止済み、プランナーへ通知済み。意図と成果は保持され、下で履歴を確認できます）
                    </span>
                    {dm.deleteReason && (
                      <p className="mt-1 break-words text-foreground">
                        <span className="text-muted-foreground">削除理由：</span>
                        {dm.deleteReason}
                      </p>
                    )}
                  </div>
                </div>
              );
            })()}
            <ApprovalExecutionFocus
              focus={{
                ...approvalFocus,
                close: () => {
                  setActiveId(activeId);
                  approvalFocus.close();
                },
              }}
              history={focusHistory}
            />
            {/* Force Radix's internal viewport wrapper (display:table, sizes to content)
            to block so wide/unbreakable steps (long commands, code, URLs) can't blow
            out the width and defeat the truncation below — the transcript wraps to
            the panel instead of overflowing horizontally. */}
            <SessionTranscriptPane
              active={active}
              activeKey={activeKey}
              activeState={activeState}
              activity={activity}
              approvalFocus={approvalFocus}
              contentRef={contentRef}
              focusHistory={focusHistory}
              isMain={isMain}
              loadSession={loadSession}
              showLoader={showLoader}
              taskId={taskId}
            />
            {
              <SessionComposer
                active={active}
                attachments={attachments}
                controlWorker={controlWorker}
                controllingIntent={controllingIntent}
                fileInputRef={fileInputRef}
                input={input}
                isMain={isMain}
                latestTodoSeq={latestTodoSeq}
                mainBusy={mainBusy}
                pickFiles={pickFiles}
                send={send}
                sendMode={sendMode}
                sendWorkerChat={sendWorkerChat}
                sending={sending}
                setAttachments={setAttachments}
                setInput={setInput}
                setWorkerMessage={setWorkerMessage}
                setWorkerMessageRequestId={setWorkerMessageRequestId}
                stop={stop}
                stopping={stopping}
                taskId={taskId}
                uploading={uploading}
                workerMessage={workerMessage}
                workerMessageSending={workerMessageSending}
              />
            }
          </div>
        </SideQuestionWorkspace>
        <WorkerSessionDeleteDialog
          cancelIntent={cancelIntent}
          cancelReason={cancelReason}
          controlWorker={controlWorker}
          controllingIntent={controllingIntent}
          deleteMode={deleteMode}
          setCancelIntent={setCancelIntent}
          setCancelReason={setCancelReason}
          setDeleteMode={setDeleteMode}
        />

        <NewMainSessionDialog
          confirmNewMain={confirmNewMain}
          createMainSession={createMainSession}
          creatingMain={creatingMain}
          setConfirmNewMain={setConfirmNewMain}
        />
      </div>
    </TooltipProvider>
  );
}
