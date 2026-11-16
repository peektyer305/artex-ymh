import { NextResponse } from "next/server";
import type { NextRequest } from "next/server";

const AUTH_PAGES = ["/login", "/setup"];

export function proxy(request: NextRequest) {
  // Mock demo：実際のログインはなく全ページを許可（クライアント側 auth ガードも許可）。
  if (process.env.NEXT_PUBLIC_MOCK === "1") return NextResponse.next();

  const { pathname } = request.nextUrl;
  // artex_session は「ログイン済み」を示す cookie（非機密、token を含まない。F6 以降 token は
  // localStorage + HttpOnly refresh cookie のみに保存され、middleware からは読めず、読むべきでもない）。
  const token = request.cookies.get("artex_session")?.value;
  const isAuthPage = AUTH_PAGES.some((p) => pathname === p || pathname.startsWith(`${p}/`));

  // 未ログイン → ログインページへリダイレクト
  if (!token && !isAuthPage) {
    return NextResponse.redirect(new URL("/login", request.url));
  }

  // ログイン済みでログイン/初期化ページへアクセス → メイン画面へリダイレクト
  if (token && isAuthPage) {
    return NextResponse.redirect(new URL("/function/tasks", request.url));
  }

  return NextResponse.next();
}

export const config = {
  // Next.js 内部ルート、API ルート、favicon、public/ 配下の静的ファイル（画像・フォントなど）を除外
  matcher: [
    "/((?!_next/static|_next/image|favicon\\.ico|api/|.*\\.(?:png|jpg|jpeg|gif|webp|svg|ico|woff2?|ttf|otf)$).*)",
  ],
};
