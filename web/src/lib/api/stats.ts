import type { CoverageGraphData } from "@/lib/types/assets";
import type { Stats } from "@/lib/types/stats";
import type { UsageStats } from "@/lib/types/tokens";

import { get, tq } from "./transport";

export const statsApi = {
  // ---- stats ----
  stats: (task?: string) => get<Stats>(`/stats${tq(task)}`),

  // アセットのテストカバレッジ(粗い推定、参考値)：範囲内アセットが fact に触れられた割合 + タイプ別の 総数/テスト済み。
  taskCoverage: (id: string) =>
    get<{
      enabled: boolean; // アセットカバレッジ機能が有効か；false の時は他のフィールドはゼロ値
      scope_rows: number;
      denominator: number;
      tested: number;
      pct: number | null;
      by_type: { type: string; total: number; tested: number }[];
    }>(`/tasks/${id}/coverage`),

  // アセットカバレッジグラフ：範囲内の全アセット + 接続用のルートドメイン/企業ノード、tested/in_scope を含む。
  taskCoverageGraph: (id: string) => get<CoverageGraphData>(`/tasks/${id}/coverage-graph`),

  // グローバル llm_usage 集計（ダッシュボードの新 token ビュー）：profile 別の合計 + 日別バケット。
  usageStats: (days = 365) => get<UsageStats>(`/tokens/usage?days=${days}`),
};
