"use client";

import * as React from "react";
import { ChevronRightIcon, Loader2Icon, PaperclipIcon, PlusIcon } from "lucide-react";
import { toast } from "sonner";
import { TaskLLMProfileChain } from "@/components/task-llm-profile-chain";
import { TaskTemplateControls } from "@/components/task-template-controls";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import { Field, FieldDescription, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Sheet, SheetClose, SheetContent, SheetDescription, SheetFooter, SheetHeader, SheetTitle, SheetTrigger } from "@/components/ui/sheet";
import { Spinner } from "@/components/ui/spinner";
import { Textarea } from "@/components/ui/textarea";
import { api } from "@/lib/api";
import type { Company, LLMProfile, Task, TaskCategory } from "@/lib/types";
import { MAX_SOURCE_TASKS, appendUploads } from "./task-utils";
import { CategoryPicker } from "./task-categories";
import { CompanyPicker, SourceTaskPicker } from "./task-pickers";

export function CreateTaskSheet({
  tasks,
  categories,
  onCreated,
  onCategoriesChanged,
}: {
  tasks: Task[];
  categories: TaskCategory[];
  onCreated: () => void;
  onCategoriesChanged: () => void;
}) {
  const [open, setOpen] = React.useState(false);
  const [name, setName] = React.useState("");
  const [categoryID, setCategoryID] = React.useState<number | undefined>(undefined);
  const [description, setDescription] = React.useState("");
  const [goal, setGoal] = React.useState("");
  const [selectedTemplateID, setSelectedTemplateID] = React.useState<number | null>(null);
  const [profiles, setProfiles] = React.useState<LLMProfile[]>([]);
  const [companies, setCompanies] = React.useState<Company[]>([]);
  const [sourceTaskIDs, setSourceTaskIDs] = React.useState<string[]>([]);
  const [companyIDs, setCompanyIDs] = React.useState<number[]>([]);
  const [llmProfileIDs, setLLMProfileIDs] = React.useState<string[]>([]);
  const [creating, setCreating] = React.useState(false);
  const [timeoutMin, setTimeoutMin] = React.useState(""); // タスク単位のタイムアウト（分）。空/0 = 無制限
  const [heartbeatMin, setHeartbeatMin] = React.useState("10"); // planner のハートビート（分）。デフォルト 10、下限 10（バックエンドと一致）
  const [seedFirstIntent, setSeedFirstIntent] = React.useState(false); // 作成時にシード意図を送信し、worker は初回 planner を待たずに直接実行。デフォルトはオフで標準の「計画してから実行」
  const [coverageEnabled, setCoverageEnabled] = React.useState(true); // アセットカバレッジ機能。デフォルトはオン。オフ = カバレッジの計算/表示なし、範囲の累積なし、範囲系ツールを非表示（company 関連は影響なし）
  // 方式 1 ファイルアップロード：タスク作成前にファイルを drafts/<draftId>/uploads/ へ一時保存し、絶対パスを受け取って説明に追記する。
  const [uploading, setUploading] = React.useState(false);
  const [uploadCount, setUploadCount] = React.useState(0);
  const draftIdRef = React.useRef<string>("");
  const fileInputRef = React.useRef<HTMLInputElement>(null);
  const sheetContentRef = React.useRef<HTMLDivElement>(null);

  // load LLM profiles once for the create-task profile picker.
  React.useEffect(() => {
    api
      .llmProfiles()
      .then(setProfiles)
      .catch(() => setProfiles([]));
    api
      .companies()
      .then(setCompanies)
      .catch(() => setCompanies([]));
  }, []);

  // pickFiles uploads the chosen files into this draft's staging dir and appends their
  // absolute paths to the description; the task's agents open them by path via Read/Bash.
  async function pickFiles(files: FileList | null) {
    if (!files || files.length === 0) return;
    // crypto.randomUUID は安全なコンテキスト（https/localhost）でのみ利用可能。IP+http 経由では代替実装に切り替える。
    if (!draftIdRef.current) {
      draftIdRef.current =
        globalThis.crypto?.randomUUID?.() ?? `d${Date.now().toString(36)}${Math.random().toString(36).slice(2, 10)}`;
    }
    setUploading(true);
    try {
      const r = await api.chatUpload("staging", draftIdRef.current, Array.from(files));
      setDescription((prev) => appendUploads(prev, r.attachments));
      setUploadCount((n) => n + r.attachments.length);
    } catch (e) {
      toast.error("アップロードに失敗しました：" + (e as Error).message);
    } finally {
      setUploading(false);
      if (fileInputRef.current) fileInputRef.current.value = ""; // allow re-picking the same file
    }
  }

  async function createTask() {
    if (!description.trim() || !goal.trim()) {
      toast.error("説明とゴールを入力してください");
      return;
    }
    if (sourceTaskIDs.length > MAX_SOURCE_TASKS) {
      toast.error(`関連付けられるソースタスクは最大 ${MAX_SOURCE_TASKS} 件です`);
      return;
    }
    setCreating(true);
    try {
      const timeoutSec = Math.max(0, Math.floor(Number(timeoutMin) || 0)) * 60;
      const heartbeatSec = Math.max(10, Math.floor(Number(heartbeatMin) || 10)) * 60; // 下限 10min、バックエンドの正規化と一致
      await api.createTask({
        name: name.trim(),
        categoryId: categoryID,
        description: description.trim(),
        goal: goal.trim(),
        llmProfileIds: llmProfileIDs.map(Number),
        sourceTaskIds: sourceTaskIDs,
        companyIds: companyIDs,
        timeoutSeconds: timeoutSec,
        seedFirstIntent,
        planHeartbeatSeconds: heartbeatSec,
        coverageEnabled,
      });
      toast.success("タスクを作成しました");
      setName("");
      setCategoryID(undefined);
      setDescription("");
      setGoal("");
      setSelectedTemplateID(null);
      setSourceTaskIDs([]);
      setCompanyIDs([]);
      setLLMProfileIDs([]);
      setTimeoutMin("");
      setHeartbeatMin("10");
      setSeedFirstIntent(false);
      setCoverageEnabled(true);
      setUploadCount(0);
      draftIdRef.current = "";
      setOpen(false);
      onCreated();
    } catch (e) {
      toast.error("作成に失敗しました：" + (e as Error).message);
    } finally {
      setCreating(false);
    }
  }

  return (
    <Sheet open={open} onOpenChange={setOpen}>
      <SheetTrigger asChild>
        <Button size="sm">
          <PlusIcon /> 新規タスク
        </Button>
      </SheetTrigger>
      {/* 幅 45vw の右側ドロワー：画面の高さいっぱいにスクロールでき、長いフォームがダイアログの高さ制限を受けない。狭い画面では全幅になる。
            内容は flex の列：ヘッダー/フッターは固定、中央のフィールド領域は flex-1 で独立スクロール。 */}
      <SheetContent
        ref={sheetContentRef}
        side="right"
        className="w-full! max-w-none! gap-0 p-0 sm:w-[45vw]! sm:max-w-[45vw]!"
      >
        <SheetHeader className="border-b p-6">
          <SheetTitle>新規タスク</SheetTitle>
          <SheetDescription>テスト対象とゴールを入力します。詳細パラメータは必要に応じて展開できます。</SheetDescription>
        </SheetHeader>

        <div className="flex-1 overflow-y-auto p-6">
          <div className="grid gap-5">
            <TaskTemplateControls
              description={description}
              goal={goal}
              selectedTemplateID={selectedTemplateID}
              onSelectedTemplateIDChange={setSelectedTemplateID}
              onApply={(template) => {
                setDescription(template.description);
                setGoal(template.goal);
                setUploadCount(0);
              }}
              portalContainer={sheetContentRef}
            />
            <div className="grid gap-2">
              <Label htmlFor="name">名前（任意）</Label>
              <Input
                id="name"
                placeholder="識別しやすい名前を付けます。例：Acme 公式サイトのペネトレーション"
                value={name}
                onChange={(e) => setName(e.target.value)}
              />
            </div>
            <Field>
              <FieldLabel htmlFor="task-category">タスクカテゴリ</FieldLabel>
              <CategoryPicker
                categories={categories}
                value={categoryID}
                onValueChange={setCategoryID}
                onCategoryCreated={onCategoriesChanged}
                portalContainer={sheetContentRef}
              />
              <FieldDescription>任意、単一カテゴリ。タスク一覧の絞り込みとアーカイブに使い、エージェントの実行には影響しません。</FieldDescription>
            </Field>
            <div className="grid gap-2">
              <Label htmlFor="description">説明</Label>
              <Textarea
                id="description"
                className="min-h-32"
                placeholder="テスト対象と背景。例：example.com というサイトをテストする"
                value={description}
                onChange={(e) => setDescription(e.target.value)}
              />
              {/* ファイルをアップロード（複数可）：drafts/ に一時保存し、絶対パスを上の説明に追記する。worker はそれを使って Read/Bash で開く。 */}
              <div className="flex flex-wrap items-center gap-2">
                <input
                  ref={fileInputRef}
                  type="file"
                  multiple
                  className="hidden"
                  onChange={(e) => void pickFiles(e.target.files)}
                />
                <Button
                  type="button"
                  variant="outline"
                  size="sm"
                  onClick={() => fileInputRef.current?.click()}
                  disabled={uploading}
                >
                  {uploading ? <Loader2Icon className="animate-spin" /> : <PaperclipIcon />}
                  ファイルをアップロード
                </Button>
                <span className="text-muted-foreground text-xs">
                  {uploadCount > 0
                    ? `${uploadCount} 件のファイルをアップロードしました。絶対パスを説明の末尾に追記しました（編集可能）`
                    : "複数のファイルを選べます。アップロード後、保存先の絶対パスを説明に追加します。ワーカーはReadやBashでファイルを開けます。"}
                </span>
              </div>
            </div>
            <div className="grid gap-2">
              <Label htmlFor="goal">ゴール</Label>
              <Textarea
                id="goal"
                className="min-h-32"
                placeholder="何を達成するか。例：管理画面の権限を取得する、サーバー権限を取得する"
                value={goal}
                onChange={(e) => setGoal(e.target.value)}
              />
            </div>
            <Field>
              <FieldLabel htmlFor="source-tasks">関連タスク</FieldLabel>
              <SourceTaskPicker
                tasks={tasks}
                value={sourceTaskIDs}
                onValueChange={setSourceTaskIDs}
                portalContainer={sheetContentRef}
              />
              <FieldDescription>
                最大 {MAX_SOURCE_TASKS}{" "}
                件まで関連付けられます。選択したタスクの調査メモ、アセット範囲、通信記録を読み取り専用で参照できます。参照先を変更すると、その内容も更新されます。新しいタスクの結果は、このタスクの調査メモに保存します。
              </FieldDescription>
            </Field>
            <Field>
              <FieldLabel htmlFor="task-companies">関連企業のアセット範囲</FieldLabel>
              <CompanyPicker
                companies={companies}
                value={companyIDs}
                onValueChange={setCompanyIDs}
                portalContainer={sheetContentRef}
              />
              <FieldDescription>
                タスク作成時に、選択した企業のアセットを「テストアセット」に追加します。ドメイン、IP、CIDR、ICP、企業キーワードは、調査範囲の情報としてエージェントに渡します。実行指示の自動作成やゴールの変更は行いません。
              </FieldDescription>
            </Field>
            <Field>
              <FieldLabel htmlFor="llm-profiles">使用するLLM設定の順序</FieldLabel>
              <TaskLLMProfileChain
                profiles={profiles}
                value={llmProfileIDs}
                onValueChange={setLLMProfileIDs}
                inputId="llm-profiles"
                portalContainer={sheetContentRef}
              />
              <FieldDescription>リスト順にフェイルオーバーします。先頭が現在の設定で、明確にクォータ不足の時だけ次へ切り替えます。</FieldDescription>
            </Field>

            {/* 詳細パラメータはデフォルトで折りたたむ：タイムアウト/ハートビート/最初の意図。展開時だけ場所を取り、よく使う導線をすっきり保つ。 */}
            <Collapsible>
              <CollapsibleTrigger className="group flex w-full items-center gap-2 border-t pt-4 text-sm font-medium">
                <ChevronRightIcon className="text-muted-foreground size-4 transition-transform group-data-[state=open]:rotate-90" />
                詳細設定
                <span className="text-muted-foreground ml-auto text-xs font-normal">タイムアウト · ハートビート · 最初の実行指示</span>
              </CollapsibleTrigger>
              <CollapsibleContent className="grid gap-5 pt-5">
                <div className="grid gap-2">
                  <Label htmlFor="timeout-min">タスクのタイムアウト（分、任意）</Label>
                  <Input
                    id="timeout-min"
                    type="number"
                    min={0}
                    className="w-40"
                    placeholder="空 = 無制限"
                    value={timeoutMin}
                    onChange={(e) => setTimeoutMin(e.target.value)}
                  />
                  <p className="text-muted-foreground text-xs">
                    指定時間に達すると、各エージェントが結果を保存し、プランナーが最終判定を行います。その後、タスクはタイムアウトとして終了します。
                  </p>
                </div>
                <div className="grid gap-2">
                  <Label htmlFor="heartbeat-min">プランナーの定期実行間隔（分）</Label>
                  <Input
                    id="heartbeat-min"
                    type="number"
                    min={10}
                    className="w-40"
                    placeholder="デフォルト 10"
                    value={heartbeatMin}
                    onChange={(e) => setHeartbeatMin(e.target.value)}
                  />
                  <p className="text-muted-foreground text-xs">
                    前回の計画終了、またはタスク開始から指定時間が経過し、新しい通知がなければ、プランナーが計画と実行中のワーカーを再確認します。最短で10分です。
                  </p>
                </div>
                <div className="grid gap-2">
                  <label htmlFor="seed-first-intent" className="flex items-center gap-2 text-sm">
                    <Checkbox
                      id="seed-first-intent"
                      checked={seedFirstIntent}
                      onCheckedChange={(v) => setSeedFirstIntent(!!v)}
                    />
                    最初の実行指示（説明+ゴール）を直接送信
                  </label>
                  <p className="text-muted-foreground text-xs">
                    オンにすると、説明とゴールを最初の実行指示として送り、ワーカーが計画を待たずに作業を始めます。実行後はプランナーが結果を確認して計画を補います。1回の作業で解けることが多いCTFなどに適しています。オフの場合は、計画を立ててから実行します。
                  </p>
                </div>
                <div className="grid gap-2">
                  <label htmlFor="coverage-enabled" className="flex items-center gap-2 text-sm">
                    <Checkbox
                      id="coverage-enabled"
                      checked={coverageEnabled}
                      onCheckedChange={(v) => setCoverageEnabled(!!v)}
                    />
                    アセットカバレッジ機能
                  </label>
                  <p className="text-muted-foreground text-xs">
                    オンにすると、調査済みの範囲を集計し、カバレッジと状況図に進捗を表示します。オフの場合、状況図にはアセットだけを表示し、エージェントへの範囲関連ツールの提供も停止します。「関連企業のアセット範囲」は引き続き利用できます。初期設定はオンです。
                  </p>
                </div>
              </CollapsibleContent>
            </Collapsible>
          </div>
        </div>

        <SheetFooter className="flex-row justify-end gap-2 border-t p-4">
          <SheetClose asChild>
            <Button variant="outline">キャンセル</Button>
          </SheetClose>
          <Button onClick={createTask} disabled={creating || uploading}>
            {creating && <Spinner data-icon="inline-start" />}
            {creating ? "作成中" : "作成"}
          </Button>
        </SheetFooter>
      </SheetContent>
    </Sheet>
  );
}
