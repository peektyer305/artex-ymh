"use client";
import * as React from "react";

import { api } from "@/lib/api";

import type { TaskSessionState } from "./use-task-session-state";

type Context = Pick<
  TaskSessionState,
  | "chatStatusRequestRef"
  | "setLLMResolutions"
  | "setMainChatRunning"
  | "setPendingIntercepts"
  | "setSessionTokens"
  | "setTaskTokens"
>;

export function useSessionRuntimePolling(taskId: string, context: Context) {
  const {
    chatStatusRequestRef,
    setLLMResolutions,
    setMainChatRunning,
    setPendingIntercepts,
    setSessionTokens,
    setTaskTokens,
  } = context;
  // ── task token total (whole task, all agents) ───────────────────────────────────
  React.useEffect(() => {
    let alive = true;
    const load = () =>
      api
        .tokenStats(taskId)
        .then((r) => {
          if (!alive) return;
          setTaskTokens(r.total);
          setSessionTokens(Object.fromEntries(r.sessions.map((item) => [item.session, item])));
        })
        .catch(() => {
          // Polling is best-effort; the next interval retries automatically.
        });
    void load();
    const t = setInterval(load, 5000);
    return () => {
      alive = false;
      clearInterval(t);
    };
  }, [taskId, setTaskTokens, setSessionTokens]);

  React.useEffect(() => {
    let alive = true;
    setMainChatRunning(null);
    const load = () => {
      const request = ++chatStatusRequestRef.current;
      return api
        .chatStatus(taskId)
        .then(({ running }) => {
          if (alive && chatStatusRequestRef.current === request) setMainChatRunning(running);
        })
        .catch(() => {
          // Keep the last authoritative value. Before the first success, recent
          // activity remains a conservative fallback.
        });
    };
    void load();
    const timer = setInterval(load, 2000);
    return () => {
      alive = false;
      clearInterval(timer);
    };
  }, [taskId, setMainChatRunning, chatStatusRequestRef]);

  React.useEffect(() => {
    let alive = true;
    const load = () =>
      api
        .taskLLMResolution(taskId)
        .then((value) => {
          if (alive) setLLMResolutions(value);
        })
        .catch(() => {
          // Polling is best-effort; the next interval retries automatically.
        });
    void load();
    const timer = setInterval(load, 10_000);
    return () => {
      alive = false;
      clearInterval(timer);
    };
  }, [taskId, setLLMResolutions]);

  React.useEffect(() => {
    let alive = true;
    const load = () =>
      api
        .interceptTask(taskId)
        .then((rows) => {
          if (alive) setPendingIntercepts(rows.filter((r) => r.status === "pending"));
        })
        .catch(() => {
          // Polling is best-effort; the next interval retries automatically.
        });
    void load();
    const t = setInterval(load, 5000);
    return () => {
      alive = false;
      clearInterval(t);
    };
  }, [taskId, setPendingIntercepts]);

  // Re-render on a timer so "streaming" liveness recomputes as activity goes stale.
  const [, setTick] = React.useState(0);

  React.useEffect(() => {
    const id = setInterval(() => setTick((t) => t + 1), 1500);
    return () => clearInterval(id);
  }, []);
}
