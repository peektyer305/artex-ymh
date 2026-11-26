"use client";
import * as React from "react";

import type {
  IntentAsset,
  InterceptApprovalRow,
  Session,
  SessionTokenUsage,
  TaskLLMResolutions,
  TaskNode,
  TokenTotal,
} from "@/lib/types";

import { MAIN_ID, mainSessionKey, type SessionStore } from "../_lib/session-model";
import type { useSessionAttachments } from "./use-session-attachments";

export function useTaskSessionState() {
  const [selectedSessionId, setActiveId] = React.useState(MAIN_ID);

  // Keep a located archived/older session selectable after leaving focus mode.
  const [locatedSession, setLocatedSession] = React.useState<{ taskId: string; session: Session }>();

  // Main-agent conversation segments (newest-first); currentSeg is the writable one.
  const [mainSegs, setMainSegs] = React.useState<{ seq: number; created_at: string }[]>([{ seq: 0, created_at: "" }]);

  const [currentSeg, setCurrentSeg] = React.useState(0);

  const [creatingMain, setCreatingMain] = React.useState(false);

  const [confirmNewMain, setConfirmNewMain] = React.useState(false);

  // モバイル（<lg）ではセッション一覧を既定で折りたたむ：画面の高さが厳しく、一覧が 10~15rem を
  // 常に占めると、下のセッション記録がタイトルと入力欄だけに圧迫される。折りたたむと記録領域がほぼ全高を
  // 得る。タイトルバーをタップすると展開してセッションを選べ、選ぶと自動で折りたたむ。デスクトップは影響なし（lg 以上は常に展開）。
  const [listOpen, setListOpen] = React.useState(false);

  // Per-session lazily-loaded caches, keyed by session_key (main | plan | intent:<id>).
  const [store, setStore] = React.useState<SessionStore>({});

  // Worker sessions derived from exploration intents (paged past the old 300 cap).
  const [intents, setIntents] = React.useState<TaskNode[]>([]);

  const [intentAssets, setIntentAssets] = React.useState<IntentAsset[]>([]);

  const [olderIntents, setOlderIntents] = React.useState<TaskNode[]>([]);

  const [firstIntentsHasMore, setFirstIntentsHasMore] = React.useState(false);

  const [olderIntentsHasMore, setOlderIntentsHasMore] = React.useState(false);

  const [hasLoadedOlderIntentsPage, setHasLoadedOlderIntentsPage] = React.useState(false);

  const [loadingOlderIntents, setLoadingOlderIntents] = React.useState(false);

  const [input, setInput] = React.useState("");

  const [sending, setSending] = React.useState(false);

  const [stopping, setStopping] = React.useState(false);

  const [mainChatRunning, setMainChatRunning] = React.useState<boolean | null>(null);

  const [controllingIntent, setControllingIntent] = React.useState<string | null>(null);

  const [cancelIntent, setCancelIntent] = React.useState<Session | null>(null);

  const [cancelReason, setCancelReason] = React.useState("");

  // 削除モード：soft=論理削除（既定、deleted に設定 + 理由を記録、データは保持）| hard=物理削除（独占の子孫を連鎖削除）。
  const [deleteMode, setDeleteMode] = React.useState<"soft" | "hard">("soft");

  const [workerMessage, setWorkerMessage] = React.useState("");

  const [workerMessageRequestId, setWorkerMessageRequestId] = React.useState("");

  const [workerMessageSending, setWorkerMessageSending] = React.useState(false);

  // SSE connection state — surfaced so a dropped realtime link is visible, never
  // silently shown as "no messages".
  const [sseLive, setSseLive] = React.useState(false);

  // Whole-task token total (all agents), polled from the backend aggregate.
  const [taskTokens, setTaskTokens] = React.useState<TokenTotal | null>(null);

  const [sessionTokens, setSessionTokens] = React.useState<Record<string, SessionTokenUsage>>({});

  const [llmResolutions, setLLMResolutions] = React.useState<TaskLLMResolutions | null>(null);

  // Pending intercept requests for this task — used to show warning icons on sessions.
  const [pendingIntercepts, setPendingIntercepts] = React.useState<InterceptApprovalRow[]>([]);

  // Refs backing SSE/loading without re-render churn.
  const snapshotRef = React.useRef(0);

  // task-level snapshot cursor → SSE since=
  const esRef = React.useRef<EventSource | null>(null);

  const activeKeyRef = React.useRef(mainSessionKey(0));

  // current session key (for SSE dispatch/unread)
  const atBottomRef = React.useRef(true);

  // transcript pinned to bottom?
  const llmToastSeqRef = React.useRef<Set<number>>(new Set());

  const chatStatusRequestRef = React.useRef(0);

  const firstIntentsRef = React.useRef<TaskNode[]>([]);

  // Per-key request token: a stale response for a key is ignored (guards fast
  // latest/older interleaving). Writes are ALWAYS keyed, so a late response can only
  // touch its own session cache — never the currently-viewed one (see §7.5).
  const reqTokenRef = React.useRef<Record<string, number>>({});

  // Keys with a latest-page load in flight — dedups the double trigger where the
  // first-load effect and the active-session effect both want "main" on mount (the
  // former also opens the SSE, so it must not be pre-empted).
  const loadingKeysRef = React.useRef<Set<string>>(new Set());
  return {
    selectedSessionId,
    setActiveId,
    locatedSession,
    setLocatedSession,
    mainSegs,
    setMainSegs,
    currentSeg,
    setCurrentSeg,
    creatingMain,
    setCreatingMain,
    confirmNewMain,
    setConfirmNewMain,
    listOpen,
    setListOpen,
    store,
    setStore,
    intents,
    setIntents,
    intentAssets,
    setIntentAssets,
    olderIntents,
    setOlderIntents,
    firstIntentsHasMore,
    setFirstIntentsHasMore,
    olderIntentsHasMore,
    setOlderIntentsHasMore,
    hasLoadedOlderIntentsPage,
    setHasLoadedOlderIntentsPage,
    loadingOlderIntents,
    setLoadingOlderIntents,
    input,
    setInput,
    sending,
    setSending,
    stopping,
    setStopping,
    mainChatRunning,
    setMainChatRunning,
    controllingIntent,
    setControllingIntent,
    cancelIntent,
    setCancelIntent,
    cancelReason,
    setCancelReason,
    deleteMode,
    setDeleteMode,
    workerMessage,
    setWorkerMessage,
    workerMessageRequestId,
    setWorkerMessageRequestId,
    workerMessageSending,
    setWorkerMessageSending,
    sseLive,
    setSseLive,
    taskTokens,
    setTaskTokens,
    sessionTokens,
    setSessionTokens,
    llmResolutions,
    setLLMResolutions,
    pendingIntercepts,
    setPendingIntercepts,
    snapshotRef,
    esRef,
    activeKeyRef,
    atBottomRef,
    llmToastSeqRef,
    chatStatusRequestRef,
    firstIntentsRef,
    reqTokenRef,
    loadingKeysRef,
  };
}

export type TaskSessionState = ReturnType<typeof useTaskSessionState> & ReturnType<typeof useSessionAttachments>;
