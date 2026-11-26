"use client";
import * as React from "react";

import { useApprovalFocus, useApprovalHistory } from "@/components/approval-execution-focus";
import { useSideQuestions } from "@/hooks/use-side-questions";
import { api } from "@/lib/api";
import { useChatSendMode } from "@/lib/chat-send-mode";
import type { Activity, IntentAsset, Session, TaskLLMResolution, TaskNode, TokenTotal } from "@/lib/types";

import {
  intentToSession,
  keyForSession,
  MAIN_SESSION,
  mainSessionId,
  mainSessionKey,
  mainSessionTitle,
  mergeBySeq,
  PAGE,
  PLANNER_SESSION,
  STREAM_WINDOW_MS,
  SYSTEM_SESSION,
} from "../_lib/session-model";
import { useSessionActions } from "./use-session-actions";
import { useSessionAttachments } from "./use-session-attachments";
import { useSessionHistory } from "./use-session-history";
import { useSessionMessageActions } from "./use-session-message-actions";
import { useSessionRuntimePolling } from "./use-session-runtime-polling";
import { useSessionScroll } from "./use-session-scroll";
import { useTaskSessionState } from "./use-task-session-state";
import { useWorkerSessionPolling } from "./use-worker-session-polling";

export function useTaskSessions(taskId: string) {
  const state = { ...useTaskSessionState(), ...useSessionAttachments(taskId) };
  const {
    selectedSessionId,
    setActiveId,
    locatedSession,
    setLocatedSession,
    mainSegs,
    currentSeg,
    creatingMain,
    confirmNewMain,
    setConfirmNewMain,
    listOpen,
    setListOpen,
    store,
    intents,
    intentAssets,
    olderIntents,
    firstIntentsHasMore,
    olderIntentsHasMore,
    hasLoadedOlderIntentsPage,
    loadingOlderIntents,
    input,
    setInput,
    sending,
    stopping,
    mainChatRunning,
    controllingIntent,
    cancelIntent,
    setCancelIntent,
    cancelReason,
    setCancelReason,
    deleteMode,
    setDeleteMode,
    workerMessage,
    setWorkerMessage,
    setWorkerMessageRequestId,
    workerMessageSending,
    sseLive,
    taskTokens,
    sessionTokens,
    llmResolutions,
    pendingIntercepts,
    activeKeyRef,
    atBottomRef,
    attachments,
    setAttachments,
    uploading,
    fileInputRef,
    pickFiles,
  } = state;
  useSessionRuntimePolling(taskId, state);
  const { patchStore, loadSession, loadEarlier } = useSessionHistory(taskId, state);
  const { loadOlderIntents } = useWorkerSessionPolling(taskId, state);
  const { createMainSession, patchIntentState, controlWorker } = useSessionActions(taskId, state);
  const approvalFocus = useApprovalFocus({ taskId });

  const focusSession = React.useMemo<Session | undefined>(() => {
    const source = approvalFocus.state?.source;
    if (!source) return undefined;
    if (source.session.startsWith("main:")) {
      const seg = Number(source.session.slice(5));
      return { ...MAIN_SESSION, id: mainSessionId(seg), seg, title: mainSessionTitle(seg), live: false };
    }
    if (source.session === "plan") return { ...PLANNER_SESSION, live: false };
    if (source.session.startsWith("intent:")) {
      const id = source.session.slice(7);
      return {
        id,
        role: "worker",
        intent_id: id,
        title: `Worker #${id}`,
        status: "done",
        live: false,
        last_activity: source.items[0]?.ts ?? "",
      };
    }
    return undefined;
  }, [approvalFocus.state?.source]);

  React.useEffect(() => {
    if (focusSession) setLocatedSession({ taskId, session: focusSession });
  }, [taskId, focusSession, setLocatedSession]);

  const retainedSession = locatedSession?.taskId === taskId ? locatedSession.session : undefined;

  const activeId = focusSession?.id ?? selectedSessionId;

  // Combined, de-duplicated worker list (newest first page + older loaded pages).
  const allIntents = React.useMemo(() => {
    const byId = new Map<string, TaskNode>();
    for (const n of olderIntents) byId.set(n.id, n);
    for (const n of intents) byId.set(n.id, n); // fresh poll wins over older snapshot
    return [...byId.values()].sort((a, b) => Number(b.id) - Number(a.id));
  }, [intents, olderIntents]);

  const intentAssetsByID = React.useMemo(() => {
    const grouped = new Map<string, IntentAsset[]>();
    for (const asset of intentAssets) {
      const key = String(asset.intent_id);
      const current = grouped.get(key);
      if (current) current.push(asset);
      else grouped.set(key, [asset]);
    }
    return grouped;
  }, [intentAssets]);

  const workerSessions = React.useMemo(() => allIntents.map(intentToSession), [allIntents]);

  const intentsHasMore = hasLoadedOlderIntentsPage ? olderIntentsHasMore : firstIntentsHasMore;

  // For each worker session (intent), derive the display title from the intent
  // payload summary. Also store the full TaskNode for the hover-JSON tooltip.
  const sessionMeta = React.useMemo(() => {
    const map = new Map<string, { title: string; json: unknown; deleted: boolean; deleteReason: string }>();
    for (const node of allIntents) {
      let title = `Intent ${node.id}`;
      let parsedPayload: unknown = node.payload;
      // 論理削除:意図の state='deleted'、削除理由は独立フィールド delete_reason に置く。
      const deleted = node.state === "deleted";
      const deleteReason = node.delete_reason ?? "";
      if (node.payload) {
        try {
          const p = JSON.parse(node.payload);
          parsedPayload = p;
          if (p?.summary) title = String(p.summary);
        } catch {
          title = node.payload.trim() || title;
        }
      }
      const json = { ...node, payload: parsedPayload };
      map.set(node.id, { title, json, deleted, deleteReason });
    }
    return map;
  }, [allIntents]);

  // Returns true if any pending intercept belongs to this session.
  // Worker agent_name format: "work#N · #intentID". Main/planner match by role key.
  const hasPendingForSession = React.useCallback(
    (s: Session): boolean => {
      if (s.inherited) return false;
      if (!pendingIntercepts.length) return false;
      if (s.role === "mainagent") return pendingIntercepts.some((r) => r.agent_name === "mainagent");
      if (s.role === "planner") return pendingIntercepts.some((r) => r.agent_name === "planner");
      // Worker: extract intent ID from "work#N · #<intentID>"
      return pendingIntercepts.some((r) => {
        const m = r.agent_name.match(/·\s*#(\d+)$/);
        return m ? m[1] === s.intent_id : false;
      });
    },
    [pendingIntercepts],
  );

  // Liveness from each session's last-seen activity time (kept fresh by the 1.5s tick).
  const recentLive = React.useCallback(
    (key: string) => {
      const ts = store[key]?.lastTs;
      if (!ts) return false;
      const t = Date.parse(ts);
      return t > 0 && Date.now() - t < STREAM_WINDOW_MS;
    },
    [store],
  );

  const currentMainKey = mainSessionKey(currentSeg);

  const plannerLive = recentLive("plan");

  // Which main segment is streaming RIGHT NOW. A main turn is serialized per task, so at
  // most one segment is live. Gate on the real running flag (sending / mainChatRunning)
  // rather than "recent activity", and drop it the moment the turn's terminal record
  // (kind='result', or an error) lands — otherwise the badge lingers for STREAM_WINDOW_MS
  // after the agent already finished. null = nothing running.
  const liveMainSeg = React.useMemo<number | null>(() => {
    if (!(sending || mainChatRunning)) return null;
    // the streaming segment is the one with the freshest activity (incl. the just-sent turn)
    let seg = currentSeg;
    let bestTs = -1;
    for (const m of mainSegs) {
      const raw = store[mainSessionKey(m.seq)]?.lastTs;
      const ts = raw ? Date.parse(raw) : -1;
      if (ts > bestTs) {
        bestTs = ts;
        seg = m.seq;
      }
    }
    const items = store[mainSessionKey(seg)]?.items ?? [];
    const last = items[items.length - 1];
    if (last && (last.kind === "result" || (last.kind === "text" && last.is_error))) return null;
    return seg;
  }, [sending, mainChatRunning, mainSegs, store, currentSeg]);

  // Every main-agent segment is an independent, interactive session (like the top-level
  // chat conversations) — you can talk in any of them, newest-first.
  const mainSessions = React.useMemo<Session[]>(
    () =>
      mainSegs.map((m) => ({
        id: mainSessionId(m.seq),
        role: "mainagent",
        title: mainSessionTitle(m.seq),
        status: "running",
        live: m.seq === liveMainSeg,
        last_activity: m.created_at,
        seg: m.seq,
      })),
    [mainSegs, liveMainSeg],
  );

  const sessions = React.useMemo(() => {
    const items = [...mainSessions, { ...PLANNER_SESSION, live: plannerLive }, ...workerSessions, SYSTEM_SESSION];
    const located = focusSession ?? retainedSession;
    if (located && !items.some((s) => s.id === located.id)) items.push(located);
    return items;
  }, [mainSessions, workerSessions, plannerLive, focusSession, retainedSession]);

  const grouped = {
    mainagent: sessions.filter((s) => s.role === "mainagent"),
    planner: sessions.filter((s) => s.role === "planner"),
    worker: sessions.filter((s) => s.role === "worker"),
    system: sessions.filter((s) => s.role === "system"),
  };

  const active = sessions.find((s) => s.id === activeId) ?? MAIN_SESSION;

  const side = useSideQuestions(
    active.role === "mainagent"
      ? `/api/tasks/${taskId}/chat`
      : active.role === "worker" && !active.inherited && active.intent_id
        ? `/api/tasks/${taskId}/intents/${active.intent_id}`
        : null,
  );

  const isMain = active.role === "mainagent";

  const isPlanner = active.role === "planner";

  const isSystem = active.role === "system";

  const activeKey = keyForSession(active);

  const activeState = store[activeKey];

  // A main turn is serialized per task (chat lock), so "busy" is task-wide: while any
  // main segment is mid-turn the active composer is disabled. Clear it the moment the
  // active session's terminal record (kind='result', or an error) lands, so the input
  // re-enables immediately instead of waiting for the next chat-status poll.
  const activeItems = activeState?.items ?? [];

  const activeLast = activeItems[activeItems.length - 1];

  const activeSettled =
    !!activeLast && (activeLast.kind === "result" || (activeLast.kind === "text" && activeLast.is_error));

  const mainBusy = isMain && (sending || (!activeSettled && (mainChatRunning ?? recentLive(activeKey))));

  // 折りたたみ時（モバイル）はタイトルバーが一覧全体の代わりになる：現在のセッション名 + 他セッションの未読合計を表示し、
  // そうしないと折りたたんだ後、どのセッションを見ているかも、他に新着があるかも分からなくなる。
  const activeDisplayTitle = (active.role === "worker" ? sessionMeta.get(active.id)?.title : "") || active.title;

  const hiddenUnread = React.useMemo(
    () => Object.entries(store).reduce((sum, [key, s]) => (key === activeKey ? sum : sum + s.unread), 0),
    [store, activeKey],
  );

  // Keep the SSE dispatcher's notion of the active session current, and lazily load
  // + clear unread whenever the active session changes.
  // biome-ignore lint/correctness/useExhaustiveDependencies: cache updates must not reactivate the current session.
  React.useEffect(() => {
    activeKeyRef.current = activeKey;
    const st = store[activeKey];
    if (!st || (!st.loaded && !st.loading)) {
      loadSession(activeKey);
    } else if (st.unread) {
      patchStore(activeKey, (s) => ({ ...s, unread: 0 }));
    }
  }, [activeKey]);

  const focusKey = approvalFocus.state?.source?.session;

  const loadFocusPage = React.useCallback(
    (before: number) => api.activityHistory(taskId, focusKey ?? "main", before, PAGE),
    [taskId, focusKey],
  );

  const mergeFocusPage = React.useCallback(
    (page: { items: Activity[]; hasMore: boolean }) => {
      if (!focusKey) return;
      patchStore(focusKey, (s) => {
        const items = mergeBySeq(page.items, s.items);
        return { ...s, items, hasMore: page.hasMore, earliestSeq: items[0]?.seq ?? s.earliestSeq };
      });
    },
    [focusKey, patchStore],
  );

  const focusHistory = useApprovalHistory(
    approvalFocus.state?.source,
    !!focusKey && !!store[focusKey]?.loaded,
    focusKey ? (store[focusKey]?.items ?? []) : [],
    loadFocusPage,
    mergeFocusPage,
  );

  React.useEffect(() => {
    if (approvalFocus.state) atBottomRef.current = false;
  }, [approvalFocus.state, atBottomRef]);

  // Main agent is the human↔orchestrator CONSOLE: only the conversation (user msgs +
  // the main agent's own replies/steps). Planner session shows planner steps; a
  // worker session shows only its intent's activity, led by the intent objective.
  const activity = React.useMemo(() => {
    const items = activeState?.items ?? [];
    // The main-agent console renders purely from server data: the human turn is
    // persisted+broadcast by the backend BEFORE it returns, so it arrives over the
    // same SSE stream (worker="mainagent") as every agent step — no client-side
    // optimistic echo, hence no fabricated seq that could collide with real DB ids.
    if (isMain) return items;
    if (isPlanner || isSystem) return items;
    // Worker session: the intent leads the transcript as a right-aligned "user"-style
    // message (the task handed to this worker), followed by its execution steps.
    const intentTitle = sessionMeta.get(active.id)?.title ?? active.title;
    const intentMsg: Activity = {
      seq: -1, // sorts/leads before any real step (real seq ≥ 0)
      worker: items[0]?.worker ?? active.id, // reuse the lane so no worker chips appear
      ts: active.last_activity || "",
      kind: "intent", // LLM-generated objective — rendered as a distinct (non-human) bubble
      summary: intentTitle,
      source_task_id: active.source_task_id,
      inherited: active.inherited,
    };
    return [intentMsg, ...items];
  }, [
    isMain,
    isPlanner,
    isSystem,
    activeState,
    active.id,
    active.title,
    active.last_activity,
    active.source_task_id,
    active.inherited,
    sessionMeta,
  ]);

  // seq of this session's most-recent TodoWrite call — for the Todo popover.
  const latestTodoSeq = React.useMemo(() => {
    for (let i = activity.length - 1; i >= 0; i--) {
      const a = activity[i];
      if (a.kind === "tool_use" && a.tool === "TodoWrite") return a.seq;
    }
    return null;
  }, [activity]);

  // Persisted result totals come from the backend's full activity history. Only
  // the latest unfinished usage frame is added client-side, so partial history
  // pages cannot undercount completed runs or double-count the active one.
  const tokenForSession = React.useCallback(
    (key: string): TokenTotal => {
      const saved = sessionTokens[key];
      const total: TokenTotal = {
        input_tokens: saved?.input_tokens ?? 0,
        output_tokens: saved?.output_tokens ?? 0,
        cache_read_tokens: saved?.cache_read_tokens ?? 0,
        cache_write_tokens: saved?.cache_write_tokens ?? 0,
      };
      const items = store[key]?.items ?? [];
      for (let index = items.length - 1; index >= 0; index--) {
        const item = items[index];
        if (item.kind === "result") break;
        if (item.kind !== "usage") continue;
        total.input_tokens += item.input_tokens ?? 0;
        total.output_tokens += item.output_tokens ?? 0;
        total.cache_read_tokens += item.cache_read_tokens ?? 0;
        total.cache_write_tokens += item.cache_write_tokens ?? 0;
        break;
      }
      return total;
    },
    [sessionTokens, store],
  );

  const activeTokens = tokenForSession(activeKey);

  const tokenTotal = {
    i: activeTokens.input_tokens,
    o: activeTokens.output_tokens,
    cr: activeTokens.cache_read_tokens,
    any:
      activeTokens.input_tokens +
        activeTokens.output_tokens +
        activeTokens.cache_read_tokens +
        activeTokens.cache_write_tokens >
      0,
  };

  // Run duration = span from this session's first step to its last (for a live
  // session, "now" so it ticks up — the 1.5s setTick above re-renders it).
  const runDuration = React.useMemo(() => {
    let min = Infinity,
      max = 0;
    for (const a of activity) {
      const t = Date.parse(a.ts);
      if (!Number.isFinite(t)) continue;
      if (t < min) min = t;
      if (t > max) max = t;
    }
    if (!Number.isFinite(min) || max === 0) return null;
    const end = active.live ? Date.now() : max;
    return Math.max(0, end - min);
  }, [activity, active.live]);

  const mainLoaded = !!store[currentMainKey]?.loaded;

  // 送信キーはシステム設定（localStorage）で決まる。既定は Enter で送信。
  const sendMode = useChatSendMode();

  // What the transcript pane should show for the active session.
  const showLoader = !activeState || (activeState.loading && !activeState.loaded);

  const resolutionForSession = (session: Session): TaskLLMResolution | undefined => {
    if (!llmResolutions || session.inherited || session.role === "system") return undefined;
    if (session.role === "mainagent") return llmResolutions.mainagent;
    if (session.role === "planner") return llmResolutions.planner;
    return llmResolutions.worker;
  };

  const activeResolution = resolutionForSession(active);

  const activeAssets =
    active.role === "worker" && active.intent_id ? intentAssetsByID.get(active.intent_id) : undefined;
  const { contentRef } = useSessionScroll({
    ...state,
    activeId,
    activity,
    approvalFocus,
    focusHistory,
    loadEarlier,
  });
  const { stop, send, sendWorkerChat } = useSessionMessageActions(taskId, {
    ...state,
    active,
    mainBusy,
    side,
    patchIntentState,
  });
  return {
    taskId,
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
  };
}

export type TaskSessionsController = ReturnType<typeof useTaskSessions>;
