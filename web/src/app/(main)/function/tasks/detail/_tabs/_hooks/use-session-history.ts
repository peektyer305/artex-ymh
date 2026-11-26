"use client";
import * as React from "react";

import { toast } from "sonner";

import { api, sseUrl } from "@/lib/api";
import { MOCK } from "@/lib/mock/enabled";
import type { Activity } from "@/lib/types";

import {
  emptyState,
  MAIN_ID,
  MAX_KEEP,
  mainSessionId,
  mainSessionKey,
  mergeBySeq,
  PAGE,
  type SessionState,
  type SessionStore,
  SYSTEM_SCAN_PAGE,
  sessionKeyOf,
} from "../_lib/session-model";
import type { TaskSessionState } from "./use-task-session-state";

type Context = Pick<
  TaskSessionState,
  | "activeKeyRef"
  | "atBottomRef"
  | "esRef"
  | "llmToastSeqRef"
  | "loadingKeysRef"
  | "reqTokenRef"
  | "setActiveId"
  | "setCurrentSeg"
  | "setFirstIntentsHasMore"
  | "setHasLoadedOlderIntentsPage"
  | "setIntentAssets"
  | "setIntents"
  | "setLLMResolutions"
  | "setMainSegs"
  | "setOlderIntents"
  | "setOlderIntentsHasMore"
  | "setSessionTokens"
  | "setSseLive"
  | "setStore"
  | "setTaskTokens"
  | "snapshotRef"
  | "store"
>;

