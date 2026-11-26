"use client";

import * as React from "react";
import Link from "next/link";
import { ArrowDownIcon, ArrowUpDownIcon, ArrowUpIcon, Loader2Icon, PauseIcon, PencilIcon, PinIcon, PinOffIcon, PlayIcon } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Spinner } from "@/components/ui/spinner";
import { TableHead } from "@/components/ui/table";
import { type SortDirection } from "@/lib/sort-preference";
import type { Task } from "@/lib/types";
import { cn } from "@/lib/utils";
import { TaskSortField, taskControlAction, taskIsPinned } from "./task-utils";

export function taskSortIcon(active: boolean, direction: SortDirection) {
  if (!active) {
    return <ArrowUpDownIcon className="size-3.5 opacity-40 transition-opacity group-hover/sort:opacity-100" />;
  }
  if (direction === "asc") return <ArrowUpIcon className="size-3.5" />;
  return <ArrowDownIcon className="size-3.5" />;
}

export function SortableTaskHead({
  field,
  label,
  activeField,
  direction,
  align = "left",
  className,
  onSort,
}: {
  field: TaskSortField;
  label: string;
  activeField: TaskSortField;
  direction: SortDirection;
  align?: "left" | "right";
  className?: string;
  onSort: (field: TaskSortField) => void;
}) {
  const active = activeField === field;
  let ariaSort: React.AriaAttributes["aria-sort"] = "none";
  if (active) ariaSort = direction === "asc" ? "ascending" : "descending";

  let actionLabel = `${label}で降順に並び替え`;
  if (active) actionLabel = `${label}は現在${direction === "asc" ? "昇順" : "降順"}、クリックで並び替え方向を切り替え`;

  return (
    <TableHead className={className} aria-sort={ariaSort}>
      <button
        type="button"
        className={cn(
          "group/sort inline-flex h-full w-full items-center gap-1 outline-none focus-visible:underline",
          align === "right" && "justify-end",
        )}
        aria-label={actionLabel}
        onClick={() => onSort(field)}
      >
        <span>{label}</span>
        {taskSortIcon(active, direction)}
      </button>
    </TableHead>
  );
}

export function TaskNameEditor({ task, onRename }: { task: Task; onRename: (task: Task, name: string) => Promise<void> }) {
  const [editing, setEditing] = React.useState(false);
  const [name, setName] = React.useState(task.name ?? "");
  const [saving, setSaving] = React.useState(false);
  const inputRef = React.useRef<HTMLInputElement>(null);
  const cancelOnBlurRef = React.useRef(false);

  React.useEffect(() => {
    if (!editing) setName(task.name ?? "");
  }, [editing, task.name]);

  async function finishEditing() {
    if (cancelOnBlurRef.current) {
      cancelOnBlurRef.current = false;
      setName(task.name ?? "");
      setEditing(false);
      return;
    }

    const next = name.trim();
    if (!next || next === task.name?.trim()) {
      setName(task.name ?? "");
      setEditing(false);
      return;
    }

    setSaving(true);
    try {
      await onRename(task, next);
      setEditing(false);
    } catch {
      requestAnimationFrame(() => inputRef.current?.focus());
    } finally {
      setSaving(false);
    }
  }

  if (editing) {
    return (
      <div className="flex min-w-0 items-center gap-1">
        <Input
          ref={inputRef}
          value={name}
          maxLength={200}
          autoFocus
          disabled={saving}
          aria-label={`タスク #${task.id} の名前`}
          className="h-7 min-w-28 max-w-48 px-2 font-medium"
          onFocus={(event) => event.currentTarget.select()}
          onChange={(event) => setName(event.target.value)}
          onBlur={() => void finishEditing()}
          onKeyDown={(event) => {
            if (event.key === "Enter") {
              event.preventDefault();
              event.currentTarget.blur();
            } else if (event.key === "Escape") {
              event.preventDefault();
              cancelOnBlurRef.current = true;
              event.currentTarget.blur();
            }
          }}
        />
        {saving && <Spinner className="shrink-0" />}
      </div>
    );
  }

  return (
    <div className="flex min-w-0 items-center gap-1">
      <Link
        href={`/function/tasks/detail?id=${encodeURIComponent(task.id)}`}
        className="min-w-0 truncate rounded-sm hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
        title={task.name?.trim() ? task.name : task.description}
      >
        {task.name?.trim() ? task.name : <span className="text-muted-foreground">名称未設定</span>}
      </Link>
      <Button
        type="button"
        variant="ghost"
        size="icon-xs"
        className="shrink-0 opacity-100 transition-opacity [@media(hover:hover)]:opacity-0 [@media(hover:hover)]:group-focus-within:opacity-100 [@media(hover:hover)]:group-hover:opacity-100"
        aria-label={`タスク #${task.id} の名前を変更`}
        title="名前を変更"
        onClick={() => {
          setName(task.name ?? "");
          setEditing(true);
        }}
      >
        <PencilIcon />
      </Button>
    </div>
  );
}

export function taskPinIcon(pinning: boolean, pinned: boolean) {
  if (pinning) return <Spinner />;
  if (pinned) return <PinOffIcon />;
  return <PinIcon />;
}

export function TaskPinAction({ task, onTogglePinned }: { task: Task; onTogglePinned: (task: Task) => Promise<void> }) {
  const [pinning, setPinning] = React.useState(false);
  const pinned = taskIsPinned(task);

  return (
    <Button
      type="button"
      variant="ghost"
      size="icon"
      disabled={pinning}
      aria-label={pinned ? `タスク #${task.id} のピン留めを解除` : `タスク #${task.id} をピン留め`}
      title={pinned ? "ピン留め解除" : "ピン留め"}
      onClick={async () => {
        setPinning(true);
        try {
          await onTogglePinned(task);
        } finally {
          setPinning(false);
        }
      }}
    >
      {taskPinIcon(pinning, pinned)}
    </Button>
  );
}

// 3 状態のアイコンは分岐で書き、ネストした三項演算子は使わない。リポジトリの Biome ベースラインが noNestedTernary を禁止しているため。
export function taskControlIcon(pending: boolean, action: "pause" | "resume" | null) {
  if (pending) return <Loader2Icon className="animate-spin" />;
  if (action === "resume") return <PlayIcon />;
  return <PauseIcon />;
}

// TaskControlButton は行内の一時停止/再開スイッチ。終端状態のタスクは非表示ではなく disabled で描画し、
// 各行の操作列の幅を揃えて、ボタン位置が状態によってずれないようにする。
export function TaskControlButton({
  task,
  onControl,
}: {
  task: Task;
  onControl: (id: string, action: "pause" | "resume") => Promise<void>;
}) {
  const [pending, setPending] = React.useState(false);
  const action = taskControlAction(task.status);
  const label = action === "resume" ? "タスクを再開" : "タスクを一時停止";
  return (
    <Button
      size="icon"
      variant="ghost"
      aria-label={label}
      title={action ? label : "このステータスでは一時停止も再開もできません"}
      disabled={!action || pending}
      onClick={async () => {
        if (!action) return;
        setPending(true);
        try {
          await onControl(task.id, action);
        } finally {
          setPending(false);
        }
      }}
    >
      {taskControlIcon(pending, action)}
    </Button>
  );
}
