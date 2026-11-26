"use client";
import * as React from "react";

import { api } from "@/lib/api";

import type { TaskSessionState } from "./use-task-session-state";

type Context = Pick<
  TaskSessionState,
  | "firstIntentsRef"
  | "intents"
  | "loadingOlderIntents"
  | "olderIntents"
  | "setFirstIntentsHasMore"
  | "setHasLoadedOlderIntentsPage"
  | "setIntentAssets"
  | "setIntents"
  | "setLoadingOlderIntents"
  | "setOlderIntents"
  | "setOlderIntentsHasMore"
>;

export function useWorkerSessionPolling(taskId: string, context: Context) {
  const {
    firstIntentsRef,
    intents,
    loadingOlderIntents,
    olderIntents,
    setFirstIntentsHasMore,
    setHasLoadedOlderIntentsPage,
    setIntentAssets,
    setIntents,
    setLoadingOlderIntents,
    setOlderIntents,
    setOlderIntentsHasMore,
  } = context;
  React.useEffect(() => {
    let active = true;
    const load = () =>
      api
        .taskIntentAssets(taskId)
        .then((assets) => {
          if (active) setIntentAssets(assets);
        })
        .catch(() => {
          // The next poll retries; Worker controls and transcripts remain available.
        });
    void load();
    const timer = setInterval(load, 5000);
    return () => {
      active = false;
      clearInterval(timer);
    };
  }, [taskId, setIntentAssets]);

  // ── worker (intent) session list — paged, poll first page lightly ───────────────
  React.useEffect(() => {
    let active = true;
    firstIntentsRef.current = [];
    setIntents([]);
    setOlderIntents([]);
    setFirstIntentsHasMore(false);
    setOlderIntentsHasMore(false);
    const load = () =>
      api
        .intentsPage(taskId, 0, 300)
        .then((r) => {
          if (!active) return;
          const freshIds = new Set(r.items.map((item) => item.id));
          const oldestFreshId = r.items.reduce((minimum, item) => Math.min(minimum, Number(item.id)), Infinity);
          const displaced = r.hasMore
            ? firstIntentsRef.current.filter((item) => !freshIds.has(item.id) && Number(item.id) < oldestFreshId)
            : [];
          if (displaced.length > 0) {
            setOlderIntents((previous) => {
              const byId = new Map(previous.map((item) => [item.id, item]));
              for (const item of displaced) byId.set(item.id, item);
              return [...byId.values()];
            });
          }
          firstIntentsRef.current = r.items;
          setIntents(r.items);
          setFirstIntentsHasMore(r.hasMore);
        })
        .catch(() => {
          // Polling is best-effort; the next interval retries automatically.
        });
    void load();
    const t = setInterval(load, 5000);
    return () => {
      active = false;
      clearInterval(t);
    };
  }, [taskId, firstIntentsRef, setFirstIntentsHasMore, setOlderIntentsHasMore, setOlderIntents, setIntents]);

  const loadOlderIntents = React.useCallback(() => {
    if (loadingOlderIntents) return;
    const all = [...intents, ...olderIntents];
    const minId = all.reduce((m, n) => Math.min(m, Number(n.id)), Infinity);
    if (!Number.isFinite(minId)) return;
    setLoadingOlderIntents(true);
    api
      .intentsPage(taskId, minId, 300)
      .then((r) => {
        setOlderIntents((prev) => {
          const seen = new Set([...intents, ...prev].map((n) => n.id));
          return [...prev, ...r.items.filter((n) => !seen.has(n.id))];
        });
        setOlderIntentsHasMore(r.hasMore);
        setHasLoadedOlderIntentsPage(true);
      })
      .catch(() => {
        // A later manual retry can fetch this page again.
      })
      .finally(() => setLoadingOlderIntents(false));
  }, [
    taskId,
    intents,
    olderIntents,
    loadingOlderIntents,
    setOlderIntentsHasMore,
    setOlderIntents,
    setLoadingOlderIntents,
    setHasLoadedOlderIntentsPage,
  ]);
  return { loadOlderIntents };
}
