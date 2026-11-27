"use client";

import * as React from "react";

import { api } from "@/lib/api";
import { createPoller } from "@/lib/poller";
import type { FindingStats } from "@/lib/types";

const EMPTY_STATS: FindingStats = {
  total: 0,
  pending: 0,
  critical: 0,
  high: 0,
  medium: 0,
  low: 0,
  vulnclasses: [],
  tasks: [],
};

export function useFindingStats() {
  const [stats, setStats] = React.useState<FindingStats>(EMPTY_STATS);
  const [statsLoaded, setStatsLoaded] = React.useState(false);
  const requestVersion = React.useRef(0);
  const mounted = React.useRef(false);

  const receive = React.useCallback((version: number, value: FindingStats) => {
    if (!mounted.current || requestVersion.current !== version) return;
    setStats(value);
    setStatsLoaded(true);
  }, []);

  const refreshStats = React.useCallback(async () => {
    const version = ++requestVersion.current;
    try {
      receive(version, await api.findingStats());
    } catch {
      // Keep the previous counts if an on-demand refresh fails.
    }
  }, [receive]);

  React.useEffect(() => {
    mounted.current = true;
    const poller = createPoller(
      (signal) => {
        const version = ++requestVersion.current;
        return api.findingStats(signal).then((value) => ({ version, value }));
      },
      ({ version, value }) => receive(version, value),
      5000,
    );
    void poller.refresh();
    return () => {
      mounted.current = false;
      requestVersion.current++;
      poller.stop();
    };
  }, [receive]);

  return { stats, statsLoaded, refreshStats };
}
