// sseUrl builds a URL for Server-Sent Events streams. SSE must NOT go through the
// Next.js dev `/api` rewrite: that proxy buffers the streamed response, so event
// frames never reach the browser (the EventSource opens but receives 0 messages).
// We therefore connect straight to the Go backend, whose CORS is open. Override
// with NEXT_PUBLIC_SSE_BASE; set it to "" to force same-origin (e.g. behind a
// production reverse proxy that flushes SSE correctly).
// 認証情報(F6):まず POST /api/sse-ticket(/api 代理経由、Authorization ヘッダ付き)で 60s
// 有効な使い捨て ticket を取り ?ticket= に付ける。access token は URL/アクセスログに出さない。ticket
// は使用後に失効するので、EventSource の自動再接続は 2 回目のハンドシェイクで 401 になる;呼び出し側は
// 接続が完全に閉じた(readyState === CLOSED)後に本関数を呼び直して ticket を取り直し再接続する。
// mockReport returns a canned Markdown report for the demo.
export function mockReport(_task?: string): string {
  return `# ARTEX ペネトレーションテストレポート — Acme Corp

## 概要
- 範囲：acme.com（www / admin / api / shop / vpn サブドメインを含む）
- 確認済みファインディング：6 件（高 3 · 中 3 · 低 2）
- エンジンモード：exploring

## 主要なファインディング
1. **[高] 管理画面のデフォルトパスワード** admin.acme.com admin/admin123 → 管理画面を完全に掌握できる。
2. **[高] SQL インジェクション** www.acme.com/search?q= → acme_prod データベースを読み取れる。
3. **[高] IDOR** api.acme.com/v1/orders?id= → 権限外で他人の注文（電話番号/住所を含む）を読み取れる。
4. **[中] 反射型 XSS**、**.git のソース露出**、**ログインにレート制限なし**。

## 推奨
- 管理画面はパスワード変更を強制 + MFA 有効化、デフォルトパスワードを禁止。
- search エンドポイントはパラメータ化クエリ、出力エンコーディング。
- API にオブジェクト単位の認可チェック（IDOR）を追加、強力な JWT 鍵へ変更。

> （demo）本レポートは mock データから生成したもので、画面デモ専用です。`;
}
