"use client";

import * as React from "react";
import Link from "next/link";
import { DndContext, type DragEndEvent, DragOverlay, type DragStartEvent, KeyboardSensor, PointerSensor, useDraggable, useDroppable, useSensor, useSensors } from "@dnd-kit/core";
import { FolderInputIcon, GripVerticalIcon, PlusIcon, SaveIcon, TagsIcon, Trash2Icon } from "lucide-react";
import { toast } from "sonner";
import { StatusBadge } from "@/components/status-badge";
import { AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent, AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle } from "@/components/ui/alert-dialog";
import { Button } from "@/components/ui/button";
import { Combobox, ComboboxChip, ComboboxChips, ComboboxChipsInput, ComboboxContent, ComboboxItem, ComboboxList, ComboboxValue } from "@/components/ui/combobox";
import { Dialog, DialogClose, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle, DialogTrigger } from "@/components/ui/dialog";
import { Empty, EmptyDescription, EmptyHeader, EmptyTitle } from "@/components/ui/empty";
import { Field, FieldDescription, FieldGroup, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { Item, ItemActions, ItemContent, ItemDescription, ItemGroup, ItemMedia, ItemTitle } from "@/components/ui/item";
import { ScrollArea } from "@/components/ui/scroll-area";
import { Select, SelectContent, SelectGroup, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Sheet, SheetContent, SheetDescription, SheetFooter, SheetHeader, SheetTitle, SheetTrigger } from "@/components/ui/sheet";
import { Spinner } from "@/components/ui/spinner";
import { api } from "@/lib/api";
import type { Task, TaskCategory } from "@/lib/types";
import { cn } from "@/lib/utils";
import { CategoryManagementView, UNCATEGORIZED_VALUE } from "./task-utils";

// BulkDeleteTasksDialog deletes every checked task with one shared set of cleanup options.
// The backend has no batch endpoint, so deletion runs one task at a time (see deleteTasks)
// and the button shows live progress.
// MoveTasksCategoryDialog confirms a target before applying, so a mis-click on a
// large selection cannot silently re-file every task. UNCATEGORIZED_VALUE stands
// in for "no category" because Select rejects an empty string value.
export function MoveTasksCategoryDialog({
  categories,
  count,
  moving,
  onMove,
}: {
  categories: TaskCategory[];
  count: number;
  moving: boolean;
  onMove: (categoryID?: number) => Promise<void>;
}) {
  const [open, setOpen] = React.useState(false);
  const [target, setTarget] = React.useState(UNCATEGORIZED_VALUE);

  const handleOpenChange = (next: boolean) => {
    if (moving) return;
    setOpen(next);
    if (next) setTarget(UNCATEGORIZED_VALUE);
  };

  return (
    <Dialog open={open} onOpenChange={handleOpenChange}>
      <DialogTrigger asChild>
        <Button size="sm" variant="outline">
          <FolderInputIcon data-icon="inline-start" /> カテゴリを変更
        </Button>
      </DialogTrigger>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>選択した {count} 件のタスクのカテゴリを変更</DialogTitle>
          <DialogDescription>変更先のカテゴリは選択したタスクすべてに適用されます。「未分類」を選ぶと現在のカテゴリから外れます。</DialogDescription>
        </DialogHeader>
        <Field>
          <FieldLabel htmlFor="bulk-category">変更先のカテゴリ</FieldLabel>
          <Select value={target} onValueChange={setTarget} disabled={moving}>
            <SelectTrigger id="bulk-category" className="w-full">
              <SelectValue placeholder="カテゴリを選択" />
            </SelectTrigger>
            <SelectContent>
              <SelectGroup>
                <SelectItem value={UNCATEGORIZED_VALUE}>未分類</SelectItem>
                {categories.map((category) => (
                  <SelectItem key={category.id} value={String(category.id)}>
                    {category.name}
                  </SelectItem>
                ))}
              </SelectGroup>
            </SelectContent>
          </Select>
          {categories.length === 0 && <FieldDescription>カテゴリがまだありません。「カテゴリ管理」から作成してください。</FieldDescription>}
        </Field>
        <DialogFooter>
          <DialogClose asChild>
            <Button variant="outline" disabled={moving}>
              キャンセル
            </Button>
          </DialogClose>
          <Button
            disabled={moving}
            onClick={() => {
              void onMove(target === UNCATEGORIZED_VALUE ? undefined : Number(target)).then(() => setOpen(false));
            }}
          >
            {moving && <Spinner data-icon="inline-start" />}
            移動
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

// CategoryPicker は新規タスク内の単一選択カテゴリ選択器：既存カテゴリを検索でき、ライブラリにない
// 名前を入力して Enter（またはドロップダウンの「作成」）で即時に作成して選択する。選択項目は
// 削除可能な tag で表示する。カテゴリはグローバル資源で、ここでの即時作成は「カテゴリ管理」での
// 手動作成と等価。カテゴリは 1 つだけ選べる。
export function CategoryPicker({
  categories,
  value,
  onValueChange,
  onCategoryCreated,
  portalContainer,
}: {
  categories: TaskCategory[];
  value?: number;
  onValueChange: (categoryID?: number) => void;
  onCategoryCreated: () => void;
  portalContainer?: React.RefObject<HTMLElement | null>;
}) {
  const [inputValue, setInputValue] = React.useState("");
  const [creating, setCreating] = React.useState(false);
  // 新規作成したカテゴリは親が再取得するまで categories に反映されないため、ローカルに控えを
  // 持っておき、その間 chip やドロップダウンが「不明なカテゴリ」と表示されるのを防ぐ。
  const [localExtra, setLocalExtra] = React.useState<TaskCategory[]>([]);

  const allCategories = React.useMemo(() => {
    const byID = new Map<number, TaskCategory>();
    for (const category of categories) byID.set(category.id, category);
    for (const category of localExtra) if (!byID.has(category.id)) byID.set(category.id, category);
    return [...byID.values()];
  }, [categories, localExtra]);

  const byID = React.useMemo(() => new Map(allCategories.map((c) => [String(c.id), c])), [allCategories]);
  const categoryIDs = React.useMemo(() => allCategories.map((c) => String(c.id)), [allCategories]);
  const selectedIDs = value != null ? [String(value)] : [];

  const trimmed = inputValue.trim();
  const lower = trimmed.toLowerCase();
  // base-ui のデフォルトの部分一致フィルタに合わせ、「該当するカテゴリがあるか」を判定する。
  const matchCount = trimmed
    ? allCategories.filter((c) => c.name.toLowerCase().includes(lower)).length
    : allCategories.length;

  const createAndSelect = async () => {
    if (!trimmed || creating) return;
    // 完全同名が既にあればそのまま選択し、重複作成しない。
    const existing = allCategories.find((c) => c.name.toLowerCase() === lower);
    if (existing) {
      onValueChange(existing.id);
      setInputValue("");
      return;
    }
    setCreating(true);
    try {
      const created = await api.createTaskCategory(trimmed);
      setLocalExtra((prev) => [...prev, created]);
      onValueChange(created.id);
      setInputValue("");
      onCategoryCreated();
      toast.success(`カテゴリ「${created.name}」を作成しました`);
    } catch (e) {
      toast.error(`カテゴリの作成に失敗しました：${(e as Error).message}`);
    } finally {
      setCreating(false);
    }
  };

  return (
    <Combobox
      items={categoryIDs}
      itemToStringValue={(id) => byID.get(id)?.name ?? id}
      multiple
      value={selectedIDs}
      onValueChange={(next: string[]) => {
        // 単一選択：最後に選ばれた 1 件を採用し、chip を削除（クリア）すると未分類に戻る。
        const last = next[next.length - 1];
        onValueChange(last ? Number(last) : undefined);
        setInputValue("");
      }}
      inputValue={inputValue}
      onInputValueChange={setInputValue}
    >
      <ComboboxChips>
        <ComboboxValue>
          {selectedIDs.map((id) => (
            <ComboboxChip key={id}>{byID.get(id)?.name ?? "不明なカテゴリ"}</ComboboxChip>
          ))}
        </ComboboxValue>
        <ComboboxChipsInput
          id="task-category"
          placeholder={selectedIDs.length ? "" : "カテゴリを検索、または新しい名前を入力して Enter で作成"}
          onKeyDown={(e) => {
            // 一致項目がまったく無い時の Enter = 作成。一致項目があれば base-ui の「Enter でハイライト項目を選択」を維持する。
            if (e.key === "Enter" && matchCount === 0 && trimmed) {
              e.preventDefault();
              void createAndSelect();
            }
          }}
        />
      </ComboboxChips>
      <ComboboxContent portalContainer={portalContainer}>
        <ComboboxList>
          {(id: string) => (
            <ComboboxItem key={id} value={id}>
              {byID.get(id)?.name ?? id}
            </ComboboxItem>
          )}
        </ComboboxList>
        {matchCount === 0 &&
          (trimmed ? (
            <button
              type="button"
              disabled={creating}
              onClick={() => void createAndSelect()}
              className="flex w-full items-center gap-2 px-2 py-2 text-left text-sm hover:bg-accent hover:text-accent-foreground disabled:opacity-50"
            >
              {creating ? <Spinner className="size-4" /> : <PlusIcon className="size-4" />}
              カテゴリ「{trimmed}」を作成
            </button>
          ) : (
            <div className="px-2 py-2 text-sm text-muted-foreground">名前を入力してカテゴリを検索または作成</div>
          ))}
      </ComboboxContent>
    </Combobox>
  );
}

export function CategoryDropTarget({
  value,
  name,
  count,
  selected,
  disabled,
  onSelect,
}: {
  value: string;
  name: string;
  count: number;
  selected: boolean;
  disabled: boolean;
  onSelect: () => void;
}) {
  const { isOver, setNodeRef } = useDroppable({ id: `category:${value}`, disabled });

  return (
    <button
      ref={setNodeRef}
      type="button"
      className={cn(
        "min-w-0 rounded-md border border-transparent px-2.5 py-2 text-left transition-colors",
        selected ? "bg-accent text-accent-foreground" : "hover:bg-accent/50",
        isOver && "border-primary bg-primary/10 text-foreground",
      )}
      onClick={onSelect}
    >
      <span className="block truncate font-medium text-sm">{name}</span>
      <span className="block truncate text-muted-foreground text-xs">{isOver ? "離して移動" : `${count} 件のタスク`}</span>
    </button>
  );
}

export function DraggableCategoryTask({ task, disabled, moving }: { task: Task; disabled: boolean; moving: boolean }) {
  const { attributes, isDragging, listeners, setNodeRef } = useDraggable({
    id: `task:${task.id}`,
    disabled,
  });

  return (
    <Item ref={setNodeRef} variant="outline" size="sm" className={cn(isDragging && "opacity-40")}>
      <ItemMedia className="group-has-data-[slot=item-description]/item:self-center group-has-data-[slot=item-description]/item:translate-y-0">
        {moving ? (
          <Spinner />
        ) : (
          <Button
            type="button"
            size="icon-xs"
            variant="ghost"
            className="touch-none cursor-grab active:cursor-grabbing"
            disabled={disabled}
            {...listeners}
            {...attributes}
            aria-label={`タスク #${task.id} をドラッグ`}
            title="タスクをドラッグ"
          >
            <GripVerticalIcon />
          </Button>
        )}
      </ItemMedia>
      <ItemContent className="min-w-0">
        <ItemTitle className="w-full min-w-0">
          <Link href={`/function/tasks/detail?id=${encodeURIComponent(task.id)}`} className="truncate hover:underline">
            {task.name?.trim() || task.description || `タスク #${task.id}`}
          </Link>
        </ItemTitle>
        <ItemDescription className="line-clamp-1">
          #{task.id} · {task.description}
        </ItemDescription>
      </ItemContent>
      <ItemActions>
        <StatusBadge domain="task" value={task.status} />
      </ItemActions>
    </Item>
  );
}

export function CategoryTaskDragPreview({ task }: { task: Task }) {
  return (
    <Item variant="outline" size="sm" className="w-80 bg-background shadow-lg">
      <ItemMedia className="group-has-data-[slot=item-description]/item:self-center group-has-data-[slot=item-description]/item:translate-y-0">
        <GripVerticalIcon className="size-4 text-muted-foreground" />
      </ItemMedia>
      <ItemContent className="min-w-0">
        <ItemTitle className="w-full min-w-0 truncate">
          {task.name?.trim() || task.description || `タスク #${task.id}`}
        </ItemTitle>
        <ItemDescription className="line-clamp-1">#{task.id}</ItemDescription>
      </ItemContent>
    </Item>
  );
}

export function CategoryManagementSheet({
  categories,
  tasks,
  onChanged,
  onTaskMoved,
}: {
  categories: TaskCategory[];
  tasks: Task[];
  onChanged: () => void;
  onTaskMoved: (taskID: string, category: TaskCategory | null) => void;
}) {
  const [open, setOpen] = React.useState(false);
  const [selectedView, setSelectedView] = React.useState<CategoryManagementView>("new");
  const [draftName, setDraftName] = React.useState("");
  const [saving, setSaving] = React.useState(false);
  const [deleteOpen, setDeleteOpen] = React.useState(false);
  const [deleting, setDeleting] = React.useState(false);
  const [movingTaskID, setMovingTaskID] = React.useState<string | null>(null);
  const [activeTaskID, setActiveTaskID] = React.useState<string | null>(null);
  const wasOpen = React.useRef(false);
  const sensors = useSensors(
    useSensor(PointerSensor, { activationConstraint: { distance: 6 } }),
    useSensor(KeyboardSensor),
  );

  React.useEffect(() => {
    if (open && !wasOpen.current) {
      const first = categories[0];
      if (first) {
        setSelectedView(first.id);
        setDraftName(first.name);
      } else if (tasks.some((task) => task.category_id == null)) {
        setSelectedView("uncategorized");
        setDraftName("");
      } else {
        setSelectedView("new");
        setDraftName("");
      }
    }
    wasOpen.current = open;
  }, [categories, open, tasks]);

  const selectedCategory = React.useMemo(
    () =>
      typeof selectedView === "number" ? (categories.find((category) => category.id === selectedView) ?? null) : null,
    [categories, selectedView],
  );

  const uncategorizedCount = React.useMemo(() => tasks.filter((task) => task.category_id == null).length, [tasks]);

  const visibleTasks = React.useMemo(() => {
    if (selectedView === "uncategorized") return tasks.filter((task) => task.category_id == null);
    if (typeof selectedView === "number") return tasks.filter((task) => task.category_id === selectedView);
    return [];
  }, [selectedView, tasks]);

  const activeTask = React.useMemo(() => tasks.find((task) => task.id === activeTaskID) ?? null, [activeTaskID, tasks]);

  const selectCategory = (category: TaskCategory) => {
    setSelectedView(category.id);
    setDraftName(category.name);
  };

  const selectUncategorized = () => {
    setSelectedView("uncategorized");
    setDraftName("");
  };

  const startNew = () => {
    setSelectedView("new");
    setDraftName("");
  };

  async function saveCategory() {
    const name = draftName.trim();
    if (!name || saving || selectedView === "uncategorized") return;
    setSaving(true);
    try {
      if (selectedView === "new") {
        const created = await api.createTaskCategory(name);
        setSelectedView(created.id);
        setDraftName(created.name);
        toast.success("カテゴリを作成しました");
      } else {
        const updated = await api.renameTaskCategory(selectedView, name);
        setDraftName(updated.name);
        toast.success("カテゴリを更新しました");
      }
      onChanged();
    } catch (error) {
      toast.error(`${selectedView === "new" ? "カテゴリの作成" : "カテゴリの更新"}に失敗しました：${(error as Error).message}`);
    } finally {
      setSaving(false);
    }
  }

  async function deleteCategory() {
    if (!selectedCategory || deleting) return;
    const deletedID = selectedCategory.id;
    setDeleting(true);
    try {
      await api.deleteTaskCategory(deletedID);
      const next = categories.find((category) => category.id !== deletedID);
      if (next) selectCategory(next);
      else selectUncategorized();
      toast.success("カテゴリを削除しました。関連タスクは未分類に移動しました");
      setDeleteOpen(false);
      onChanged();
    } catch (error) {
      toast.error(`カテゴリの削除に失敗しました：${(error as Error).message}`);
    } finally {
      setDeleting(false);
    }
  }

  async function moveTask(task: Task, destination: string) {
    if (movingTaskID) return;
    const category =
      destination === "uncategorized" ? null : (categories.find((item) => item.id === Number(destination)) ?? null);
    if (destination !== "uncategorized" && !category) {
      toast.error("対象のカテゴリが存在しません。更新してから再試行してください");
      return;
    }
    if (task.category_id === category?.id || (task.category_id == null && category == null)) return;

    setMovingTaskID(task.id);
    try {
      await api.updateTaskCategory(task.id, category?.id);
      onTaskMoved(task.id, category);
      toast.success(`タスク #${task.id} を「${category?.name ?? "未分類"}」に移動しました`);
    } catch (error) {
      toast.error(`タスクの移動に失敗しました：${(error as Error).message}`);
    } finally {
      setMovingTaskID(null);
    }
  }

  function handleDragStart(event: DragStartEvent) {
    const id = String(event.active.id);
    setActiveTaskID(id.startsWith("task:") ? id.slice("task:".length) : null);
  }

  function handleDragEnd(event: DragEndEvent) {
    const activeID = String(event.active.id);
    const taskID = activeID.startsWith("task:") ? activeID.slice("task:".length) : null;
    setActiveTaskID(null);
    if (!taskID || !event.over) return;
    const destination = String(event.over.id);
    if (!destination.startsWith("category:")) return;
    const task = tasks.find((item) => item.id === taskID);
    if (!task) return;
    void moveTask(task, destination.slice("category:".length));
  }

  const saveLabel = saving ? "保存中" : selectedView === "new" ? "カテゴリを作成" : "変更を保存";

  return (
    <>
      <Sheet open={open} onOpenChange={setOpen}>
        <SheetTrigger asChild>
          <Button size="sm" variant="outline">
            <TagsIcon data-icon="inline-start" />
            カテゴリ管理
          </Button>
        </SheetTrigger>
        <SheetContent className="grid h-full w-full! max-w-none! grid-rows-[auto_minmax(0,1fr)_auto] gap-0 overflow-hidden p-0 sm:w-[48rem]! sm:max-w-[48rem]!">
          <SheetHeader className="border-b px-6 py-5">
            <SheetTitle>タスクカテゴリ管理</SheetTitle>
            <SheetDescription>カテゴリはタスクの絞り込みとアーカイブに使います。変更はタスク実行に影響せず、削除するとタスクは未分類に移動します。</SheetDescription>
          </SheetHeader>
          <DndContext
            sensors={sensors}
            onDragStart={handleDragStart}
            onDragCancel={() => setActiveTaskID(null)}
            onDragEnd={handleDragEnd}
          >
            <div className="grid min-h-0 overflow-y-auto lg:grid-cols-[15rem_minmax(0,1fr)] lg:overflow-hidden">
              <div className="flex min-h-0 flex-col border-b p-3 lg:border-r lg:border-b-0">
                <Button type="button" variant="outline" className="w-full" onClick={startNew}>
                  <PlusIcon data-icon="inline-start" />
                  カテゴリを新規作成
                </Button>
                <ScrollArea className="mt-2 max-h-44 lg:max-h-none lg:flex-1">
                  <div className="flex flex-col gap-1 pr-2">
                    <CategoryDropTarget
                      value="uncategorized"
                      name="未分類"
                      count={uncategorizedCount}
                      selected={selectedView === "uncategorized"}
                      disabled={movingTaskID != null}
                      onSelect={selectUncategorized}
                    />
                    {categories.map((category) => (
                      <CategoryDropTarget
                        key={category.id}
                        value={String(category.id)}
                        name={category.name}
                        count={category.task_count}
                        selected={selectedView === category.id}
                        disabled={movingTaskID != null}
                        onSelect={() => selectCategory(category)}
                      />
                    ))}
                  </div>
                </ScrollArea>
              </div>
              <ScrollArea className="min-h-0">
                <FieldGroup className="p-6">
                  {selectedView !== "uncategorized" && (
                    <Field>
                      <FieldLabel htmlFor="task-category-name">カテゴリ名</FieldLabel>
                      <Input
                        id="task-category-name"
                        value={draftName}
                        onChange={(event) => setDraftName(event.target.value)}
                        placeholder="例：外部ネットワーク評価"
                        maxLength={80}
                        onKeyDown={(event) => {
                          if (event.key === "Enter") void saveCategory();
                        }}
                      />
                      <FieldDescription>
                        {selectedCategory
                          ? `現在 ${selectedCategory.task_count} 件のタスクがこのカテゴリを使用しています。名前を変更するとタスク一覧にも反映されます。`
                          : "作成すると、新規タスクとタスク一覧の絞り込みで使えます。"}
                      </FieldDescription>
                    </Field>
                  )}
                  {selectedView !== "new" && (
                    <Field>
                      <div className="flex flex-wrap items-end justify-between gap-2">
                        <div className="flex min-w-0 flex-col gap-1">
                          <FieldLabel>{selectedCategory ? "カテゴリ内のタスク" : "未分類のタスク"}</FieldLabel>
                          <FieldDescription>
                            {selectedCategory
                              ? `このカテゴリには ${visibleTasks.length} 件のタスクがあります。`
                              : `現在 ${visibleTasks.length} 件のタスクが未分類です。`}
                          </FieldDescription>
                        </div>
                      </div>
                      {visibleTasks.length === 0 ? (
                        <Empty className="min-h-36 border">
                          <EmptyHeader>
                            <EmptyTitle>{selectedCategory ? "このカテゴリにタスクはありません" : "未分類のタスクはありません"}</EmptyTitle>
                            <EmptyDescription>タスクが追加されるとここに表示されます。</EmptyDescription>
                          </EmptyHeader>
                        </Empty>
                      ) : (
                        <ItemGroup className="gap-2">
                          {visibleTasks.map((task) => (
                            <DraggableCategoryTask
                              key={task.id}
                              task={task}
                              moving={movingTaskID === task.id}
                              disabled={movingTaskID != null}
                            />
                          ))}
                        </ItemGroup>
                      )}
                    </Field>
                  )}
                </FieldGroup>
              </ScrollArea>
            </div>
            <DragOverlay>{activeTask ? <CategoryTaskDragPreview task={activeTask} /> : null}</DragOverlay>
          </DndContext>
          <SheetFooter className="border-t px-6 py-4 sm:flex-row sm:items-center">
            {selectedCategory && (
              <Button
                type="button"
                variant="destructive"
                className="sm:mr-auto"
                disabled={saving || deleting}
                onClick={() => setDeleteOpen(true)}
              >
                <Trash2Icon data-icon="inline-start" />
                カテゴリを削除
              </Button>
            )}
            <Button type="button" variant="outline" onClick={() => setOpen(false)}>
              閉じる
            </Button>
            {selectedView !== "uncategorized" && (
              <Button
                type="button"
                disabled={!draftName.trim() || saving || deleting}
                onClick={() => void saveCategory()}
              >
                {saving ? <Spinner data-icon="inline-start" /> : <SaveIcon data-icon="inline-start" />}
                {saveLabel}
              </Button>
            )}
          </SheetFooter>
        </SheetContent>
      </Sheet>
      <AlertDialog open={deleteOpen} onOpenChange={setDeleteOpen}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>カテゴリ「{selectedCategory?.name || "名称未設定のカテゴリ"}」を削除しますか？</AlertDialogTitle>
            <AlertDialogDescription>
              カテゴリを削除すると、含まれる {selectedCategory?.task_count ?? 0} 件のタスクは自動で「未分類」に移動します。タスクのデータは削除されません。
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel disabled={deleting}>キャンセル</AlertDialogCancel>
            <AlertDialogAction
              variant="destructive"
              disabled={deleting}
              onClick={(event) => {
                event.preventDefault();
                void deleteCategory();
              }}
            >
              {deleting && <Spinner data-icon="inline-start" />}
              {deleting ? "削除中" : "削除"}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </>
  );
}
