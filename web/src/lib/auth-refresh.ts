// F6: 401 → refresh-and-retry の単一インスタンス refresh。
// access token(2h,localStorage)が失効したら、HttpOnly refresh cookie(14d,
// Path=/api/auth)で /api/auth/refresh に新 token を要求する。並行 401 は同じ
// refresh 要求を共有し、複数要求が同時に refresh token を回転して互いに無効化するのを防ぐ。

export type FetchLike = (input: string, init?: RequestInit) => Promise<Response>;

let refreshPromise: Promise<boolean> | null = null;

// refreshAccessToken は refresh cookie で新しい access token を取得し、
// localStorage に書き戻す。false は refresh も失効(ログイン状態を消して /login へ)。
// fetchImpl は node:test の単体テスト用に注入できる。
export function refreshAccessToken(fetchImpl: FetchLike = fetch): Promise<boolean> {
  if (!refreshPromise) {
    refreshPromise = (async () => {
      try {
        const r = await fetchImpl("/api/auth/refresh", { method: "POST" });
        if (!r.ok) return false;
        const data = (await r.json()) as { token?: unknown };
        if (typeof data.token !== "string" || !data.token) return false;
        localStorage.setItem("artex_token", data.token);
        return true;
      } catch {
        return false;
      }
    })().finally(() => {
      refreshPromise = null;
    });
  }
  return refreshPromise;
}
