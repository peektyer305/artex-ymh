// Real backend client. /api/* is proxied to the Go backend (next.config rewrites).
// Returns the domain types in lib/types.ts. Shapes match the backend handlers;
// a few fields the backend serializes differently (e.g. created_at as a unix int)
// are passed through and formatted at the call site.

import { MOCK } from "@/lib/mock/enabled";
import { mockHandle } from "@/lib/mock/handler";
import { request } from "@/lib/request";

export async function http<T>(path: string, init?: RequestInit): Promise<T> {
  if (MOCK) return mockHandle<T>(init?.method ?? "GET", path, init?.body ?? null);
  const response = await request(`/api${path}`, init);
  if (response.status === 204) return undefined as T;
  return response.json() as Promise<T>;
}

export async function sseUrl(path: string): Promise<string> {
  const base =
    process.env.NEXT_PUBLIC_SSE_BASE ??
    (typeof window !== "undefined" ? `${window.location.protocol}//${window.location.hostname}:8787` : "");
  const { ticket } = await post<{ ticket: string }>("/sse-ticket");
  const sep = path.includes("?") ? "&" : "?";
  return `${base}${path}${sep}ticket=${encodeURIComponent(ticket)}`;
}

export const get = <T>(p: string, signal?: AbortSignal) => http<T>(p, { signal });

export const post = <T>(p: string, body?: unknown) =>
  http<T>(p, { method: "POST", body: body ? JSON.stringify(body) : undefined });

export const put = <T>(p: string, body?: unknown) =>
  http<T>(p, { method: "PUT", body: body ? JSON.stringify(body) : undefined });

export const patch = <T>(p: string, body?: unknown) =>
  http<T>(p, { method: "PATCH", body: body ? JSON.stringify(body) : undefined });

export const del = <T>(p: string, body?: unknown) =>
  http<T>(p, { method: "DELETE", body: body ? JSON.stringify(body) : undefined });

// Go serializes nil slices as JSON null — coerce to [].
export const arr = <T>(x: T[] | null | undefined): T[] => x ?? [];

export const tq = (task?: string, sep: "?" | "&" = "?") => (task ? `${sep}task=${encodeURIComponent(task)}` : "");

// イントラネット運用ページの 4 つのリストの ?task_id= フィルタパラメータ(空 = グローバルビュー)。
export const taskQuery = (taskId?: string) => (taskId ? `?task_id=${encodeURIComponent(taskId)}` : "");
