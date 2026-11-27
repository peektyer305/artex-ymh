"use client";

import type * as React from "react";

import { KeyRoundIcon, Trash2Icon } from "lucide-react";

import { AssetDslSearch } from "@/components/asset-dsl-search";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { TableCell, TableRow } from "@/components/ui/table";
import { TabsContent } from "@/components/ui/tabs";
import { safeHref } from "@/lib/safe-href";
import type { Asset } from "@/lib/types";
import { cn } from "@/lib/utils";

import { AssetCard, Chips, HoneypotBadge, MethodBadge, statusTone } from "./asset-table";

interface Props {
  tab: string;
  rows: Asset[];
  rowsTab: string;
  total: number;
  loaded: boolean;
  loading: boolean;
  page: number;
  size: number;
  setPage: React.Dispatch<React.SetStateAction<number>>;
  setSize: React.Dispatch<React.SetStateAction<number>>;
  selected: Set<number>;
  toggleSelect: (id: number) => void;
  toggleSelectAll: (ids: number[]) => void;
  openDelete: (ids: number[]) => void;
  companyName: (id?: number) => string;
  query: string;
  setQuery: (query: string) => void;
  dslError: string;
}

export function AssetTabs({
  tab,
  rows,
  rowsTab,
  total,
  loaded,
  loading,
  page,
  size,
  setPage,
  setSize,
  selected,
  toggleSelect,
  toggleSelectAll,
  openDelete,
  companyName,
  query,
  setQuery,
  dslError,
}: Props) {
  // rows are already the current server-side page.
  const tabData = (type: string): Asset[] => (rowsTab === type ? rows : []);
  const currentRows = tabData(tab);
  const dslMode = query.trim() !== "";

  const searchBox = (
    <AssetDslSearch
      query={query}
      onChange={setQuery}
      loading={loading}
      error={dslError}
      count={dslMode ? total : undefined}
    />
  );

  return (
    <>
      {/* ルートドメイン */}
      <TabsContent value="root_domain" className="mt-0 flex min-h-0 flex-1 flex-col gap-2">
        {searchBox}
        <AssetCard
          cols={["", "ドメイン", "ICP 登録", "所属企業", ""]}
          loaded={loaded}
          total={total}
          page={page}
          size={size}
          onSize={setSize}
          onPage={setPage}
          rows={currentRows}
          selected={selected}
          onToggleAll={(ids) => toggleSelectAll(ids)}
        >
          {tabData("root_domain").map((a) => (
            <TableRow key={a.id} className={selected.has(a.id) ? "bg-muted/40" : undefined}>
              <TableCell className="w-8 pr-0">
                <Checkbox checked={selected.has(a.id)} onCheckedChange={() => toggleSelect(a.id)} />
              </TableCell>
              <TableCell className="font-mono text-xs font-medium">{a.domain}</TableCell>
              <TableCell className="text-xs">{a.icp || "—"}</TableCell>
              <TableCell className="text-xs">
                {companyName(a.company_id) || <span className="text-muted-foreground">未所属</span>}
              </TableCell>
              <TableCell className="w-8 pl-0">
                <Button
                  variant="ghost"
                  size="icon"
                  className="size-7 text-muted-foreground hover:text-destructive"
                  onClick={() => openDelete([a.id])}
                  aria-label={`アセット ${a.domain || a.id} を削除`}
                >
                  <Trash2Icon className="size-3.5" />
                </Button>
              </TableCell>
            </TableRow>
          ))}
        </AssetCard>
      </TabsContent>

      {/* IP */}
      <TabsContent value="ip" className="mt-0 flex min-h-0 flex-1 flex-col gap-2">
        {searchBox}
        <AssetCard
          cols={["", "IP", "Cセグメント", "紐付けドメイン", "開放ポート", ""]}
          loaded={loaded}
          total={total}
          page={page}
          size={size}
          onSize={setSize}
          onPage={setPage}
          rows={currentRows}
          selected={selected}
          onToggleAll={(ids) => toggleSelectAll(ids)}
        >
          {tabData("ip").map((a) => (
            <TableRow key={a.id} className={selected.has(a.id) ? "bg-muted/40" : undefined}>
              <TableCell className="w-8 pr-0">
                <Checkbox checked={selected.has(a.id)} onCheckedChange={() => toggleSelect(a.id)} />
              </TableCell>
              <TableCell className="font-mono text-xs font-medium">{a.ip}</TableCell>
              <TableCell className="font-mono text-xs">{a.c_segment || "—"}</TableCell>
              <TableCell>
                <Chips items={a.bound_domains ?? []} mono />
              </TableCell>
              <TableCell>
                <Chips
                  items={(a.open_ports ?? []).map((p) => (p.service ? `${p.port}/${p.service}` : String(p.port)))}
                  mono
                />
              </TableCell>
              <TableCell className="w-8 pl-0">
                <Button
                  variant="ghost"
                  size="icon"
                  className="size-7 text-muted-foreground hover:text-destructive"
                  onClick={() => openDelete([a.id])}
                  aria-label={`アセット ${a.ip || a.id} を削除`}
                >
                  <Trash2Icon className="size-3.5" />
                </Button>
              </TableCell>
            </TableRow>
          ))}
        </AssetCard>
      </TabsContent>

      {/* サブドメイン */}
      <TabsContent value="subdomain" className="mt-0 flex min-h-0 flex-1 flex-col gap-2">
        {searchBox}
        <AssetCard
          cols={["", "ドメイン", "ルートドメイン", "レコード種別", "レコード値", ""]}
          loaded={loaded}
          total={total}
          page={page}
          size={size}
          onSize={setSize}
          onPage={setPage}
          rows={currentRows}
          selected={selected}
          onToggleAll={(ids) => toggleSelectAll(ids)}
        >
          {tabData("subdomain").map((a) => (
            <TableRow key={a.id} className={selected.has(a.id) ? "bg-muted/40" : undefined}>
              <TableCell className="w-8 pr-0">
                <Checkbox checked={selected.has(a.id)} onCheckedChange={() => toggleSelect(a.id)} />
              </TableCell>
              <TableCell className="font-mono text-xs font-medium">{a.domain}</TableCell>
              <TableCell className="font-mono text-xs">{a.root_domain || "—"}</TableCell>
              <TableCell className="text-xs">{a.record_type || "—"}</TableCell>
              <TableCell className="max-w-xs truncate font-mono text-xs">
                {(Array.isArray(a.record_value) ? a.record_value.join(", ") : a.record_value) || "—"}
              </TableCell>
              <TableCell className="w-8 pl-0">
                <Button
                  variant="ghost"
                  size="icon"
                  className="size-7 text-muted-foreground hover:text-destructive"
                  onClick={() => openDelete([a.id])}
                  aria-label={`アセット ${a.domain || a.id} を削除`}
                >
                  <Trash2Icon className="size-3.5" />
                </Button>
              </TableCell>
            </TableRow>
          ))}
        </AssetCard>
      </TabsContent>

      {/* アプリケーション */}
      <TabsContent value="app" className="mt-0 flex min-h-0 flex-1 flex-col gap-2">
        {searchBox}
        <AssetCard
          cols={["", "アプリ名", "Bundle ID", "カテゴリ", "ICP 登録", ""]}
          loaded={loaded}
          total={total}
          page={page}
          size={size}
          onSize={setSize}
          onPage={setPage}
          rows={currentRows}
          selected={selected}
          onToggleAll={(ids) => toggleSelectAll(ids)}
        >
          {tabData("app").map((a) => (
            <TableRow key={a.id} className={selected.has(a.id) ? "bg-muted/40" : undefined}>
              <TableCell className="w-8 pr-0">
                <Checkbox checked={selected.has(a.id)} onCheckedChange={() => toggleSelect(a.id)} />
              </TableCell>
              <TableCell className="text-xs font-medium">{a.app_name || "—"}</TableCell>
              <TableCell className="font-mono text-xs">{a.bundle_id || "—"}</TableCell>
              <TableCell className="text-xs">{a.category || "—"}</TableCell>
              <TableCell className="text-xs">{a.app_icp || "—"}</TableCell>
              <TableCell className="w-8 pl-0">
                <Button
                  variant="ghost"
                  size="icon"
                  className="size-7 text-muted-foreground hover:text-destructive"
                  onClick={() => openDelete([a.id])}
                  aria-label={`アセット ${a.app_name || a.id} を削除`}
                >
                  <Trash2Icon className="size-3.5" />
                </Button>
              </TableCell>
            </TableRow>
          ))}
        </AssetCard>
      </TabsContent>

      {/* サービス */}
      <TabsContent value="service" className="mt-0 flex min-h-0 flex-1 flex-col gap-2">
        {searchBox}
        <AssetCard
          cols={[
            "",
            "サービス",
            "ドメイン",
            "IP",
            "ポート",
            "ステータスコード",
            "タイトル",
            "フィンガープリント",
            "ハニーポット",
            "認証",
            "",
          ]}
          loaded={loaded}
          total={total}
          page={page}
          size={size}
          onSize={setSize}
          onPage={setPage}
          rows={currentRows}
          selected={selected}
          onToggleAll={(ids) => toggleSelectAll(ids)}
        >
          {tabData("service").map((a) => {
            const isHttp = a.service_type === "http";
            const svc = a.service_name || (isHttp ? "http" : "") || a.service_type || "";
            // a.url は対象側のデータなので、http(s) のみ href を許可する。安全でない場合はプレーンテキスト表示に落とす。
            const href = isHttp ? safeHref(a.url) : undefined;
            let domainCell: React.ReactNode = "—";
            if (href) {
              domainCell = (
                <a
                  href={href}
                  target="_blank"
                  rel="noreferrer"
                  className="text-blue-600 hover:underline dark:text-blue-400"
                >
                  {a.domain || a.url}
                </a>
              );
            } else if (a.domain || a.url) {
              domainCell = a.domain || a.url;
            }
            return (
              <TableRow key={a.id} className={selected.has(a.id) ? "bg-muted/40" : undefined}>
                <TableCell className="w-8 pr-0">
                  <Checkbox checked={selected.has(a.id)} onCheckedChange={() => toggleSelect(a.id)} />
                </TableCell>
                <TableCell className="text-xs">
                  <Badge variant={isHttp ? "default" : "secondary"} className="font-mono text-[10px]">
                    {svc || "—"}
                  </Badge>
                </TableCell>
                <TableCell className="max-w-[14rem] truncate font-mono text-xs" title={a.domain || a.url}>
                  {domainCell}
                </TableCell>
                <TableCell className="font-mono text-xs">{a.ip || "—"}</TableCell>
                <TableCell className="font-mono text-xs tabular-nums">{a.port || "—"}</TableCell>
                <TableCell>
                  {a.status_code != null ? (
                    <span className={cn("font-mono text-xs font-semibold tabular-nums", statusTone(a.status_code))}>
                      {a.status_code}
                    </span>
                  ) : (
                    "—"
                  )}
                </TableCell>
                <TableCell className="max-w-[12rem] truncate text-xs" title={a.page_title}>
                  {a.page_title || "—"}
                </TableCell>
                <TableCell>
                  <Chips items={a.technologies ?? []} />
                </TableCell>
                <TableCell>
                  <HoneypotBadge score={a.honeypot_score} evidence={a.honeypot_evidence} />
                </TableCell>
                <TableCell>
                  {(a.auth ?? []).length === 0 ? (
                    <span className="text-xs text-muted-foreground">—</span>
                  ) : (
                    (a.auth ?? []).map((authItem, i) => {
                      const item = authItem as Record<string, string>;
                      const label = item.type || item.username || "認証";
                      return (
                        <span key={i} className="inline-flex items-center gap-1 text-[11px]">
                          <KeyRoundIcon className="size-3 text-muted-foreground" />
                          <span className="font-mono">{label}</span>
                        </span>
                      );
                    })
                  )}
                </TableCell>
                <TableCell className="w-8 pl-0">
                  <Button
                    variant="ghost"
                    size="icon"
                    className="size-7 text-muted-foreground hover:text-destructive"
                    onClick={() => openDelete([a.id])}
                    aria-label={`アセット ${a.url || a.id} を削除`}
                  >
                    <Trash2Icon className="size-3.5" />
                  </Button>
                </TableCell>
              </TableRow>
            );
          })}
        </AssetCard>
      </TabsContent>

      {/* API */}
      <TabsContent value="endpoint" className="mt-0 flex min-h-0 flex-1 flex-col gap-2">
        {searchBox}
        <AssetCard
          cols={["", "メソッド", "完全なURL", "パラメータ", ""]}
          loaded={loaded}
          total={total}
          page={page}
          size={size}
          onSize={setSize}
          onPage={setPage}
          rows={currentRows}
          selected={selected}
          onToggleAll={(ids) => toggleSelectAll(ids)}
        >
          {tabData("endpoint").map((a) => (
            <TableRow key={a.id} className={selected.has(a.id) ? "bg-muted/40" : undefined}>
              <TableCell className="w-8 pr-0">
                <Checkbox checked={selected.has(a.id)} onCheckedChange={() => toggleSelect(a.id)} />
              </TableCell>
              <TableCell className="w-16">
                <MethodBadge method={a.method || ""} />
              </TableCell>
              <TableCell className="max-w-sm truncate font-mono text-xs" title={a.url}>
                {a.url || "—"}
              </TableCell>
              <TableCell>
                <Chips
                  items={(a.params ?? []).map((p) => {
                    const item = p as Record<string, string>;
                    return item.name ? `${item.name}(${item.location || "?"})` : "?";
                  })}
                  mono
                />
              </TableCell>
              <TableCell className="w-8 pl-0">
                <Button
                  variant="ghost"
                  size="icon"
                  className="size-7 text-muted-foreground hover:text-destructive"
                  onClick={() => openDelete([a.id])}
                  aria-label={`アセット ${a.url || a.id} を削除`}
                >
                  <Trash2Icon className="size-3.5" />
                </Button>
              </TableCell>
            </TableRow>
          ))}
        </AssetCard>
      </TabsContent>
    </>
  );
}
