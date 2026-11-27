"use client";

import * as React from "react";

import { DownloadIcon } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { RadioGroup, RadioGroupItem } from "@/components/ui/radio-group";
import { api } from "@/lib/api";
import type { FindingQuery } from "@/lib/types";
import { cn } from "@/lib/utils";

interface Props {
  selectedIds: ReadonlySet<string>;
  filteredTotal: number;
  filters: Omit<FindingQuery, "page" | "pageSize">;
}

export function FindingExport({ selectedIds, filteredTotal, filters }: Props) {
  const [exportOpen, setExportOpen] = React.useState(false);
  const [exportScope, setExportScope] = React.useState<"filtered" | "all" | "selected">("filtered");
  const [exportFormat, setExportFormat] = React.useState<"md-single" | "md-zip" | "csv" | "json">("md-single");
  const [exporting, setExporting] = React.useState(false);

  function openExport() {
    setExportScope(selectedIds.size > 0 ? "selected" : "filtered");
    setExportOpen(true);
  }

  async function doExport() {
    setExporting(true);
    try {
      await api.exportFindings({ format: exportFormat, scope: exportScope, filters, ids: [...selectedIds] });
      setExportOpen(false);
      toast.success("エクスポートファイルのダウンロードを開始しました");
    } catch (error) {
      toast.error(`エクスポートに失敗しました：${(error as Error).message}`);
    } finally {
      setExporting(false);
    }
  }

  return (
    <>
      <div className="ml-auto flex items-center gap-3">
        {selectedIds.size > 0 && (
          <span className="text-xs text-muted-foreground tabular-nums">選択中 {selectedIds.size} 件</span>
        )}
        <Button size="sm" variant="outline" onClick={openExport}>
          <DownloadIcon /> エクスポート
        </Button>
      </div>
      <Dialog open={exportOpen} onOpenChange={setExportOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>ファインディングのエクスポート</DialogTitle>
            <DialogDescription>
              エクスポート範囲と形式を選択してください。生成後、ブラウザが自動でダウンロードします。
            </DialogDescription>
          </DialogHeader>

          <div className="flex flex-col gap-5 py-1">
            <div className="flex flex-col gap-2">
              <span className="text-xs text-muted-foreground">エクスポート範囲</span>
              <RadioGroup value={exportScope} onValueChange={(v) => setExportScope(v as typeof exportScope)}>
                <label htmlFor="export-scope-filtered" className="flex items-center gap-2 text-sm">
                  <RadioGroupItem id="export-scope-filtered" value="filtered" /> 現在の絞り込み結果をエクスポート（全{" "}
                  {filteredTotal} 件）
                </label>
                <label htmlFor="export-scope-all" className="flex items-center gap-2 text-sm">
                  <RadioGroupItem id="export-scope-all" value="all" /> すべてをエクスポート
                </label>
                <label
                  htmlFor="export-scope-selected"
                  className={cn("flex items-center gap-2 text-sm", selectedIds.size === 0 && "text-muted-foreground")}
                >
                  <RadioGroupItem id="export-scope-selected" value="selected" disabled={selectedIds.size === 0} />
                  選択した {selectedIds.size} 件をエクスポート
                </label>
              </RadioGroup>
            </div>

            <div className="flex flex-col gap-2">
              <span className="text-xs text-muted-foreground">エクスポート形式</span>
              <RadioGroup value={exportFormat} onValueChange={(v) => setExportFormat(v as typeof exportFormat)}>
                <label htmlFor="export-format-md-single" className="flex items-center gap-2 text-sm">
                  <RadioGroupItem id="export-format-md-single" value="md-single" /> Markdown サマリレポート（単一 .md
                  ファイル）
                </label>
                <label htmlFor="export-format-md-zip" className="flex items-center gap-2 text-sm">
                  <RadioGroupItem id="export-format-md-zip" value="md-zip" /> Markdown 分割ファイル（1 脆弱性 1
                  .md、.zip にまとめる）
                </label>
                <label htmlFor="export-format-csv" className="flex items-center gap-2 text-sm">
                  <RadioGroupItem id="export-format-csv" value="csv" /> CSV テーブル（.csv）
                </label>
                <label htmlFor="export-format-json" className="flex items-center gap-2 text-sm">
                  <RadioGroupItem id="export-format-json" value="json" /> JSON（.json）
                </label>
              </RadioGroup>
            </div>
          </div>

          <DialogFooter>
            <Button variant="outline" onClick={() => setExportOpen(false)} disabled={exporting}>
              キャンセル
            </Button>
            <Button onClick={doExport} disabled={exporting || (exportScope === "selected" && selectedIds.size === 0)}>
              <DownloadIcon /> {exporting ? "エクスポート中…" : "エクスポート"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  );
}
