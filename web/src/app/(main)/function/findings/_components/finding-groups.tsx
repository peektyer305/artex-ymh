"use client";

import type * as React from "react";

import Link from "next/link";

import { ArrowUpRightIcon, ChevronRightIcon } from "lucide-react";

import { StatusBadge } from "@/components/status-badge";
import { TablePagination } from "@/components/table-pagination";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Spinner } from "@/components/ui/spinner";
import type { Finding, FindingGroup } from "@/lib/types";
import { cn } from "@/lib/utils";

import { FindingsTable, fmtTime, SEVERITIES, UNASSIGNED_TASK } from "./findings-table";

export interface GroupFindingsState {
  items: Finding[];
  total: number;
  page: number;
  pageSize: number;
  loaded: boolean;
  loading: boolean;
}

export function findingGroupKey(group: FindingGroup) {
  return group.task_id === null ? UNASSIGNED_TASK : String(group.task_id);
}

function groupTitle(group: FindingGroup) {
  if (group.task_id === null) return "未関連 / タスク削除済み";
  if (group.task_name) return `${group.task_name}（タスク #${group.task_id}）`;
  return `タスク #${group.task_id}`;
}

interface Props {
  groups: FindingGroup[];
  expandedGroups: Set<string>;
  groupFindings: Record<string, GroupFindingsState>;
  toggleGroup: (key: string) => void;
  loadGroup: (key: string, page: number, pageSize: number) => void;
  rowProps: Omit<React.ComponentProps<typeof FindingsTable>, "items" | "selectAllLabel">;
  page: number;
  pageSize: number;
  groupTotal: number;
  setPage: (page: number) => void;
  setPageSize: (size: number) => void;
}

export function FindingGroups({
  groups,
  expandedGroups,
  groupFindings,
  toggleGroup,
  loadGroup,
  rowProps,
  page,
  pageSize,
  groupTotal,
  setPage,
  setPageSize,
}: Props) {
  return (
    <div className="flex flex-col gap-3">
      {groups.map((group) => {
        const key = findingGroupKey(group);
        const groupOpen = expandedGroups.has(key);
        const state = groupFindings[key] ?? {
          items: [],
          total: group.count,
          page: 1,
          pageSize: 10,
          loaded: false,
          loading: false,
        };
        return (
          <Card key={key} className="gap-0 py-0">
            <CardHeader className="px-4 py-3">
              <div className="flex min-w-0 flex-wrap items-center gap-3">
                <button
                  type="button"
                  className="flex min-w-0 flex-1 items-center gap-3 text-left"
                  aria-expanded={groupOpen}
                  onClick={() => toggleGroup(key)}
                >
                  <ChevronRightIcon
                    className={cn(
                      "size-4 shrink-0 text-muted-foreground transition-transform",
                      groupOpen && "rotate-90",
                    )}
                  />
                  <div className="flex min-w-0 flex-col gap-1">
                    <CardTitle className="truncate text-sm">
                      {groupTitle(group)}
                    </CardTitle>
                    <CardDescription className="truncate" title={group.task_description}>
                      {group.task_description || "ソースタスクは利用できません"}
                    </CardDescription>
                  </div>
                </button>
                <div className="flex flex-wrap items-center gap-2">
                  {group.task_status && <StatusBadge domain="task" value={group.task_status} dot />}
                  {SEVERITIES.map((level) => {
                    const count = group[level];
                    if (count === 0) return null;
                    return (
                      <span key={level} className="inline-flex items-center gap-1">
                        <StatusBadge domain="severity" value={level} dot />
                        <span className="text-xs tabular-nums text-muted-foreground">{count}</span>
                      </span>
                    );
                  })}
                  <span className="text-xs tabular-nums text-muted-foreground">{fmtTime(group.last_found_at)}</span>
                  {group.task_id !== null && (
                    <Button size="icon-sm" variant="ghost" asChild>
                      <Link
                        href={`/function/tasks/detail?id=${group.task_id}`}
                        aria-label={`タスク #${group.task_id} を表示`}
                      >
                        <ArrowUpRightIcon />
                      </Link>
                    </Button>
                  )}
                </div>
              </div>
            </CardHeader>
            {groupOpen && (
              <CardContent className="px-0">
                {state.loading && !state.loaded ? (
                  <div className="flex min-h-36 items-center justify-center">
                    <Spinner />
                  </div>
                ) : (
                  <>
                    <FindingsTable
                      items={state.items}
                      selectAllLabel="このグループの現在のページをすべて選択"
                      {...rowProps}
                    />
                    <TablePagination
                      page={state.page}
                      pageSize={state.pageSize}
                      total={state.total}
                      onPageChange={(nextPage) => void loadGroup(key, nextPage, state.pageSize)}
                      onPageSizeChange={(nextSize) => void loadGroup(key, 1, nextSize)}
                    />
                  </>
                )}
              </CardContent>
            )}
          </Card>
        );
      })}
      {groups.length === 0 && (
        <Card>
          <CardContent className="py-12 text-center text-sm text-muted-foreground">
            一致するファインディングがありません。
          </CardContent>
        </Card>
      )}
      <TablePagination
        page={page}
        pageSize={pageSize}
        total={groupTotal}
        onPageChange={setPage}
        onPageSizeChange={(nextPageSize) => {
          setPageSize(nextPageSize);
          setPage(1);
        }}
        pageSizeOptions={[5, 10, 20]}
      />
    </div>
  );
}
