"use client";

import * as React from "react";

import {
  Loader2Icon,
  PlugZapIcon,
  PlusIcon,
  RefreshCwIcon,
  RotateCcwIcon,
  SaveIcon,
  StarIcon,
  Trash2Icon,
  ZapIcon,
} from "lucide-react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Separator } from "@/components/ui/separator";
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from "@/components/ui/sheet";
import { Switch } from "@/components/ui/switch";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { api } from "@/lib/api";
import type { LLMPoolMember, LLMPoolStatus, LLMProfile, LLMRetryOverride } from "@/lib/types";
import { cn } from "@/lib/utils";

import { ProfileRetryFields, RetryPolicyPanel, ZERO_OVERRIDE } from "./_components/retry";

// 思考スイッチ(thinking.type)と思考強度(reasoning_effort)は【互いに独立】のフィールドで、
// それぞれ個別に設定する。thinking を持たず強度だけで思考を有効化できる API もあるため分離する。
// 保存時の空文字列 = そのフィールドを【送信しない】；Radix Select は空 value を受け付けないため、
// UI では "none" センチネルで表現し、保存・読込時に "" と相互変換（NONE / fromStore / toStore）。
const NONE = "none";
const fromStore = (v?: string) => (v ? v : NONE);
const toStore = (v: string) => (v === NONE ? "" : v);
const THINKING_TYPES: { value: string; label: string }[] = [
  { value: NONE, label: "送信しない（デフォルト）" },
  { value: "disabled", label: "オフ" },
  { value: "enabled", label: "オン" },
];
// 出力上限に使うリクエストフィールド名（openai 形式でのみ有効）。NONE ↔ "" は同じセンチネル変換。
const MAX_TOKENS_FIELDS: { value: string; label: string }[] = [
  { value: NONE, label: "max_tokens（デフォルト）" },
  { value: "max_completion_tokens", label: "max_completion_tokens" },
];
// 他の 2 形式はフィールド名が固定で、選択肢は意味を持たないため説明文で明示する。
const MAX_TOKENS_FIELD_HINTS: Record<string, string> = {
  openai:
    "出力上限を送るフィールド名を選びます。通常はmax_tokensを使い、利用するモデルや互換APIがmax_completion_tokensを要求する場合は、そちらを選んでください。",
  anthropic: "openai 形式でのみ選択可能。Anthropic のフィールド名は max_tokens に固定されています。",
  "openai-responses": "openai 形式でのみ選択可能。Responses API のフィールド名は max_output_tokens に固定されています。",
};
const EFFORT_LEVELS: { value: string; label: string }[] = [
  { value: NONE, label: "送信しない（デフォルト）" },
  { value: "low", label: "low" },
  { value: "medium", label: "medium" },
  { value: "high", label: "high" },
  { value: "xhigh", label: "xhigh" },
  { value: "max", label: "max" },
];

function cooldownText(secs: number) {
  if (secs <= 0) return "";
  if (secs < 60) return `${secs}s`;
  return `${Math.ceil(secs / 60)}min`;
}

// カードに表示する設定の「正常かどうか」。Key 未設定はリクエスト自体を送れないため、ブレーカーより先に示す；
// 他の状態はポーリングのブレーカー記録に由来する（ポーリングがオフなら新規記録はなく、この場合「正常」= 既知の障害なし）。
type Health = { label: string; cls: string; hint?: string };
function healthOf(p: LLMProfile, m?: LLMPoolMember): Health {
  if (!p.api_key_hint) {
    return {
      label: "Key 未設定",
      cls: "border-muted-foreground/40 text-muted-foreground",
      hint: "APIキーが未入力のため呼び出せません",
    };
  }
  if (m?.state === "tripped") {
    return {
      label: m.cooldown_secs > 0 ? `遮断中 · ${cooldownText(m.cooldown_secs)}` : "遮断中",
      cls: "border-destructive/50 text-destructive",
      hint: m.last_error,
    };
  }
  if (m?.state === "degraded") {
    return {
      label: `異常 · 失敗 ${m.fails} 回`,
      cls: "border-amber-500/50 text-amber-600 dark:text-amber-400",
      hint: m.last_error,
    };
  }
  return { label: "正常", cls: "border-emerald-500/50 text-emerald-600 dark:text-emerald-400" };
}

// ─────────────────────────────────────────────────────────────────────────────
// ポーリング設定ドロワー
// ─────────────────────────────────────────────────────────────────────────────

