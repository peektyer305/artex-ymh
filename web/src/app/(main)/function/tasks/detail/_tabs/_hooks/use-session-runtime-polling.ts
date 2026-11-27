"use client";
import * as React from "react";

import { api } from "@/lib/api";
import { createPoller } from "@/lib/poller";

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
  React.useEffect(() => {
    const poller = createPoller(
      (signal) => api.tokenStats(taskId, signal),
      (result) => {
        setTaskTokens(result.total);
        setSessionTokens(Object.fromEntries(result.sessions.map((item) => [item.session, item])));
      },
      5000,
    );
    void poller.refresh();
    return () => poller.stop();
  }, [taskId, setTaskTokens, setSessionTokens]);

  React.useEffect(() => {
    setMainChatRunning(null);
    const poller = createPoller(
      (signal) => {
        const request = ++chatStatusRequestRef.current;
        return api.chatStatus(taskId, signal).then(({ running }) => ({ request, running }));
      },
      ({ request, running }) => {
        if (chatStatusRequestRef.current === request) setMainChatRunning(running);
      },
      2000,
    );
    void poller.refresh();
    return () => poller.stop();
  }, [taskId, setMainChatRunning, chatStatusRequestRef]);

  React.useEffect(() => {
    const poller = createPoller((signal) => api.taskLLMResolution(taskId, signal), setLLMResolutions, 10_000);
    void poller.refresh();
    return () => poller.stop();
  }, [taskId, setLLMResolutions]);

  React.useEffect(() => {
    const poller = createPoller(
      (signal) => api.interceptTask(taskId, signal),
      (rows) => setPendingIntercepts(rows.filter((row) => row.status === "pending")),
      5000,
    );
    void poller.refresh();
    return () => poller.stop();
  }, [taskId, setPendingIntercepts]);

  // Re-render on a timer so "streaming" liveness recomputes as activity goes stale.
  const [, setTick] = React.useState(0);

  React.useEffect(() => {
    const id = setInterval(() => setTick((t) => t + 1), 1500);
    return () => clearInterval(id);
  }, []);
}
