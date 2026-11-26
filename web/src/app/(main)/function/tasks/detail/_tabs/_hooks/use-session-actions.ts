"use client";
import * as React from "react";

import { toast } from "sonner";

import { api } from "@/lib/api";
import type { Session, TaskNode } from "@/lib/types";

import { emptyState, mainSessionId, mainSessionKey } from "../_lib/session-model";
import type { TaskSessionState } from "./use-task-session-state";

type Context = Pick<
  TaskSessionState,
  | "controllingIntent"
  | "creatingMain"
  | "setActiveId"
  | "setCancelIntent"
  | "setCancelReason"
  | "setConfirmNewMain"
  | "setControllingIntent"
  | "setCreatingMain"
  | "setCurrentSeg"
  | "setInput"
  | "setIntents"
  | "setListOpen"
  | "setMainSegs"
  | "setOlderIntents"
  | "setStore"
>;

export function useSessionActions(taskId: string, context: Context) {
  const {
    controllingIntent,
    creatingMain,
    setActiveId,
    setCancelIntent,
    setCancelReason,
    setConfirmNewMain,
    setControllingIntent,
    setCreatingMain,
    setCurrentSeg,
    setInput,
    setIntents,
    setListOpen,
    setMainSegs,
    setOlderIntents,
    setStore,
  } = context;
  // Start a fresh main-agent session: only the segment counter advances — the task's
  // graph/assets/goal are untouched, so the agent continues over the same task with a
  // clean context. The old segment stays as read-only history you can switch back to.
  async function createMainSession() {
    if (creatingMain) return;
    setCreatingMain(true);
    try {
      const r = await api.newMainSession(taskId);
      setMainSegs((prev) => [{ seq: r.seq, created_at: r.created_at }, ...prev.filter((m) => m.seq !== r.seq)]);
      setCurrentSeg(r.current ?? r.seq);
      // Seed an empty, loaded state so the new (empty) session renders immediately.
      setStore((prev) => ({ ...prev, [mainSessionKey(r.seq)]: { ...emptyState(), loaded: true } }));
      setActiveId(mainSessionId(r.seq));
      setListOpen(false);
      setInput("");
    } catch (e) {
      toast.error(`セッションの作成に失敗しました：${(e as Error).message}`);
    } finally {
      setCreatingMain(false);
      setConfirmNewMain(false);
    }
  }

  const patchIntentState = React.useCallback(
    (intentId: string, state?: string) => {
      const patch = (rows: TaskNode[]) =>
        state
          ? rows.map((row) => (row.id === intentId ? { ...row, state } : row))
          : rows.filter((row) => row.id !== intentId);
      setIntents(patch);
      setOlderIntents(patch);
    },
    [setIntents, setOlderIntents],
  );

  const controlWorker = React.useCallback(
    async (session: Session, action: "pause" | "resume" | "cancel", reason?: string, mode?: "soft" | "hard") => {
      if (!session.intent_id || session.inherited || controllingIntent) return;
      if (action === "cancel" && !reason?.trim()) {
        toast.error("削除理由を入力してください");
        return;
      }
      setControllingIntent(session.intent_id);
      try {
        const res = await api.controlIntent(taskId, session.intent_id, action, reason, mode);
        if (action === "pause") {
          patchIntentState(session.intent_id, "paused");
          toast.success(`Worker #${session.intent_id} を一時停止しました`);
        } else if (action === "resume") {
          patchIntentState(session.intent_id, "open");
          toast.success(`Worker #${session.intent_id} を再開しました。再取得を待機中`);
        } else if (mode === "hard") {
          // 物理削除:意図と独占の下流を物理的に削除し、一覧からその行を除く。
          patchIntentState(session.intent_id);
          const d = res.deleted;
          const extra = d ? `（意図 ${d.intents} / 事実 ${d.facts} / 脆弱性 ${d.findings} を含む）` : "";
          toast.success(`Worker #${session.intent_id} とその独占下流を完全に削除しました${extra}`);
          setCancelReason("");
        } else {
          // 論理削除:意図を deleted にし、削除理由を記録、ノードと成果は保持。
          patchIntentState(session.intent_id, "deleted");
          toast.success(
            `Worker #${session.intent_id} を削除しました（理由を記録済み、プランナーがこれに基づき再計画します）`,
          );
          setCancelReason("");
        }
      } catch (error) {
        toast.error(`Worker の操作に失敗しました：${(error as Error).message}`);
      } finally {
        setControllingIntent(null);
        setCancelIntent(null);
      }
    },
    [controllingIntent, patchIntentState, taskId, setControllingIntent, setCancelReason, setCancelIntent],
  );
  return { createMainSession, patchIntentState, controlWorker };
}