function PoolSheet({
  open,
  onOpenChange,
  pool,
  onReload,
}: {
  open: boolean;
  onOpenChange: (o: boolean) => void;
  pool: LLMPoolStatus | null;
  onReload: () => Promise<void>;
}) {
  const [busy, setBusy] = React.useState(false);

  // クールダウン残り秒数はバックエンドが算出する。ドロワーが開いていて異常な設定がある時だけ定期取得し、進める。
  React.useEffect(() => {
    if (!open || !pool?.enabled || !pool.chain.some((m) => m.state !== "ok")) return;
    const t = setInterval(() => void onReload(), 10_000);
    return () => clearInterval(t);
  }, [open, pool, onReload]);

  async function toggle(patch: { llm_pool_enabled?: boolean; llm_pool_bind_fallback?: boolean }) {
    if (busy) return;
    setBusy(true);
    try {
      await api.setSettings(patch);
      await onReload();
      if (patch.llm_pool_enabled !== undefined) {
        toast.success(patch.llm_pool_enabled ? "LLM設定の自動切り替えをオンにしました" : "LLM設定の自動切り替えをオフにしました");
      } else {
        toast.success("自動切り替えの設定を更新しました");
      }
    } catch (e) {
      toast.error(`設定に失敗しました：${(e as Error).message}`);
    } finally {
      setBusy(false);
    }
  }

  async function recover(id?: string) {
    try {
      await api.resetLLMPool(id);
      await onReload();
      toast.success(id ? "この設定を再開しました" : "すべての設定を再開しました");
    } catch (e) {
      toast.error(`再開に失敗しました：${(e as Error).message}`);
    }
  }

  const enabled = pool?.enabled ?? false;
  const chain = pool?.chain ?? [];
  // ポーリングに参加するメンバー（「ポーリング不参加」指定を除く）。順序はバックエンドの実際の試行順。
  const inChain = chain.filter((m) => m.active || !m.excluded);
  const tripped = chain.filter((m) => m.state === "tripped");

  return (
    <Sheet open={open} onOpenChange={onOpenChange}>
      <SheetContent side="right" className="flex flex-col gap-0 p-0 data-[side=right]:sm:max-w-lg">
        <SheetHeader className="px-4">
          <SheetTitle className="flex items-center gap-2">
            <ZapIcon className="size-4" /> LLM設定の自動切り替え
          </SheetTitle>
          <SheetDescription>
            オンにすると、<b>モデル未指定</b>のエージェントは、残高不足、APIキーの無効化、レート制限、サービスの障害などで現在の設定を使えないとき、次の設定に切り替えます。
          </SheetDescription>
        </SheetHeader>

        <div className="flex min-h-0 flex-1 flex-col gap-4 overflow-y-auto px-4 pb-6">
          <div className="flex items-center justify-between gap-4 rounded-lg border p-3">
            <div className="grid gap-0.5">
              <Label className="text-sm">自動切り替えを有効にする</Label>
              <p className="text-muted-foreground text-xs">
                初期設定はオフです。オフの場合は、現在のアクティブ設定だけを使います。
              </p>
            </div>
            <Switch
              checked={enabled}
              disabled={busy}
              onCheckedChange={(v) => void toggle({ llm_pool_enabled: v })}
              aria-label="LLM設定の自動切り替えのオン/オフ"
            />
          </div>

          {enabled && (
            <>
              <div className="flex items-center justify-between gap-4 rounded-lg border p-3">
                <div className="grid gap-0.5">
                  <Label className="text-sm">モデル指定時も自動切り替え</Label>
                  <p className="text-muted-foreground text-xs">
                    オフの場合、エージェントやタスクに指定した設定だけを使います。オンにすると、指定した設定で失敗した場合も、下の順序で別の設定を試します。初期設定はオフです。
                  </p>
                </div>
                <Switch
                  checked={pool?.bind_fallback ?? false}
                  disabled={busy}
                  onCheckedChange={(v) => void toggle({ llm_pool_bind_fallback: v })}
                  aria-label="指定設定からの自動切り替えのオン/オフ"
                />
              </div>

              <Separator />

              <div className="grid gap-2">
                <div className="flex items-center justify-between">
                  <Label className="text-sm">切り替え順</Label>
                  {tripped.length > 0 && (
                    <Button size="sm" variant="ghost" onClick={() => void recover()}>
                      <RotateCcwIcon /> すべて再開
                    </Button>
                  )}
                </div>
                {inChain.length < 2 && (
                  <p className="text-muted-foreground text-xs">
                    現在利用可能な設定は {inChain.length} 件です。自動切り替えを使うには、APIキーを登録した対象設定が2件以上必要です。
                  </p>
                )}
                {chain.map((m) => {
                  const excluded = m.excluded && !m.active;
                  const order = excluded ? null : inChain.findIndex((x) => x.profile_id === m.profile_id) + 1;
                  return (
                    <div
                      key={m.profile_id}
                      className={cn(
                        "grid gap-1 rounded-lg border p-2.5 text-sm",
                        excluded && "opacity-55",
                        m.state === "tripped" && "border-destructive/40",
                      )}
                    >
                      <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
                        <span className="w-5 shrink-0 text-center font-mono text-muted-foreground text-xs">
                          {order ?? "—"}
                        </span>
                        <span className="font-medium">{m.name}</span>
                        {m.active && (
                          <Badge variant="outline" className="border-amber-400/50 text-amber-500">
                            アクティブ
                          </Badge>
                        )}
                        {excluded && <Badge variant="outline">自動切り替えの対象外</Badge>}
                        <div className="ml-auto flex items-center gap-2">
                          {m.state === "tripped" && m.cooldown_secs > 0 && (
                            <span className="text-muted-foreground text-xs">クールダウン {cooldownText(m.cooldown_secs)}</span>
                          )}
                          {m.state === "degraded" && (
                            <span className="text-muted-foreground text-xs">連続失敗 {m.fails} 回</span>
                          )}
                          {m.state !== "ok" && (
                            <Button
                              size="icon"
                              variant="ghost"
                              className="size-7"
                              aria-label="今すぐ再開"
                              title="今すぐ再開：ブレーカーを解除し、次回の呼び出しでこの設定を再試行します"
                              onClick={() => void recover(m.profile_id)}
                            >
                              <RotateCcwIcon className="size-3.5" />
                            </Button>
                          )}
                        </div>
                      </div>
                      <div className="flex flex-wrap items-center gap-x-3 pl-7 text-muted-foreground text-xs">
                        <code className="truncate font-mono">{m.model}</code>
                        {!m.active && <span>優先度 {m.priority}</span>}
                      </div>
                      {m.last_error && (
                        <p className="truncate pl-7 font-mono text-muted-foreground text-xs" title={m.last_error}>
                          {m.last_error}
                        </p>
                      )}
                    </div>
                  );
                })}
                {chain.length === 0 && (
                  <div className="rounded-lg border border-dashed p-4 text-center text-muted-foreground text-sm">
                    設定はありません
                  </div>
                )}
              </div>

              <div className="rounded-lg border border-dashed p-3 text-muted-foreground text-xs leading-relaxed">
                アクティブ設定を最初に使い、その後は優先度の高い順に試します。失敗した設定は60秒、5分、30分の順に待機時間を延ばして一時停止し、待機後に自動復帰します。リクエストがコンテキストウィンドウに収まらない設定は使いません。モデルを指定したエージェントとタスクは、初期設定では自動切り替えの対象外です。
              </div>
            </>
          )}
        </div>
      </SheetContent>
    </Sheet>
  );
}