export function useSessionHistory(taskId: string, context: Context) {
  const {
    activeKeyRef,
    atBottomRef,
    esRef,
    llmToastSeqRef,
    loadingKeysRef,
    reqTokenRef,
    setActiveId,
    setCurrentSeg,
    setFirstIntentsHasMore,
    setHasLoadedOlderIntentsPage,
    setIntentAssets,
    setIntents,
    setLLMResolutions,
    setMainSegs,
    setOlderIntents,
    setOlderIntentsHasMore,
    setSessionTokens,
    setSseLive,
    setStore,
    setTaskTokens,
    snapshotRef,
    store,
  } = context;
  // ── store helpers ──────────────────────────────────────────────────────────────
  const patchStore = React.useCallback(
    (key: string, fn: (s: SessionState) => SessionState) => {
      setStore((prev) => ({ ...prev, [key]: fn(prev[key] ?? emptyState()) }));
    },
    [setStore],
  );

  // Load a session's LATEST page (before=0) on first open. Keyed + request-token
  // guarded so a switch away can't corrupt the view.
  const loadSession = React.useCallback(
    (key: string) => {
      if (MOCK) return; // MOCK preloads everything up front
      if (loadingKeysRef.current.has(key)) return; // already in flight (e.g. main on mount)
      loadingKeysRef.current.add(key);
      const token = (reqTokenRef.current[key] ?? 0) + 1;
      reqTokenRef.current[key] = token;
      patchStore(key, (s) => ({ ...s, loading: true, error: undefined }));

      if (key === "system") {
        void (async () => {
          let since = 0;
          let systemItems: Activity[] = [];
          for (;;) {
            const page = await api.activity(taskId, { since, limit: SYSTEM_SCAN_PAGE });
            if (reqTokenRef.current[key] !== token) return;
            systemItems = mergeBySeq(
              systemItems,
              page.items.filter(
                (item) => item.worker === "system" || item.kind === "llm_switch" || item.kind === "llm_failover",
              ),
            );
            if (page.items.length < SYSTEM_SCAN_PAGE || page.cursor <= since) break;
            since = page.cursor;
          }
          patchStore(key, (s) => {
            const items = mergeBySeq(systemItems, s.items);
            return {
              ...s,
              items,
              loaded: true,
              loading: false,
              hasMore: false,
              earliestSeq: items.length ? items[0].seq : 0,
              unread: 0,
              lastTs: items.length ? items[items.length - 1].ts : s.lastTs,
              error: undefined,
            };
          });
        })()
          .catch((error) => {
            if (reqTokenRef.current[key] !== token) return;
            patchStore(key, (s) => ({
              ...s,
              loading: false,
              error: (error as Error).message || "読み込みに失敗しました",
            }));
          })
          .finally(() => loadingKeysRef.current.delete(key));
        return;
      }

      api
        .activityHistory(taskId, key, 0, PAGE)
        .then((r) => {
          if (reqTokenRef.current[key] !== token) return; // superseded
          if (r.snapshotCursor > snapshotRef.current) snapshotRef.current = r.snapshotCursor;
          patchStore(key, (s) => {
            const items = mergeBySeq(r.items, s.items); // keep any live frames arrived meanwhile
            return {
              ...s,
              items,
              loaded: true,
              loading: false,
              hasMore: r.hasMore,
              earliestSeq: items.length ? items[0].seq : 0,
              unread: 0,
              error: undefined,
            };
          });
        })
        .catch((e) => {
          if (reqTokenRef.current[key] !== token) return;
          patchStore(key, (s) => ({ ...s, loading: false, error: (e as Error).message || "読み込みに失敗しました" }));
        })
        .finally(() => loadingKeysRef.current.delete(key));
    },
    [taskId, patchStore, snapshotRef, reqTokenRef, loadingKeysRef],
  );

  // Load one older page (scroll-up) for a session, preserving scroll position.
  const loadEarlier = React.useCallback(
    (key: string, viewport: () => HTMLElement | null) => {
      const st = store[key];
      if (!st || st.loadingMore || !st.hasMore || !st.earliestSeq) return;
      const vp = viewport();
      const prevH = vp?.scrollHeight ?? 0;
      const prevTop = vp?.scrollTop ?? 0;
      patchStore(key, (s) => ({ ...s, loadingMore: true }));
      api
        .activityHistory(taskId, key, st.earliestSeq, PAGE)
        .then((r) => {
          patchStore(key, (s) => {
            const items = mergeBySeq(r.items, s.items);
            return {
              ...s,
              items,
              loadingMore: false,
              hasMore: r.hasMore,
              earliestSeq: items.length ? items[0].seq : s.earliestSeq,
            };
          });
          requestAnimationFrame(() => {
            const v = viewport();
            if (v) v.scrollTop = prevTop + (v.scrollHeight - prevH);
          });
        })
        .catch(() => {
          patchStore(key, (s) => ({ ...s, loadingMore: false }));
        });
    },
    [taskId, store, patchStore],
  );

  // ── first load + single task SSE ────────────────────────────────────────────────
  // On task open: load Main's latest page, take the task-level snapshot cursor from
  // it, THEN open ONE task SSE at since=snapshot_cursor. History covers id≤cursor and
  // the SSE covers id>cursor with no gap. The SSE tails ALL agents; frames are routed
  // by session_key. EventSource auto-reconnects and (via our `id:` lines → Last-Event-
  // ID) resumes from the DB, so a dropped realtime link self-heals; seq-merge dedups.
  React.useEffect(() => {
    setStore({});
    setIntents([]);
    setIntentAssets([]);
    setOlderIntents([]);
    setFirstIntentsHasMore(false);
    setOlderIntentsHasMore(false);
    setHasLoadedOlderIntentsPage(false);
    setTaskTokens(null);
    setSessionTokens({});
    setLLMResolutions(null);
    setSseLive(false);
    setActiveId(MAIN_ID); // a stale worker id from the previous task must not leak in
    setMainSegs([{ seq: 0, created_at: "" }]);
    setCurrentSeg(0);
    snapshotRef.current = 0;
    llmToastSeqRef.current = new Set();
    reqTokenRef.current = {};
    // Reserve the initial main key so the active-session effect (which fires for main on
    // mount) won't double-load it and pre-empt the SSE opened here.
    loadingKeysRef.current = new Set([mainSessionKey(0)]);
    let alive = true;

    // MOCK demo: no SSE backend — pull one activity snapshot and bucket by session.
    if (MOCK) {
      api
        .activity(taskId)
        .then((r) => {
          if (!alive) return;
          const buckets: SessionStore = {};
          for (const a of r.items) {
            const k = sessionKeyOf(a);
            if (!buckets[k]) buckets[k] = emptyState();
            buckets[k].items.push(a);
          }
          for (const k of Object.keys(buckets)) {
            const st = buckets[k];
            st.items.sort((p, q) => p.seq - q.seq);
            st.loaded = true;
            st.hasMore = false;
            st.earliestSeq = st.items.length ? st.items[0].seq : 0;
            st.lastTs = st.items.length ? st.items[st.items.length - 1].ts : "";
          }
          buckets[mainSessionKey(0)] ??= { ...emptyState(), loaded: true };
          buckets.system ??= { ...emptyState(), loaded: true };
          setStore(buckets);
        })
        .catch(() =>
          setStore({
            [mainSessionKey(0)]: { ...emptyState(), loaded: true },
            system: { ...emptyState(), loaded: true },
          }),
        );
      return () => {
        alive = false;
      };
    }

    const token = (reqTokenRef.current.mainboot ?? 0) + 1;
    reqTokenRef.current.mainboot = token;
    let bootKey = mainSessionKey(0);
    // F6: SSE の認証情報は 60s の使い捨て ticket（api.ts sseUrl 参照）。EventSource 標準の
    // 自動再接続は 2 回目のハンドシェイクで必ず 401 になる。接続が完全に閉じたら ticket を取り直して再接続する。snapshot カーソル
    // の再生 + mergeBySeq の seq 重複排除で空いた区間を補い、従来の Last-Event-ID 継続と等価になる。
    let sseRetry: ReturnType<typeof setTimeout> | undefined;
    const openStreamRef = { current: null as null | (() => void) };
    // Resolve the main-agent segments first, then load the CURRENT segment's history and
    // open the SSE from its snapshot cursor. The SSE tails all segments and routes each
    // frame by session_key (main:<seg>), so switching segments needs no new stream.
    api
      .mainSessions(taskId)
      .then((ms) => {
        if (!alive || reqTokenRef.current.mainboot !== token) throw new Error("superseded");
        const segs = ms.sessions.length ? ms.sessions : [{ seq: 0, created_at: "" }];
        setMainSegs(segs);
        setCurrentSeg(ms.current);
        bootKey = mainSessionKey(ms.current);
        if (bootKey !== mainSessionKey(0)) loadingKeysRef.current.delete(mainSessionKey(0));
        loadingKeysRef.current.add(bootKey);
        setActiveId(mainSessionId(ms.current));
        patchStore(bootKey, (s) => ({ ...s, loading: true }));
        return api.activityHistory(taskId, bootKey, 0, PAGE);
      })
      .then((r) => {
        if (!alive || reqTokenRef.current.mainboot !== token) return;
        snapshotRef.current = r.snapshotCursor;
        patchStore(bootKey, (s) => {
          const items = mergeBySeq(r.items, s.items);
          return {
            ...s,
            items,
            loaded: true,
            loading: false,
            hasMore: r.hasMore,
            earliestSeq: items.length ? items[0].seq : 0,
            unread: 0,
          };
        });
        // Open the single task SSE from the snapshot cursor.
        openStreamRef.current = () => {
          void sseUrl(
            `/api/exploration/activity/stream?task=${encodeURIComponent(taskId)}&since=${snapshotRef.current}`,
          )
            .then((url) => {
              if (!alive) return;
              const es = new EventSource(url);
              esRef.current = es;
              es.onopen = () => setSseLive(true);
              es.onerror = () => {
                setSseLive(false);
                // 使い捨て ticket はブラウザの自動再接続に対応しない（再試行すると 401）。完全に閉じたら ticket を取り直して再接続する。
                if (es.readyState === EventSource.CLOSED && alive && esRef.current === es) {
                  esRef.current = null;
                  sseRetry = setTimeout(() => openStreamRef.current?.(), 3000);
                }
              };
              es.onmessage = (e) => {
                let a: Activity;
                try {
                  a = JSON.parse(e.data) as Activity;
                } catch {
                  return; // ignore malformed frame
                }
                if ((a.kind === "llm_switch" || a.kind === "llm_failover") && !llmToastSeqRef.current.has(a.seq)) {
                  llmToastSeqRef.current.add(a.seq);
                  const transition = a.metadata?.llm_transition;
                  if (transition?.mode === "exhausted" || a.is_error) {
                    toast.error(a.summary, { id: `task-${taskId}-llm-${a.seq}` });
                  } else if (transition?.mode === "automatic") {
                    toast.success(a.summary, { id: `task-${taskId}-llm-${a.seq}` });
                  } else {
                    toast.info(a.summary, { id: `task-${taskId}-llm-${a.seq}` });
                  }
                  void api
                    .taskLLMResolution(taskId)
                    .then((value) => {
                      if (alive) setLLMResolutions(value);
                    })
                    .catch(() => {
                      // The periodic resolver poll will retry if this event-triggered refresh fails.
                    });
                }
                const k = sessionKeyOf(a);
                setStore((prev) => {
                  const cur = prev[k] ?? emptyState();
                  const activeK = activeKeyRef.current;
                  // Merge into sessions that are loaded, actively loading, or the current
                  // view (so returning is instant + a frame that lands mid-load isn't lost).
                  // A cold, inactive session also keeps a tiny accounting tail so the
                  // sidebar can add the latest unfinished usage without loading history.
                  if (!cur.loaded && !cur.loading && k !== activeK) {
                    const accountingItems =
                      a.kind === "usage" || a.kind === "result" ? mergeBySeq(cur.items, [a]).slice(-4) : cur.items;
                    return {
                      ...prev,
                      [k]: { ...cur, items: accountingItems, lastTs: a.ts, unread: cur.unread + 1 },
                    };
                  }
                  let items = mergeBySeq(cur.items, [a]);
                  // Memory bound: trim oldest when over cap (older re-fetched on scroll-up),
                  // but never while the user is reading this session's history (scrolled up).
                  let hasMore = cur.hasMore;
                  let earliestSeq = cur.earliestSeq;
                  const trimmable = k !== activeK || atBottomRef.current;
                  if (trimmable && items.length > MAX_KEEP) {
                    items = items.slice(items.length - MAX_KEEP);
                    hasMore = true;
                    earliestSeq = items[0].seq;
                  }
                  const unread = k === activeK ? 0 : cur.unread + 1;
                  return { ...prev, [k]: { ...cur, items, lastTs: a.ts, unread, hasMore, earliestSeq } };
                });
              };
            })
            .catch(() => {
              if (alive) sseRetry = setTimeout(() => openStreamRef.current?.(), 3000);
            });
        };
        openStreamRef.current();
      })
      .catch((err) => {
        if (!alive || reqTokenRef.current.mainboot !== token) return;
        if ((err as Error).message === "superseded") return;
        patchStore(bootKey, (s) => ({
          ...s,
          loading: false,
          error: (err as Error).message || "読み込みに失敗しました",
        }));
      })
      .finally(() => loadingKeysRef.current.delete(bootKey));

    return () => {
      alive = false;
      if (sseRetry) clearTimeout(sseRetry);
      esRef.current?.close();
      esRef.current = null;
    };
  }, [
    taskId,
    patchStore,
    activeKeyRef,
    setTaskTokens,
    setSseLive,
    setOlderIntentsHasMore,
    setCurrentSeg,
    setFirstIntentsHasMore,
    setHasLoadedOlderIntentsPage,
    setIntentAssets,
    setIntents,
    setLLMResolutions,
    setMainSegs,
    setOlderIntents,
    setSessionTokens,
    setStore,
    snapshotRef,
    setActiveId,
    reqTokenRef,
    loadingKeysRef,
    llmToastSeqRef,
    atBottomRef,
    esRef,
  ]);
  return { patchStore, loadSession, loadEarlier };
}
