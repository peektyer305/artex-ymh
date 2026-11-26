// Real backend client. /api/* is proxied to the Go backend (next.config rewrites).
// Returns the domain types in lib/types.ts. Shapes match the backend handlers;
// a few fields the backend serializes differently (e.g. created_at as a unix int)
// are passed through and formatted at the call site.

import { MOCK } from "@/lib/mock/enabled";
import { request } from "@/lib/request";

import { mockReport } from "./mock-report";
import { tq } from "./transport";

export const reportsApi = {
  report: async (task?: string) => {
    if (MOCK) return mockReport(task);
    const r = await request(`/api/report${tq(task)}`, {});
    if (!r.ok) throw new Error(`report: ${r.status}`);
    return r.text();
  },
};
