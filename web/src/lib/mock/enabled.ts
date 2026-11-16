// Mock スイッチ。ビルド時に注入される公開変数（NEXT_PUBLIC_ 接頭辞のみブラウザから読める）。
// Vercel で NEXT_PUBLIC_MOCK=1 を設定するとサイト全体が mock で動き、バックエンド不要になる。
export const MOCK = process.env.NEXT_PUBLIC_MOCK === "1";
