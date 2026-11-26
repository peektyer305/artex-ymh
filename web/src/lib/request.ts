import { refreshAccessToken } from "./auth-refresh.ts";

export interface RequestDependencies {
  fetch: typeof globalThis.fetch;
  token: () => string | null;
  refresh: () => Promise<boolean>;
  unauthorized: () => void;
}

const defaults: RequestDependencies = {
  fetch: (...args) => globalThis.fetch(...args),
  token: () => typeof window === "undefined" ? null : localStorage.getItem("artex_token"),
  refresh: () => refreshAccessToken(),
  unauthorized: () => {
    if (typeof window === "undefined") return;
    localStorage.removeItem("artex_token");
    document.cookie = "artex_session=; path=/; max-age=0";
    window.location.href = "/login";
  },
};

export class ApiError extends Error {
  status: number;
  constructor(message: string, status: number) {
    super(message);
    this.name = "ApiError";
    this.status = status;
  }
}

// Only a confirmed 401 is retried. Network failures may follow a successful write.
export async function request(
  path: string,
  init: RequestInit = {},
  dependencies: RequestDependencies = defaults,
): Promise<Response> {
  const pathname = new URL(path, "http://artex.invalid").pathname;
  const replayable = !(typeof ReadableStream !== "undefined" && init.body instanceof ReadableStream);
  for (let attempt = 0; attempt < 2; attempt++) {
    const headers = new Headers(init.headers);
    const token = dependencies.token();
    if (token) headers.set("Authorization", `Bearer ${token}`);
    if (typeof init.body === "string" && !headers.has("Content-Type")) {
      headers.set("Content-Type", "application/json");
    }
    const response = await dependencies.fetch(path, { ...init, headers });
    if (response.status === 401) {
      if (attempt === 0 && replayable && !pathname.startsWith("/api/auth/") && await dependencies.refresh()) {
        await response.body?.cancel();
        continue;
      }
      dependencies.unauthorized();
      throw new ApiError("ログインの有効期限が切れました。再度ログインしてください。", 401);
    }
    if (!response.ok) {
      let message = `${init.method ?? "GET"} ${pathname}: ${response.status}`;
      const text = await response.text();
      try {
        const value: unknown = JSON.parse(text);
        if (typeof value === "object" && value !== null && "error" in value && typeof value.error === "string" && value.error.trim()) {
          message = value.error.trim();
        }
      } catch {
        // Reverse proxies may return HTML. Do not display that response as an error.
      }
      throw new ApiError(message, response.status);
    }
    return response;
  }
  throw new ApiError("認証を更新できませんでした。", 401);
}
