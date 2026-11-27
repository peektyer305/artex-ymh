"use client";

import * as React from "react";

import type { Graph as G6Graph } from "@antv/g6";
import { RefreshCw } from "lucide-react";

import { Button } from "@/components/ui/button";
import type { IntranetSegment, IntranetTopology } from "@/lib/types";
import { cn } from "@/lib/utils";

// セグメント配色パレット：segments の順に色を割り当て、超過分は循環して再利用します。
const SEGMENT_PALETTE = [
  "#3b82f6",
  "#10b981",
  "#f59e0b",
  "#8b5cf6",
  "#f43f5e",
  "#06b6d4",
  "#84cc16",
  "#f97316",
  "#ec4899",
  "#14b8a6",
];

// アセットカバレッジ図と同じ方式：lucide アイコン（v1.22 パス）を白い data URI としてノードアイコンに描画します。
// react-dom/server が React19/Next のクライアントバンドルで問題を起こすため、手書きで埋め込みます。
function svgUri(inner: string, filled = false): string {
  const attrs = filled
    ? 'fill="#fff" stroke="none"'
    : 'fill="none" stroke="#fff" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"';
  const svg = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" ${attrs}>${inner}</svg>`;
  return `data:image/svg+xml,${encodeURIComponent(svg)}`;
}

// 一般ホスト = モニター、プラットフォーム自身（コールバック端）= 塗りつぶしの五芒星。
const HOST_ICON = svgUri(
  '<rect x="2" y="3" width="20" height="14" rx="2"/><line x1="8" x2="16" y1="21" x2="21"/><line x1="12" x2="12" y1="17" y2="21"/>',
);
const CALLBACK_ICON = svgUri(
  '<path d="M12 2.5l2.9 6.1 6.6.8-4.9 4.6 1.3 6.6-5.9-3.3-5.9 3.3 1.3-6.6-4.9-4.6 6.6-.8z"/>',
  true,
);

export function segmentColor(segments: IntranetSegment[], segment: string): string {
  const idx = Math.max(
    0,
    segments.findIndex((s) => s.name === segment),
  );
  return SEGMENT_PALETTE[idx % SEGMENT_PALETTE.length];
}

// G6 の型ではカスタムフィールドは data 配下ですが、公式の force サンプル（および実行時）はトップレベルの d.<field> を読みます。
// コールバックの仮引数は unknown で G6 のシグネチャを満たし、内部で nd()/ed() によりトップレベル構造へキャストします。
type G6NodeDatum = {
  id: string;
  lbl: string;
  segment: string;
  hasSession: boolean;
  alive: number;
  isCallback: boolean;
  size: number;
};
type G6EdgeDatum = { source: string; target: string; state: string };

const nd = (d: unknown) => d as G6NodeDatum;
const ed = (d: unknown) => d as G6EdgeDatum;

function toG6(data: IntranetTopology): { nodes: G6NodeDatum[]; edges: G6EdgeDatum[] } {
  return {
    nodes: (data.nodes ?? []).map((n) => ({
      id: n.id,
      lbl: n.alive_sessions > 0 ? `${n.ip} (${n.alive_sessions})` : n.ip || n.id,
      segment: n.segment,
      hasSession: n.has_session,
      alive: n.alive_sessions,
      isCallback: n.is_callback,
      size: n.is_callback ? 34 : 26,
    })),
    edges: (data.edges ?? []).map((e) => ({ source: e.from, target: e.to, state: e.state })),
  };
}

