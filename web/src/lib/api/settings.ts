import type { Settings } from "@/lib/types/settings";

import { get, post, put } from "./transport";

export const settingsApi = {
  // ---- app settings (runtime toggles) ----
  settings: () => get<Settings>(`/settings`),

  setSettings: (patch: Partial<Settings>) => put<Settings>(`/settings`, patch),

  // Run a real "test" search with the given (or saved) config to verify it works.
  testWebSearch: (patch: {
    web_search_backend?: string;
    web_search_proxy?: string;
    brave_search_api_key?: string;
    tavily_search_api_key?: string;
  }) => post<{ ok: boolean; error?: string; count?: number; backend?: string }>(`/settings/web-search/test`, patch),

  gc: (ttl = 86400) => post<{ removed: number }>(`/gc?ttl=${ttl}`, {}),
};
