// Real backend client. /api/* is proxied to the Go backend (next.config rewrites).
// Returns the domain types in lib/types.ts. Shapes match the backend handlers;
// a few fields the backend serializes differently (e.g. created_at as a unix int)
// are passed through and formatted at the call site.

import { MOCK } from "@/lib/mock/enabled";
import { request } from "@/lib/request";
import type { WorkspaceFile, WorkspaceListing } from "@/lib/types/workspace";

import { del, get, post } from "./transport";

export const workspaceApi = {
  // ---- workspace file manager (workDir) ----
  workspaceList: (path = "") => get<WorkspaceListing>(`/workspace/list?path=${encodeURIComponent(path)}`),

  workspaceRead: (path: string) => get<WorkspaceFile>(`/workspace/read?path=${encodeURIComponent(path)}`),

  workspaceWrite: (path: string, content: string) =>
    post<{ ok: boolean; path: string }>(`/workspace/write`, { path, content }),

  workspaceMkdir: (path: string) => post<{ ok: boolean; path: string }>(`/workspace/mkdir`, { path }),

  workspaceDelete: (path: string) => del<{ ok: boolean }>(`/workspace/delete?path=${encodeURIComponent(path)}`),

  workspaceUpload: async (dir: string, files: File[]) => {
    if (MOCK) return { uploaded: files.length };
    const fd = new FormData();
    for (const f of files) fd.append("file", f);
    const r = await request(`/api/workspace/upload?path=${encodeURIComponent(dir)}`, {
      method: "POST",
      body: fd,
    });
    if (!r.ok) throw new Error(`アップロードに失敗しました: ${r.status}`);
    return r.json() as Promise<{ uploaded: number }>;
  },

  workspaceDownload: async (path: string) => {
    let blob: Blob;
    if (MOCK) {
      blob = new Blob([`（demo）${path} のダウンロード内容サンプル。`], { type: "text/plain" });
    } else {
      const r = await request(`/api/workspace/download?path=${encodeURIComponent(path)}`, {});
      if (!r.ok) throw new Error(`ダウンロードに失敗しました: ${r.status}`);
      blob = await r.blob();
    }
    const objUrl = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = objUrl;
    a.download = path.split("/").pop() || "download";
    document.body.appendChild(a);
    a.click();
    a.remove();
    URL.revokeObjectURL(objUrl);
  },
};
