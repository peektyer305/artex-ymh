import type { TrafficDetail, TrafficHost, TrafficResp } from "@/lib/types/traffic";

import { del, get } from "./transport";

export const trafficApi = {
  traffic: (page = 0, size = 100, host = "", method = "", q = "") =>
    get<TrafficResp>(
      `/traffic?page=${page}&size=${size}` +
        (host ? `&host=${encodeURIComponent(host)}` : "") +
        (method && method !== "all" ? `&method=${encodeURIComponent(method)}` : "") +
        (q ? `&q=${encodeURIComponent(q)}` : ""),
    ),

  trafficExchange: (id: string) => get<TrafficDetail>(`/traffic/exchange?id=${encodeURIComponent(id)}`),

  trafficHosts: () => get<{ hosts: TrafficHost[] }>(`/traffic/hosts`),

  trafficDeleteHost: (host: string) => del<{ deleted: number }>(`/traffic?host=${encodeURIComponent(host)}`),

  trafficDeleteHosts: (hosts: string[]) => del<{ deleted: number }>(`/traffic/hosts`, { hosts }),
};
