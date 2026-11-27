"use client";
import { toast } from "sonner";

import type { useSideQuestions } from "@/hooks/use-side-questions";
import { api } from "@/lib/api";
import type { Session } from "@/lib/types";

import { MAX_WORKER_MESSAGE_CHARS, newWorkerMessageRequestID, workerMessageCharCount } from "../_lib/session-model";
import type { useSessionActions } from "./use-session-actions";
import type { TaskSessionState } from "./use-task-session-state";

type Context = Pick<
  TaskSessionState,
  | "attachments"
  | "chatStatusRequestRef"
  | "input"
  | "sending"
  | "setAttachments"
  | "setInput"
  | "setMainChatRunning"
  | "setSending"
  | "setStopping"
  | "setWorkerMessage"
  | "setWorkerMessageRequestId"
  | "setWorkerMessageSending"
  | "stopping"
  | "workerMessage"
  | "workerMessageRequestId"
  | "workerMessageSending"
> & {
  active: Session;
  mainBusy: boolean;
  patchIntentState: ReturnType<typeof useSessionActions>["patchIntentState"];
  side: ReturnType<typeof useSideQuestions>;
};

export function useSessionMessageActions(taskId: string, context: Context) {
  const {
    attachments,
    chatStatusRequestRef,
    input,
    sending,
    setAttachments,
    setInput,
    setMainChatRunning,
    setSending,
    setStopping,
    setWorkerMessage,
    setWorkerMessageRequestId,
    setWorkerMessageSending,
    stopping,
    workerMessage,
    workerMessageRequestId,
    workerMessageSending,
    active,
    mainBusy,
    patchIntentState,
    side,
  } = context;
  function stop() {
    if (stopping) return;
    setStopping(true);
    void api.stopChat(taskId).finally(() => setStopping(false));
  }

  function send() {
    const text = input.trim();
    const atts = attachments;
    if (side.handleCommand(text, () => setInput(""))) return;
    if ((!text && atts.length === 0) || sending || mainBusy) return;
    // No optimistic echo: the backend persists+broadcasts the human turn before it
    // returns, so it streams back over SSE (worker="mainagent") with its real DB
    // seq — the transcript renders it from server data like every other step. Clear
    // the composer eagerly for responsiveness; restore it if the send fails.
    setInput("");
    setAttachments([]);
    setSending(true);
    chatStatusRequestRef.current++;
    api
      .chat(text, taskId, atts.length > 0 ? atts : undefined, active.seg ?? 0)
      .then(({ mode }) => {
        chatStatusRequestRef.current++;
        setMainChatRunning(mode === "llm");
      })
      .catch((e) => {
        setInput(text); // restore so the user doesn't lose their text / attachments
        setAttachments(atts);
        toast.error(`送信に失敗しました：${(e as Error).message || "しばらくして再試行してください"}`);
      })
      .finally(() => setSending(false));
  }

  function sendWorkerChat() {
    const intentId = active.intent_id;
    const message = workerMessage.trim();
    if (side.handleCommand(message, () => setWorkerMessage(""))) return;
    if (!intentId || active.inherited || active.status !== "paused" || workerMessageSending || !message) return;
    if (workerMessageCharCount(message) > MAX_WORKER_MESSAGE_CHARS) {
      toast.error(`メッセージは ${MAX_WORKER_MESSAGE_CHARS} 文字を超えられません`);
      return;
    }
    const requestId = workerMessageRequestId || newWorkerMessageRequestID();
    if (!workerMessageRequestId) setWorkerMessageRequestId(requestId);
    setWorkerMessageSending(true);
    api
      .sendWorkerMessage(taskId, intentId, message, requestId)
      .then((result) => {
        // The server records the user turn and streams the run over SSE, so there is
        // no optimistic insert: the message and the continuation arrive live.
        patchIntentState(intentId, result.state);
        setWorkerMessage("");
        setWorkerMessageRequestId("");
        toast.success(`ワーカー #${intentId} にメッセージを送信し、即座に実行を再開しました`);
      })
      .catch((error) => {
        toast.error(`送信に失敗しました：${(error as Error).message || "しばらくして再試行してください"}`);
      })
      .finally(() => setWorkerMessageSending(false));
  }
  return { stop, send, sendWorkerChat };
}
