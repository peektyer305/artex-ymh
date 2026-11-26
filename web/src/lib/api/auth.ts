import { get, post } from "./transport";

export const authApi = {
  // バックエンドのアプリバージョン（release 時に ldflags で注入、デフォルト "dev"）。
  health: () => get<{ ok: boolean; service: string; version: string }>("/health"),

  // ---- auth ----
  authStatus: () => get<{ initialized: boolean }>("/auth/status"),

  login: (username: string, password: string) => post<{ token: string }>("/auth/login", { username, password }),

  initPassword: (password: string) => post<{ token: string }>("/auth/init", { password }),

  changePassword: (oldPassword: string, newPassword: string) =>
    post<{ ok: boolean }>("/auth/change-password", { old_password: oldPassword, new_password: newPassword }),

  // ログアウト:サーバ側の refresh token を失効させ HttpOnly cookie を消す;失敗(期限切れなど)しても
  // 従来通りローカルのログイン状態を消し、呼び出し側が後始末する。
  logout: () => post<{ ok: boolean }>("/auth/logout"),
};
