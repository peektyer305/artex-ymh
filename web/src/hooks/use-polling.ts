"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import { createPoller, type Poller } from "@/lib/poller";

export function usePolling<T>(fetcher: (signal: AbortSignal) => Promise<T>, receive: (value: T) => void, interval: number) {
  const ref = useRef<Poller<T> | null>(null);
  const [error, setError] = useState<unknown>(null);
  useEffect(() => {
    const poller = createPoller(fetcher, (value) => { setError(null); receive(value); }, interval, setError);
    ref.current = poller;
    void poller.refresh();
    return () => { ref.current = null; poller.stop(); };
  }, [fetcher, receive, interval]);
  const refresh = useCallback(() => ref.current?.refresh() ?? Promise.resolve(), []);
  return { refresh, error };
}
