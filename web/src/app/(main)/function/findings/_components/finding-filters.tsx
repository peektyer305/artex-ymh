"use client";

import { BugIcon, SearchIcon } from "lucide-react";

import { InputGroup, InputGroupAddon, InputGroupInput } from "@/components/ui/input-group";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group";
import { statusMeta } from "@/lib/status";
import type { FindingStats, FindingStatus, Severity } from "@/lib/types";

import { FindingExport } from "./finding-export";
import { FINDING_STATUSES, UNASSIGNED_TASK } from "./findings-table";

interface Props {
  search: string;
  setSearch: (value: string) => void;
  severity: "all" | Severity;
  setSeverity: (value: "all" | Severity) => void;
  status: "all" | FindingStatus;
  setStatus: (value: "all" | FindingStatus) => void;
  vulnclass: string;
  setVulnclass: (value: string) => void;
  task: string;
  setTask: (value: string) => void;
  sort: "severity" | "time";
  setSort: (value: "severity" | "time") => void;
  query: string;
  stats: FindingStats;
  selectedIds: ReadonlySet<string>;
  filteredTotal: number;
}

export function FindingFilters({
  search,
  setSearch,
  severity,
  setSeverity,
  status,
  setStatus,
  vulnclass,
  setVulnclass,
  task,
  setTask,
  sort,
  setSort,
  query,
  stats,
  selectedIds,
  filteredTotal,
}: Props) {
  return (
    <div className="flex flex-wrap items-center gap-2">
      <InputGroup className="w-full sm:w-72">
        <InputGroupInput
          type="search"
          value={search}
          onChange={(event) => setSearch(event.target.value)}
          placeholder="脆弱性の内容を検索"
          aria-label="脆弱性の内容を検索"
        />
        <InputGroupAddon>
          <SearchIcon aria-hidden="true" />
        </InputGroupAddon>
      </InputGroup>

      <ToggleGroup
        type="single"
        value={severity}
        onValueChange={(value) => value && setSeverity(value as "all" | Severity)}
        variant="outline"
        size="sm"
        spacing={0}
      >
        {(
          [
            ["all", "すべて"],
            ["critical", "緊急"],
            ["high", "高"],
            ["medium", "中"],
            ["low", "低"],
          ] as const
        ).map(([val, label]) => (
          <ToggleGroupItem key={val} value={val} aria-label={`${label}で絞り込み`}>
            {label}
          </ToggleGroupItem>
        ))}
      </ToggleGroup>

      <Select value={status} onValueChange={(v) => setStatus(v as "all" | FindingStatus)}>
        <SelectTrigger size="sm" className="w-32">
          <SelectValue placeholder="ステータス" />
        </SelectTrigger>
        <SelectContent>
          <SelectItem value="all">すべてのステータス</SelectItem>
          {FINDING_STATUSES.map((st) => (
            <SelectItem key={st} value={st}>
              {statusMeta("finding", st).label}
            </SelectItem>
          ))}
        </SelectContent>
      </Select>

      <Select value={vulnclass} onValueChange={setVulnclass}>
        <SelectTrigger size="sm" className="w-40">
          <SelectValue placeholder="脆弱性タイプ" />
        </SelectTrigger>
        <SelectContent>
          <SelectItem value="all">すべてのタイプ</SelectItem>
          {stats.vulnclasses.map((vc) => (
            <SelectItem key={vc} value={vc}>
              {vc}
            </SelectItem>
          ))}
        </SelectContent>
      </Select>

      <Select value={task} onValueChange={setTask}>
        <SelectTrigger size="sm" className="w-48">
          <SelectValue placeholder="タスク" />
        </SelectTrigger>
        <SelectContent>
          <SelectItem value="all">すべてのタスク</SelectItem>
          <SelectItem value={UNASSIGNED_TASK}>未関連 / タスク削除済み</SelectItem>
          {(stats.tasks ?? []).map((t) => {
            const id = String(t.id);
            const label = t.name || t.description || `タスク #${id}（削除済み）`;
            return (
              <SelectItem key={id} value={id}>
                <span className="flex w-full items-center gap-2">
                  <span className="max-w-[14rem] truncate" title={label}>
                    {label}
                  </span>
                  <span className="inline-flex items-center gap-1 text-muted-foreground tabular-nums">
                    <BugIcon className="size-3.5" aria-hidden="true" />
                    {t.count}
                  </span>
                </span>
              </SelectItem>
            );
          })}
        </SelectContent>
      </Select>

      <Select value={sort} onValueChange={(v) => setSort(v as "severity" | "time")}>
        <SelectTrigger size="sm" className="w-36">
          <SelectValue />
        </SelectTrigger>
        <SelectContent>
          <SelectItem value="severity">深刻度順</SelectItem>
          <SelectItem value="time">日時順</SelectItem>
        </SelectContent>
      </Select>

      <FindingExport
        selectedIds={selectedIds}
        filteredTotal={filteredTotal}
        filters={{ severity, status, vulnclass, task, query, sort }}
      />
    </div>
  );
}
