"use client";
import { ArrowUpIcon, Loader2Icon, PaperclipIcon, SquareIcon, XIcon } from "lucide-react";

import { MentionTextarea } from "@/components/mention-textarea";
import { TodoPopover } from "@/components/todo-popover";
import { InputGroup, InputGroupAddon, InputGroupButton } from "@/components/ui/input-group";
import { Spinner } from "@/components/ui/spinner";
import { api } from "@/lib/api";
import { shouldSubmitOnKey } from "@/lib/chat-send-mode";
import { fmtBytes } from "@/lib/format";
import { isBtwCommand } from "@/lib/side-questions";
import { cn } from "@/lib/utils";

import type { TaskSessionsController } from "../_hooks/use-task-sessions";
import { MAX_WORKER_MESSAGE_CHARS, workerMessageCharCount } from "../_lib/session-model";

type Props = Pick<
  TaskSessionsController,
  | "active"
  | "attachments"
  | "controlWorker"
  | "controllingIntent"
  | "fileInputRef"
  | "input"
  | "isMain"
  | "latestTodoSeq"
  | "mainBusy"
  | "pickFiles"
  | "send"
  | "sendMode"
  | "sendWorkerChat"
  | "sending"
  | "setAttachments"
  | "setInput"
  | "setWorkerMessage"
  | "setWorkerMessageRequestId"
  | "stop"
  | "stopping"
  | "taskId"
  | "uploading"
  | "workerMessage"
  | "workerMessageSending"
>;