// ─────────────────────────────────────────────────────────────────────────────
// モデル設定ドロワー（新規作成 / 編集で同じフォームを共用）
// ─────────────────────────────────────────────────────────────────────────────

function ProfileSheet({
  profile,
  open,
  onOpenChange,
  onSaved,
}: {
  profile: LLMProfile | null; // null = 新規作成
  open: boolean;
  onOpenChange: (o: boolean) => void;
  onSaved: (id: string) => void;
}) {
  const isNew = !profile;
  const [name, setName] = React.useState("");
  const [format, setFormat] = React.useState<"anthropic" | "openai" | "openai-responses">("anthropic");
  const [model, setModel] = React.useState("");
  const [baseUrl, setBaseUrl] = React.useState("");
  const [proxy, setProxy] = React.useState("");
  const [apiKey, setApiKey] = React.useState("");
  const [keyHint, setKeyHint] = React.useState("");
  const [rps, setRps] = React.useState("0");
  const [rpm, setRpm] = React.useState("0");
  const [cw, setCw] = React.useState("0"); // コンテキストウィンドウ(K tokens);0=デフォルト200K
  const [thinkingType, setThinkingType] = React.useState(NONE);
  const [effort, setEffort] = React.useState(NONE);
  const [priority, setPriority] = React.useState("0"); // ポーリング順位;大きいほど先
  const [poolExclude, setPoolExclude] = React.useState(false);
  const [streaming, setStreaming] = React.useState(true); // true=ストリーミング(デフォルト);false=非ストリーミング
  const [maxTokens, setMaxTokens] = React.useState("0"); // 1 回の返答の出力上限;0=送信しない
  const [maxTokensField, setMaxTokensField] = React.useState(NONE); // 上限に使うフィールド名;NONE=max_tokens
  const [sessionHeaderKey, setSessionHeaderKey] = React.useState(""); // カスタムセッションヘッダー名;空=送信しない
  const [retry, setRetry] = React.useState<LLMRetryOverride>(ZERO_OVERRIDE); // この設定の再試行オーバーライド;すべて 0=グローバルに従う
  const [testing, setTesting] = React.useState(false);
  const [saving, setSaving] = React.useState(false);
  const [models, setModels] = React.useState<string[]>([]);
  const [loadingModels, setLoadingModels] = React.useState(false);
  const [modelsOpen, setModelsOpen] = React.useState(false);

  // 開くたびに渡された profile でフォームを初期化（新規ならデフォルト値にリセット）。ドロワーを閉じて
  // 開き直せば毎回クリーンに始まり、前の設定の残影は残らない。
  React.useEffect(() => {
    if (!open) return;
    setName(profile?.name ?? "");
    setFormat(profile?.format === "openai" || profile?.format === "openai-responses" ? profile.format : "anthropic");
    setModel(profile?.model ?? "");
    setBaseUrl(profile?.base_url ?? "");
    setProxy(profile?.proxy ?? "");
    setRps(String(profile?.rate_per_second ?? 0));
    setRpm(String(profile?.rate_per_minute ?? 0));
    setCw(String(profile?.context_window_k ?? 0));
    setThinkingType(fromStore(profile?.thinking_type));
    setEffort(fromStore(profile?.reasoning_effort));
    setPriority(String(profile?.priority ?? 0));
    setPoolExclude(profile?.pool_exclude ?? false);
    setStreaming(profile?.streaming ?? true);
    setMaxTokens(String(profile?.max_tokens ?? 0));
    setMaxTokensField(fromStore(profile?.max_tokens_field));
    setSessionHeaderKey(profile?.session_header_key ?? "");
    setRetry(profile?.retry ?? ZERO_OVERRIDE);
    setApiKey("");
    setKeyHint(profile?.api_key_hint ?? "");
    setModels([]);
    setModelsOpen(false);
  }, [open, profile]);

  const profileId = profile ? Number(profile.id) : undefined;

  async function loadModels() {
    if (loadingModels) return;
    setLoadingModels(true);
    setModels([]);
    try {
      const r = await api.fetchLLMModels(format, baseUrl, apiKey, proxy, profileId);
      if (r.ok && r.models && r.models.length > 0) {
        setModels(r.models);
        setModelsOpen(true);
        toast.success(`${r.models.length} 個のモデルを読み込みました`);
      } else {
        toast.error(`モデルの読み込みに失敗しました：${r.error ?? "モデルを取得できませんでした"}`);
      }
    } catch (e) {
      toast.error(`モデルの読み込み中にエラーが発生しました：${(e as Error).message}`);
    } finally {
      setLoadingModels(false);
    }
  }

  async function testConnection() {
    if (testing) return;
    setTesting(true);
    try {
      // 設定が実際に使う思考パラメータでテストし、そのフィールド未対応のモデルはここで失敗させる。
      // タスク実行時に初めて壊れるのを避けるため。profile id を渡し、Key 欄が空なら保存済み Key を使う。
      const r = await api.testLLM(
        format,
        model,
        baseUrl,
        apiKey,
        proxy,
        toStore(thinkingType),
        toStore(effort),
        profileId,
        streaming,
        sessionHeaderKey.trim(),
      );
      // 返答内容も併せて表示：モデルが実際に応答したことを確認できて初めて、セッションと同等に通ったと判断できる。
      if (r.ok)
        toast.success(`接続成功 · ${r.latency_ms ?? "?"}ms · ${r.model ?? model}`, {
          description: r.reply ? `返答：${r.reply}` : undefined,
        });
      else toast.error(`接続に失敗しました：${r.error ?? "不明"}`);
    } catch (e) {
      toast.error(`テスト中にエラーが発生しました：${(e as Error).message}`);
    } finally {
      setTesting(false);
    }
  }

  async function save() {
    if (!name.trim() || !model.trim()) {
      toast.error("名前とモデルを入力してください");
      return;
    }
    if (saving) return;
    setSaving(true);
    try {
      const { id } = await api.saveLLMProfile({
        ...(profile ? { id: Number(profile.id) } : {}),
        name: name.trim(),
        format,
        model: model.trim(),
        base_url: baseUrl.trim(),
        proxy: proxy.trim(),
        api_key: apiKey,
        rate_per_second: Number(rps) || 0,
        rate_per_minute: Number(rpm) || 0,
        context_window_k: Number(cw) || 0,
        thinking_type: toStore(thinkingType),
        reasoning_effort: toStore(effort),
        priority: Number(priority) || 0,
        pool_exclude: poolExclude,
        streaming,
        max_tokens: Math.max(0, Number(maxTokens) || 0),
        // フィールド名の切り替えは openai(Chat Completions) でのみ意味を持ち、他の形式は常にデフォルトへ；
        // バックエンドも同じ正規化を行うが、ここでは UI が矛盾した値を送らないようにするだけ。
        max_tokens_field: format === "openai" ? toStore(maxTokensField) : "",
        session_header_key: sessionHeaderKey.trim(),
        retry,
      });
      if (isNew) toast.success(`作成しました：${name.trim()}（カードで「アクティブに設定」すると有効になります）`);
      else toast.success(profile?.is_default ? "保存しました。アクティブ設定は即時反映され、再起動は不要です" : "保存しました");
      onSaved(String(id));
      onOpenChange(false);
    } catch (e) {
      toast.error(`保存に失敗しました：${(e as Error).message}`);
    } finally {
      setSaving(false);
    }
  }

  return (
    <Sheet open={open} onOpenChange={onOpenChange}>
      <SheetContent
        side="right"
        className="flex flex-col gap-0 p-0 data-[side=right]:min-w-[420px] data-[side=right]:sm:max-w-xl"
      >
        <SheetHeader className="px-4">
          <SheetTitle className="flex items-center gap-2">
            {isNew ? "新規モデル設定" : `編集：${profile?.name}`}
            {profile?.is_default && (
              <Badge variant="outline" className="border-amber-400/50 text-amber-500">
                アクティブ
              </Badge>
            )}
          </SheetTitle>
          <SheetDescription>
            {isNew
              ? "作成直後は自動でアクティブになりません。カードで「アクティブに設定」すると有効になります。"
              : "変更後は保存をクリック。アクティブ設定は保存後すぐ全エージェントに反映されます。"}
          </SheetDescription>
        </SheetHeader>

        <div className="flex min-h-0 flex-1 flex-col gap-4 overflow-y-auto px-4 pb-4">
          <div className="grid gap-4 sm:grid-cols-2">
            <div className="grid gap-2">
              <Label htmlFor="p-name">名前</Label>
              <Input
                id="p-name"
                placeholder="例：OpenAI 本番"
                value={name}
                onChange={(e) => setName(e.target.value)}
              />
            </div>
            <div className="grid gap-2">
              <Label>形式</Label>
              <Select value={format} onValueChange={(v) => setFormat(v as "anthropic" | "openai" | "openai-responses")}>
                <SelectTrigger>
                  <SelectValue placeholder="形式を選択" />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="anthropic">Anthropic</SelectItem>
                  <SelectItem value="openai">OpenAI (Chat Completions)</SelectItem>
                  <SelectItem value="openai-responses">OpenAI (Responses API)</SelectItem>
                </SelectContent>
              </Select>
            </div>
          </div>

          <div className="grid gap-2">
            <Label htmlFor="p-model">モデル</Label>
            <div className="flex gap-2">
              <Input
                id="p-model"
                className="font-mono"
                placeholder="claude-opus-4-8"
                value={model}
                onChange={(e) => setModel(e.target.value)}
              />
              {/* modal: この Popover の内容は <body> に portal され、Sheet のスクロールロックの外にある。
                  modal を付けないとリストは描画されてもスクロールできない。付けば自身が最上位のロックを保持する。 */}
              <Popover open={modelsOpen} onOpenChange={setModelsOpen} modal>
                <PopoverTrigger asChild>
                  <Button
                    type="button"
                    variant="outline"
                    size="icon"
                    className="shrink-0"
                    disabled={loadingModels}
                    onClick={loadModels}
                    title="API から利用可能なモデルを読み込む"
                  >
                    {loadingModels ? <Loader2Icon className="animate-spin" /> : <RefreshCwIcon />}
                  </Button>
                </PopoverTrigger>
                {models.length > 0 && (
                  <PopoverContent className="max-h-72 w-72 gap-0 overflow-y-auto overscroll-contain p-1" align="end">
                    {models.map((m) => (
                      <button
                        key={m}
                        type="button"
                        className="w-full shrink-0 rounded-md px-2 py-1.5 text-left font-mono text-xs hover:bg-accent hover:text-accent-foreground"
                        onClick={() => {
                          setModel(m);
                          setModelsOpen(false);
                        }}
                      >
                        {m}
                      </button>
                    ))}
                  </PopoverContent>
                )}
              </Popover>
            </div>
          </div>

          <div className="grid gap-2">
            <Label htmlFor="p-base-url">Base URL（任意）</Label>
            <Input
              id="p-base-url"
              className="font-mono"
              placeholder="https://api.openai.com/v1"
              value={baseUrl}
              onChange={(e) => setBaseUrl(e.target.value)}
            />
          </div>

          <div className="grid gap-2">
            <Label htmlFor="p-proxy">プロキシ（任意）</Label>
            <Input
              id="p-proxy"
              className="font-mono"
              placeholder="socks5://user:pass@127.0.0.1:1080 · http://127.0.0.1:8080"
              value={proxy}
              onChange={(e) => setProxy(e.target.value)}
            />
            <p className="text-muted-foreground text-xs">
              LLMへのリクエストに使うプロキシです。HTTP、HTTPS、SOCKS5と認証情報に対応します（例：socks5://user:pass@host:port）。パスワードに特殊文字を含む場合はURLエンコードしてください。空欄の場合は直接接続します。
            </p>
          </div>

          <div className="grid gap-2">
            <Label htmlFor="p-session-header">カスタムセッションヘッダー（任意）</Label>
            <Input
              id="p-session-header"
              className="font-mono"
              placeholder="例 x-session-id（空欄=送信しない）"
              value={sessionHeaderKey}
              onChange={(e) => setSessionHeaderKey(e.target.value)}
            />
            <p className="text-muted-foreground text-xs">
              ヘッダー名を入力すると、毎回のリクエストにこの HTTP ヘッダーが付き、値は自動で <b>現在のセッションの session id</b>（chat なら
              conv-12、worker なら exp3-worker-i87 など）になります。session-id ヘッダーでプロンプトキャッシュ /
              スティッキールーティングを行うゲートウェイ向け。同一セッションでは複数ターンでも安定し、セッション間では異なります。空欄なら送信しません。
            </p>
          </div>

          <div className="grid gap-2">
            <Label htmlFor="p-api-key">APIキー</Label>
            <Input
              id="p-api-key"
              type="password"
              placeholder={keyHint ? `設定済み（${keyHint}）。空欄なら変更しません` : "sk-…"}
              value={apiKey}
              onChange={(e) => setApiKey(e.target.value)}
            />
          </div>

          <div className="grid gap-4 sm:grid-cols-3">
            <div className="grid gap-2">
              <Label htmlFor="p-rps">毎秒レート制限</Label>
              <Input id="p-rps" type="number" min={0} value={rps} onChange={(e) => setRps(e.target.value)} />
            </div>
            <div className="grid gap-2">
              <Label htmlFor="p-rpm">毎分レート制限</Label>
              <Input id="p-rpm" type="number" min={0} value={rpm} onChange={(e) => setRpm(e.target.value)} />
            </div>
            <div className="grid gap-2">
              <Label htmlFor="p-cw">コンテキストウィンドウ(K)</Label>
              <Input
                id="p-cw"
                type="number"
                min={0}
                max={1000}
                value={cw}
                onChange={(e) => setCw(e.target.value)}
                placeholder="200"
              />
            </div>
          </div>
          <p className="-mt-2 text-muted-foreground text-xs">
            レート制限は全エージェントで共有し、0は無制限です。コンテキストウィンドウの単位はK（千トークン）で、0は既定の200K、上限は1000（1M）です。値を大きくしすぎると、履歴の圧縮が始まらない場合があります。
          </p>

          <div className="grid gap-3 rounded-lg border p-3">
            <div className="flex items-center justify-between gap-4">
              <div className="grid gap-0.5">
                <Label htmlFor="p-priority" className="text-sm">
                  切り替えの優先度
                </Label>
                <p className="text-muted-foreground text-xs">
                  数値が大きい設定を先に使います。アクティブ設定は、この値に関係なく最初に使います。同じ優先度の設定は、順番に先頭を入れ替えます。
                </p>
              </div>
              <Input
                id="p-priority"
                type="number"
                className="w-24 shrink-0"
                value={priority}
                onChange={(e) => setPriority(e.target.value)}
              />
            </div>
            <div className="flex items-center justify-between gap-4 border-t pt-3">
              <div className="grid gap-0.5">
                <Label className="text-sm">自動切り替えの対象外</Label>
                <p className="text-muted-foreground text-xs">
                  オンにすると、自動切り替え先から除外します。エージェントやタスクから指定した場合は使用できます。特定の用途だけに使う設定に向いています。
                </p>
              </div>
              <Switch checked={poolExclude} onCheckedChange={setPoolExclude} aria-label="自動切り替えの対象外" />
            </div>
            <div className="flex items-center justify-between gap-4 border-t pt-3">
              <div className="grid gap-0.5">
                <Label className="text-sm">ストリーミング出力 · streaming</Label>
                <p className="text-muted-foreground text-xs">
                  オンにすると、SSEで応答を受け取り、処理の進行状況とトークン数を順次表示します。オフの場合は応答全体をまとめて受け取るため、生成途中の進行状況は表示されません。SSEの空フレームや思考情報の欠落が起きるゲートウェイでは、オフにすることで回避できる場合があります。初期設定はオンです。
                </p>
              </div>
              <Switch checked={streaming} onCheckedChange={setStreaming} aria-label="ストリーミング出力" />
            </div>
          </div>

          <div className="grid gap-3 rounded-lg border p-3">
            <div className="flex items-center justify-between gap-4">
              <div className="grid gap-0.5">
                <Label htmlFor="p-max-tokens" className="text-sm">
                  出力上限 · max tokens
                </Label>
                <p className="text-muted-foreground text-xs">
                  1回の応答で生成するトークン数の上限です。0の場合は上限を送らず、接続先の既定値を使います。コンテキストウィンドウは入力と出力を含む容量で、この出力上限とは別の設定です。上限が小さすぎると、思考中に打ち切られて回答が出ない場合があります。
                </p>
              </div>
              <Input
                id="p-max-tokens"
                type="number"
                min={0}
                className="w-28 shrink-0"
                value={maxTokens}
                onChange={(e) => setMaxTokens(e.target.value)}
                placeholder="0"
              />
            </div>
            <div className="flex items-center justify-between gap-4 border-t pt-3">
              <div className="grid gap-0.5">
                <Label className="text-sm">上限のフィールド名</Label>
                <p className="text-muted-foreground text-xs">{MAX_TOKENS_FIELD_HINTS[format]}</p>
              </div>
              <Select
                value={format === "openai" ? maxTokensField : NONE}
                onValueChange={setMaxTokensField}
                disabled={format !== "openai"}
              >
                <SelectTrigger className="w-56 shrink-0">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {MAX_TOKENS_FIELDS.map((o) => (
                    <SelectItem key={o.value} value={o.value}>
                      {o.label}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
          </div>

          <div className="grid gap-3 rounded-lg border p-3">
            <div className="flex items-center justify-between gap-4">
              <div className="grid gap-0.5">
                <Label className="text-sm">思考スイッチ · thinking.type</Label>
                <p className="text-muted-foreground text-xs">
                  thinking フィールドを送信するか制御。送信しない=フィールドなし（MiniMax など未対応モデル向け）；オフ=disabled
                  を送信；オン=enabled を送信。下の強度とは互いに独立です。
                </p>
              </div>
              <Select value={thinkingType} onValueChange={setThinkingType}>
                <SelectTrigger className="w-32 shrink-0">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {THINKING_TYPES.map((o) => (
                    <SelectItem key={o.value} value={o.value}>
                      {o.label}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
            <div className="flex items-center justify-between gap-4 border-t pt-3">
              <div className="grid gap-0.5">
                <Label className="text-sm">思考強度 · reasoning_effort</Label>
                <p className="text-muted-foreground text-xs">
                  独立した強度の段階（OpenAI reasoning_effort / Anthropic output_config.effort）。 thinking フィールドを持たず強度だけで
                  思考を有効化できる API もあるため、思考スイッチを送らず単独で設定できます。
                </p>
              </div>
              <Select value={effort} onValueChange={setEffort}>
                <SelectTrigger className="w-32 shrink-0">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {EFFORT_LEVELS.map((o) => (
                    <SelectItem key={o.value} value={o.value}>
                      {o.label}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
          </div>

          <ProfileRetryFields value={retry} onChange={setRetry} />
        </div>

        <div className="flex gap-2 border-t px-4 py-3">
          <Button variant="outline" onClick={testConnection} disabled={testing}>
            {testing ? <Loader2Icon className="animate-spin" /> : <PlugZapIcon />}
            {testing ? "テスト中…" : "接続テスト"}
          </Button>
          <Button onClick={save} disabled={saving} className="flex-1">
            {saving && <Loader2Icon className="animate-spin" />}
            {!saving && (isNew ? <PlusIcon /> : <SaveIcon />)}
            {isNew ? "新規作成" : "保存"}
          </Button>
        </div>
      </SheetContent>
    </Sheet>
  );
}

// ─────────────────────────────────────────────────────────────────────────────

export default function LLMPage() {
  const [profiles, setProfiles] = React.useState<LLMProfile[]>([]);
  const [pool, setPool] = React.useState<LLMPoolStatus | null>(null);
  const [poolOpen, setPoolOpen] = React.useState(false);
  // ドロワーの開閉状態と内容は別に保持：閉じる際も editing を維持しないと、閉じるアニメーション中に
  // タイトルが「編集 X」から「新規作成」にちらつく。editing = null は新規作成を表す。
  const [editOpen, setEditOpen] = React.useState(false);
  const [editing, setEditing] = React.useState<LLMProfile | null>(null);
  const openEditor = React.useCallback((p: LLMProfile | null) => {
    setEditing(p);
    setEditOpen(true);
  }, []);

  const loadPool = React.useCallback(async () => {
    try {
      setPool(await api.llmPool());
    } catch {
      /* ignore */
    }
  }, []);

  const load = React.useCallback(async () => {
    try {
      setProfiles(await api.llmProfiles());
    } catch {
      /* ignore */
    }
    await loadPool();
  }, [loadPool]);

  React.useEffect(() => {
    void load();
  }, [load]);

  // カードのヘルスバッジは profile id ごとにポーリング状態を取得。
  const health = React.useMemo(() => {
    const m = new Map<string, LLMPoolMember>();
    for (const c of pool?.chain ?? []) m.set(c.profile_id, c);
    return m;
  }, [pool]);

  async function activate(id: string, name: string) {
    try {
      await api.activateLLMProfile(id);
      toast.success(`アクティブにしました：${name}`);
      await load();
    } catch (e) {
      toast.error(`アクティブ化に失敗しました：${(e as Error).message}`);
    }
  }

  async function remove(p: LLMProfile) {
    if (p.is_default) {
      toast.error("現在アクティブな設定は削除できません");
      return;
    }
    try {
      await api.deleteLLMProfile(p.id);
      toast.success(`削除しました：${p.name}`);
      await load();
    } catch (e) {
      toast.error(`削除に失敗しました：${(e as Error).message}`);
    }
  }

  const poolOn = pool?.enabled ?? false;

  return (
    <div className="flex flex-1 flex-col gap-4 md:gap-6">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h1 className="font-semibold text-xl tracking-tight">LLM</h1>
          <p className="text-muted-foreground text-sm">
            全エージェントで共有するAPI形式、モデル、レート制限を設定します。カードをクリックすると編集できます。星印は現在のアクティブ設定です。
          </p>
        </div>
        <div className="flex items-center gap-2">
          <Button size="sm" variant="outline" onClick={() => setPoolOpen(true)}>
            <ZapIcon /> 自動切り替え設定
            {poolOn && (
              <Badge variant="outline" className="ml-1 border-emerald-500/50 text-emerald-600 dark:text-emerald-400">
                オン
              </Badge>
            )}
          </Button>
          <Button size="sm" variant="outline" onClick={() => openEditor(null)}>
            <PlusIcon /> 新規作成
          </Button>
        </div>
      </div>

      <Tabs defaultValue="profiles" className="flex-1">
        <TabsList>
          <TabsTrigger value="profiles">モデル設定</TabsTrigger>
          <TabsTrigger value="retry">再試行とバックオフ</TabsTrigger>
        </TabsList>

        <TabsContent value="profiles" className="mt-4">
          <div className="grid grid-cols-1 gap-4 md:grid-cols-2 xl:grid-cols-3">
            {profiles.map((p) => {
              const h = healthOf(p, health.get(p.id));
              return (
                // biome-ignore lint/a11y/useSemanticElements: カード内に独自の操作ボタンがあり、ネイティブ <button> を使うとボタンの入れ子（不正な HTML）になるため
                <Card
                  key={p.id}
                  role="button"
                  tabIndex={0}
                  onClick={() => openEditor(p)}
                  onKeyDown={(e) => {
                    if (e.key === "Enter" || e.key === " ") {
                      e.preventDefault();
                      openEditor(p);
                    }
                  }}
                  className={cn(
                    "cursor-pointer gap-0 py-4 outline-none transition-colors hover:border-foreground/30",
                    p.is_default && "border-amber-400/50 bg-amber-400/5",
                  )}
                >
                  <CardContent className="grid gap-2 px-4">
                    <div className="flex items-start gap-2">
                      <StarIcon
                        className={cn(
                          "mt-0.5 size-4 shrink-0",
                          p.is_default ? "fill-amber-400 text-amber-400" : "text-muted-foreground",
                        )}
                      />
                      <div className="min-w-0 flex-1">
                        <div className="flex flex-wrap items-center gap-2">
                          <span className="truncate font-medium text-sm">{p.name}</span>
                          <Badge variant="outline" className="uppercase">
                            {p.format}
                          </Badge>
                          <Badge variant="outline" className={cn("ml-auto", h.cls)} title={h.hint}>
                            {h.label}
                          </Badge>
                        </div>
                        <code className="mt-1 block truncate font-mono text-muted-foreground text-xs">{p.model}</code>
                      </div>
                    </div>

                    <div className="flex flex-wrap gap-x-3 gap-y-0.5 pl-6 text-muted-foreground text-xs">
                      {p.api_key_hint && <span>{p.api_key_hint}</span>}
                      <span>
                        {p.rate_per_second}/s · {p.rate_per_minute}/min
                      </span>
                      {p.proxy && <span className="truncate">プロキシ {p.proxy}</span>}
                      {p.reasoning_effort && (
                        <span>思考 {p.reasoning_effort === "off" ? "オフ" : p.reasoning_effort}</span>
                      )}
                      {/* ポーリング関連の 2 フィールドはポーリングがオンの時のみ意味があり、オフの時は表示しない */}
                      {poolOn &&
                        !p.is_default &&
                        (p.pool_exclude ? <span>自動切り替えの対象外</span> : <span>優先度 {p.priority ?? 0}</span>)}
                    </div>

                    <div className="mt-1 flex gap-2">
                      <Button
                        size="sm"
                        variant="outline"
                        className="flex-1"
                        disabled={p.is_default}
                        onClick={(e) => {
                          e.stopPropagation();
                          void activate(p.id, p.name);
                        }}
                      >
                        {p.is_default ? "アクティブ" : "アクティブに設定"}
                      </Button>
                      <Button
                        size="icon"
                        variant="outline"
                        aria-label="設定を削除"
                        onClick={(e) => {
                          e.stopPropagation();
                          void remove(p);
                        }}
                      >
                        <Trash2Icon className="text-destructive" />
                      </Button>
                    </div>
                  </CardContent>
                </Card>
              );
            })}
            {profiles.length === 0 && (
              <div className="col-span-full rounded-lg border border-dashed p-10 text-center text-muted-foreground text-sm">
                モデル設定がまだありません。右上の「新規作成」から最初の設定を作成してください。
              </div>
            )}
          </div>
        </TabsContent>

        <TabsContent value="retry" className="mt-4">
          <RetryPolicyPanel />
        </TabsContent>
      </Tabs>

      <ProfileSheet profile={editing} open={editOpen} onOpenChange={setEditOpen} onSaved={() => void load()} />
      <PoolSheet open={poolOpen} onOpenChange={setPoolOpen} pool={pool} onReload={loadPool} />
    </div>
  );
}
