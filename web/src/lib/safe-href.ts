// safeHref は href に入れてよい URL かを検証し、http: / https: スキームのみを許可する。
//
// 背景:アセット URL や GitHub release のアドレスなどはバックエンド/対象側から来るデータで、
// そのまま <a href> に入れると javascript: や data: などの偽スキームがリンクになる(XSS の入口)。
// コンパイル時定数でない href は必ずこの検証を通し、undefined なら呼び出し側はプレーンテキストで描画する。
//
// 注意：URL コンストラクタは相対文字列(例 "/path" やスキーム相対の "//host")に base を要求するが、
// ここではあえて base を渡さない。相対/不正な入力はすべて信頼できないものとして undefined を返す。
export function safeHref(raw: string | null | undefined): string | undefined {
  if (!raw) return undefined;
  const trimmed = raw.trim();
  if (!trimmed) return undefined;
  // 制御文字や空白は scheme に埋め込むと単純な前方一致チェックを回避できる(例 "java\tscript:")。
  // ブラウザは href 解析時にこれらを無視するため、先に除去してから検証する。
  // biome-ignore lint/suspicious/noControlCharactersInRegex: 制御文字を意図的に照合して偽装 scheme を検出する
  const cleaned = trimmed.replace(/[\u0000-\u0020]+/g, "");
  let parsed: URL;
  try {
    parsed = new URL(cleaned);
  } catch {
    return undefined;
  }
  if (parsed.protocol !== "http:" && parsed.protocol !== "https:") return undefined;
  return trimmed;
}