function TopologyLegend({
  data,
  loading,
  onRefresh,
}: {
  data: IntranetTopology | null;
  loading: boolean;
  onRefresh: () => void;
}) {
  const segments = data?.segments ?? [];
  const nodeCount = data?.nodes.length ?? 0;
  const aliveEdges = data?.edges.filter((e) => e.state === "alive").length ?? 0;
  return (
    <div className="bg-card/95 pointer-events-auto absolute top-3 left-3 flex max-w-[360px] flex-col gap-2.5 rounded-lg border p-3 text-xs shadow-sm backdrop-blur">
      <div className="flex items-center justify-between gap-3">
        {data ? (
          <span className="text-muted-foreground">
            ホスト <span className="text-foreground font-semibold tabular-nums">{nodeCount}</span> · 稼働中のリンク{" "}
            <span className="font-semibold tabular-nums text-emerald-600 dark:text-emerald-400">{aliveEdges}</span>
          </span>
        ) : (
          <span className="text-muted-foreground">{loading ? "読み込み中…" : "トポロジデータはありません"}</span>
        )}
        <Button variant="ghost" size="icon" className="size-6" onClick={onRefresh} title="更新">
          <RefreshCw className={cn("size-3.5", loading && "animate-spin")} />
        </Button>
      </div>
      {segments.length > 0 && (
        <div className="flex flex-wrap gap-x-3 gap-y-1.5">
          {segments.map((s) => (
            <span key={s.name} className="text-foreground inline-flex items-center gap-1.5">
              <span className="size-3 rounded-full" style={{ backgroundColor: segmentColor(segments, s.name) }} />
              {s.name}
            </span>
          ))}
        </div>
      )}
      <div className="border-border/60 text-muted-foreground flex flex-wrap gap-x-3 gap-y-1.5 border-t pt-2">
        <span className="inline-flex items-center gap-1.5">
          <span className="size-3 rounded-full border-2 border-emerald-600 bg-neutral-300" /> 稼働中のセッションあり
        </span>
        <span className="inline-flex items-center gap-1.5">
          <span className="size-3 rounded-full border border-white/70 bg-neutral-400" /> セッションなし
        </span>
        <span className="inline-flex items-center gap-1.5">
          <span className="flex size-3 items-center justify-center rounded-full bg-slate-500">
            <svg viewBox="0 0 24 24" className="size-2 fill-white" aria-hidden="true">
              <path d="M12 2.5l2.9 6.1 6.6.8-4.9 4.6 1.3 6.6-5.9-3.3-5.9 3.3 1.3-6.6-4.9-4.6 6.6-.8z" />
            </svg>
          </span>
          プラットフォーム自身
        </span>
        <span className="inline-flex items-center gap-1.5">
          <span className="h-0.5 w-4 rounded-full bg-emerald-500" /> 稼働中のトンネル
        </span>
        <span className="inline-flex items-center gap-1.5">
          <span className="h-0 w-4 border-t-2 border-dashed border-neutral-400" /> 停止/失敗
        </span>
      </div>
      <p className="text-muted-foreground/80 border-border/60 border-t pt-2 leading-relaxed">
        ノードをドラッグして移動し、ホイールで拡大・縮小できます。ホストをクリックすると、下部の「セッション」タブをそのホストで絞り込めます。
      </p>
    </div>
  );
}

