// イントラネット運用ページ共通の小さなユーティリティ。

/** fmtTime は RFC3339 の時刻を zh-CN の短い形式に整形します。空値・不正値はそのまま返します。 */
export function fmtTime(ts: string | undefined | null): string {
  if (!ts) return "—";
  const d = new Date(ts);
  if (Number.isNaN(d.getTime())) return ts;
  return d.toLocaleString("zh-CN", {
    month: "2-digit",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
  });
}

/** fmtBytes は人間が読めるファイルサイズを返します。 */
export function fmtBytes(n: number): string {
  if (!Number.isFinite(n) || n < 0) return "—";
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
  return `${(n / 1024 / 1024).toFixed(1)} MB`;
}
