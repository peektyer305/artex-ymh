const TOKEN_KEY = "artex_token";
// SESSION_COOKIE は「ログイン済み」の目印だけで(認証情報は含まない)、開発モードで
// Next proxy(middleware)がサーバー側リダイレクトに使う。実際の access token は
// localStorage + Authorization ヘッダのみ、refresh token は HttpOnly cookie でどちらも
// JS からは読めない(F6:JS が書き込める token ミラー cookie は廃止)。
const SESSION_COOKIE = "artex_session";

export interface CurrentUser {
  id: string;
  name: string;
  username: string;
  email: string;
  avatar: string;
  role: string;
}

export const auth = {
  getToken(): string | null {
    if (typeof window === "undefined") return null;
    // Mock demo:実際のログインは行わず、偽 token を返してルートガードを通し、そのままメイン画面へ入る。
    return localStorage.getItem(TOKEN_KEY) ?? (process.env.NEXT_PUBLIC_MOCK === "1" ? "mock-demo" : null);
  },

  setToken(token: string): void {
    localStorage.setItem(TOKEN_KEY, token);
    // ログイン目印の cookie のみ書き込む(機密性なし)。Next.js middleware がサーバー側で読む
    document.cookie = `${SESSION_COOKIE}=1; path=/; SameSite=Lax`;
  },

  clearToken(): void {
    localStorage.removeItem(TOKEN_KEY);
    document.cookie = `${SESSION_COOKIE}=; path=/; max-age=0`;
  },

  // JWT payload の sub フィールドから現在のユーザーを解析する。表示のみで署名検証はしない。
  getCurrentUser(): CurrentUser | null {
    const token = this.getToken();
    if (!token) return null;
    try {
      const parts = token.split(".");
      if (parts.length !== 3) return null;
      // base64url → base64
      const payload = JSON.parse(atob(parts[1].replace(/-/g, "+").replace(/_/g, "/")));
      const username: string = payload.sub ?? "ARTEX";
      return { id: "1", name: username, username, email: "", avatar: "", role: "operator" };
    } catch {
      return null;
    }
  },
};