// TopologyGraph はイントラネットのホストトポロジを描画します。ノード=ホスト（セグメント別の色分け、セッションありは枠線、プラットフォーム自身は星印）、
// エッジ=トンネル（alive は実線の緑、stopped·error は破線の灰）。ノードクリックで onSelectHost を呼びます。
// アセットカバレッジ図の G6 d3-force モードを再利用します（動的 import で SSR/静的エクスポート時の window 依存を回避）。
export function TopologyGraph({
  data,
  loading,
  onRefresh,
  onSelectHost,
}: {
  data: IntranetTopology | null;
  loading: boolean;
  onRefresh: () => void;
  onSelectHost: (nodeId: string) => void;
}) {
  const containerRef = React.useRef<HTMLDivElement>(null);
  const graphRef = React.useRef<G6Graph | null>(null);
  const gDataRef = React.useRef<{ nodes: G6NodeDatum[]; edges: G6EdgeDatum[] }>({ nodes: [], edges: [] });
  const onSelectHostRef = React.useRef(onSelectHost);
  onSelectHostRef.current = onSelectHost;
  // グラフ構築コールバック（クロージャ）内で最新の segments 順を読み、セグメント色を解決して図と凡例の色を一致させます。
  const segmentsRef = React.useRef<IntranetSegment[]>([]);
  segmentsRef.current = data?.segments ?? [];

  // 構造シグネチャ：ノード・エッジ集合やその重要状態が変化した時だけデータを入れ替えてレイアウトを再実行し、ポーリング中の無駄な揺れを防ぎます。
  const sig = React.useMemo(() => {
    if (!data) return "";
    const ns = data.nodes
      .map((n) => `${n.id}:${n.has_session ? "s" : "-"}${n.alive_sessions}${n.is_callback ? "c" : ""}`)
      .sort()
      .join(",");
    const es = data.edges
      .map((e) => `${e.from}>${e.to}:${e.state}`)
      .sort()
      .join(",");
    return `${ns}|${es}`;
  }, [data]);

  gDataRef.current = data ? toG6(data) : { nodes: [], edges: [] };

  const applyData = React.useCallback(() => {
    const graph = graphRef.current;
    if (!graph || graph.destroyed) return;
    graph.setData(gDataRef.current);
    // render() は d3-force レイアウトを非同期で実行します。レイアウト完了前にコンポーネントがアンマウントされると、G6 はクリア済みの context で
    // transform にアクセスして例外を投げます（純粋な teardown 競合）。これは握り潰してよく、実際の描画エラーは引き続きログ出力します。
    void graph.render().catch((err) => {
      if (!graph.destroyed) console.error("[intranet-topology] render:", err);
    });
  }, []);

  // グラフ構築（1 回のみ）。
  React.useEffect(() => {
    let destroyed = false;
    let graph: G6Graph | null = null;
    void (async () => {
      const { Graph } = await import("@antv/g6");
      if (destroyed || !containerRef.current) return;
      graph = new Graph({
        container: containerRef.current,
        autoResize: true,
        autoFit: "view",
        background: "#f0f2f7",
        node: {
          style: {
            size: (d: unknown) => nd(d).size,
            fill: (d: unknown) => {
              const n = nd(d);
              return n.isCallback ? "#475569" : segmentColor(segmentsRef.current, n.segment);
            },
            stroke: (d: unknown) => (nd(d).hasSession ? "#059669" : "rgba(255,255,255,0.85)"),
            lineWidth: (d: unknown) => (nd(d).hasSession ? 3 : 1.5),
            iconSrc: (d: unknown) => (nd(d).isCallback ? CALLBACK_ICON : HOST_ICON),
            iconWidth: (d: unknown) => Math.max(12, nd(d).size * 0.55),
            iconHeight: (d: unknown) => Math.max(12, nd(d).size * 0.55),
            labelText: (d: unknown) => nd(d).lbl,
            labelFontSize: 10,
            labelPlacement: "bottom",
            labelFill: "#475569",
            labelBackground: true,
            labelBackgroundFill: "rgba(255,255,255,0.75)",
            labelBackgroundRadius: 3,
            labelPadding: [1, 3],
          },
        },
        edge: {
          style: {
            stroke: (d: unknown) => (ed(d).state === "alive" ? "#10b981" : "#94a3b8"),
            lineWidth: 1.6,
            lineDash: (d: unknown) => (ed(d).state === "alive" ? [0] : [5, 4]),
            endArrow: false,
          },
        },
        layout: {
          type: "d3-force",
          collide: { radius: (d: unknown) => nd(d).size + 10 },
          link: { distance: 110 },
          manyBody: { strength: (d: unknown) => (nd(d).isCallback ? -320 : -160) },
        },
        behaviors: ["drag-element-force", "drag-canvas", "zoom-canvas"],
      });
      graph.on("node:click", (evt: unknown) => {
        const id = (evt as { target?: { id?: string } }).target?.id;
        if (id) onSelectHostRef.current(id);
      });
      graphRef.current = graph;
      applyData();
    })();
    return () => {
      destroyed = true;
      // レイアウトを停止してから破棄し、「レイアウト実行中に context がクリアされる」競合の窓をできるだけ短くします。
      try {
        graph?.stopLayout();
      } catch {
        /* グラフ未構築、またはレイアウトコンテキストが既にない可能性 */
      }
      graph?.destroy();
      graphRef.current = null;
    };
  }, [applyData]);

  // データ変化 → データ再投入 + レイアウト。sig は再配置のトリガー専用（applyData は gDataRef を読む）。
  // biome-ignore lint/correctness/useExhaustiveDependencies: sig は意図的な再配置トリガーの依存
  React.useEffect(() => {
    applyData();
  }, [sig, applyData]);

  return (
    <div className="relative h-full w-full">
      <div ref={containerRef} className="h-full w-full" />
      <TopologyLegend data={data} loading={loading} onRefresh={onRefresh} />
    </div>
  );
}
