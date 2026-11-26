import type { Audit } from "@/lib/types/security";

import { get, tq } from "./transport";

export const auditApi = {
  // ---- traffic / audit / report / chat ----
  audit: (task?: string) => get<Audit>(`/audit${tq(task)}`),
};