export function SessionComposer({
  active,
  attachments,
  controlWorker,
  controllingIntent,
  fileInputRef,
  input,
  isMain,
  latestTodoSeq,
  mainBusy,
  pickFiles,
  send,
  sendMode,
  sendWorkerChat,
  sending,
  setAttachments,
  setInput,
  setWorkerMessage,
  setWorkerMessageRequestId,
  stop,
  stopping,
  taskId,
  uploading,
  workerMessage,
  workerMessageSending,
}: Props) {
  return (
    <>
      {" "}
      {isMain ? (
        <div className="border-t p-3">
          {attachments.length > 0 && (
            <div className="mb-2 flex flex-wrap gap-1.5">
              {attachments.map((a) => (
                <div
                  key={a.path}
                  className="flex items-center gap-1.5 rounded-md border bg-muted/50 px-2 py-1 text-xs"
                  title={a.path}
                >
                  <PaperclipIcon className="size-3 shrink-0 text-primary" />
                  <span className="max-w-[160px] truncate">{a.name}</span>
                  <span className="text-muted-foreground">{fmtBytes(a.size)}</span>
                  <button
                    type="button"
                    className="ml-0.5 text-muted-foreground hover:text-foreground"
                    onClick={() => setAttachments((p) => p.filter((x) => x.path !== a.path))}
                    title="外す"
                  >
                    <XIcon className="size-3" />
                  </button>
                </div>
              ))}
            </div>
          )}
          <input
            ref={fileInputRef}
            type="file"
            multiple
            className="hidden"
            onChange={(e) => void pickFiles(e.target.files)}
          />
          <InputGroup className="min-h-9 has-disabled:opacity-100">
            <MentionTextarea
              inputGroup
              rows={1}
              aria-label="メイン Agent にメッセージを送信"
              placeholder={
                mainBusy
                  ? "メイン Agent が実行中です。/btw で質問できます…"
                  : "メイン Agent にメッセージ、@ で脆弱性やアセットを引用…"
              }
              value={input}
              disabled={sending}
              onValueChange={setInput}
              onKeyDown={(e) => {
                if (!shouldSubmitOnKey(e, sendMode)) return;
                e.preventDefault();
                send();
              }}
              className="max-h-36 min-h-9 overflow-y-auto"
            />
            <InputGroupAddon align="block-end">
              <InputGroupButton
                size="icon-xs"
                variant="ghost"
                onClick={() => fileInputRef.current?.click()}
                disabled={mainBusy || uploading}
                title="ファイルをアップロード"
                aria-label="ファイルをアップロード"
              >
                {uploading ? <Loader2Icon className="animate-spin" /> : <PaperclipIcon />}
              </InputGroupButton>
              {mainBusy && isBtwCommand(input) && (
                <InputGroupButton size="icon-xs" onClick={send} aria-label="サイドクエスチョンを送信">
                  <ArrowUpIcon />
                </InputGroupButton>
              )}
              {mainBusy ? (
                <InputGroupButton
                  className="ml-auto"
                  size="icon-xs"
                  variant="destructive"
                  onClick={stop}
                  disabled={stopping}
                  title="現在の実行を停止"
                  aria-label="現在の実行を停止"
                >
                  {stopping ? <Loader2Icon className="animate-spin" /> : <SquareIcon />}
                </InputGroupButton>
              ) : (
                <InputGroupButton
                  className="ml-auto"
                  size="icon-xs"
                  variant="default"
                  onClick={send}
                  disabled={(!input.trim() && attachments.length === 0) || sending}
                  title="メッセージを送信"
                  aria-label="メッセージを送信"
                >
                  {sending ? <Loader2Icon className="animate-spin" /> : <ArrowUpIcon />}
                </InputGroupButton>
              )}
            </InputGroupAddon>
          </InputGroup>
        </div>
      ) : active.role === "worker" &&
        !active.inherited &&
        (active.status === "running" || active.status === "paused") ? (
        <div className="border-t p-3">
          <InputGroup className="min-h-9 has-disabled:opacity-100">
            <MentionTextarea
              inputGroup
              rows={1}
              aria-label={`Worker #${active.intent_id} にメッセージを送信`}
              placeholder={`Worker #${active.intent_id} にメッセージ、@ でレコードを引用し実行方向を調整…`}
              value={workerMessage}
              onValueChange={(value) => {
                setWorkerMessage(value);
                setWorkerMessageRequestId("");
              }}
              onKeyDown={(event) => {
                if (!shouldSubmitOnKey(event, sendMode)) return;
                event.preventDefault();
                sendWorkerChat();
              }}
              disabled={workerMessageSending}
              aria-invalid={workerMessageCharCount(workerMessage) > MAX_WORKER_MESSAGE_CHARS}
              className="max-h-36 min-h-9 overflow-y-auto"
            />
            <InputGroupAddon align="block-end">
              <span
                className={cn(
                  "px-1 text-[10px] tabular-nums text-muted-foreground",
                  workerMessageCharCount(workerMessage) > MAX_WORKER_MESSAGE_CHARS && "text-destructive",
                )}
              >
                {workerMessageCharCount(workerMessage)}/{MAX_WORKER_MESSAGE_CHARS}
              </span>
              {active.status === "running" && isBtwCommand(workerMessage) && (
                <InputGroupButton size="icon-xs" onClick={sendWorkerChat} aria-label="サイドクエスチョンを送信">
                  <ArrowUpIcon />
                </InputGroupButton>
              )}
              {active.status === "running" ? (
                <InputGroupButton
                  className="ml-auto"
                  size="icon-xs"
                  variant="destructive"
                  onClick={() => void controlWorker(active, "pause")}
                  disabled={controllingIntent === active.intent_id}
                  title="現在の Worker を一時停止"
                  aria-label="現在の Worker を一時停止"
                >
                  {controllingIntent === active.intent_id ? <Loader2Icon className="animate-spin" /> : <SquareIcon />}
                </InputGroupButton>
              ) : (
                <>
                  <InputGroupButton
                    className="ml-auto"
                    size="xs"
                    variant="ghost"
                    onClick={() => void controlWorker(active, "resume")}
                    disabled={controllingIntent === active.intent_id || workerMessageSending}
                    title="メッセージを送らずに実行を継続"
                    aria-label="実行を継続"
                  >
                    {controllingIntent === active.intent_id ? <Loader2Icon className="animate-spin" /> : "そのまま継続"}
                  </InputGroupButton>
                  <InputGroupButton
                    size="icon-xs"
                    variant="default"
                    onClick={sendWorkerChat}
                    disabled={
                      workerMessageSending ||
                      !workerMessage.trim() ||
                      workerMessageCharCount(workerMessage) > MAX_WORKER_MESSAGE_CHARS
                    }
                    title="メッセージを送信"
                    aria-label="メッセージを送信"
                  >
                    {workerMessageSending ? <Spinner /> : <ArrowUpIcon />}
                  </InputGroupButton>
                </>
              )}
            </InputGroupAddon>
          </InputGroup>
        </div>
      ) : (
        <div className="flex items-center border-t px-4 py-2">
          <TodoPopover
            seq={latestTodoSeq}
            fetchDetail={(seq) => api.activityDetail(seq, taskId).then((r) => r.detail ?? "")}
          />
        </div>
      )}{" "}
    </>
  );
}
