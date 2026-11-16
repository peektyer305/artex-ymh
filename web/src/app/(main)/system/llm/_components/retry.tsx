"use client";

// LLM 再試行設定の共用部品：5 層の再試行それぞれの「回数 + 間隔」。
//
// 5 層を内側から外側へ：接続確立(SDK) → 空レスポンス(SDK) → 同一 provider 安全ウィンドウ → ポーリング遮断 → 意図の再実行。
// 内側 3 層はエンドポイントに紐づくため、モデル設定ごとにグローバル既定を上書きできる。外側 2 層はプロセス単位で、グローバルに 1 つだけ。
//
// すべての入力は「空欄 = 未設定」という同じ意味論に従い、バックエンドの db.RetryRule と一致する：
//   回数   空/0 = 組み込み既定 | -1 = この層の再試行をオフ | >0 = この回数
//   間隔   空/0 = この層本来の指数バックオフ | >0 = この固定ミリ秒間隔に変更

import * as React from "react";

import { Loader2Icon, SaveIcon } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { api } from "@/lib/api";
import type { LLMRetryOverride, LLMRetryPolicy, LLMRetryRule } from "@/lib/types";

export const ZERO_RULE: LLMRetryRule = { attempts: 0, interval_ms: 0 };
export const ZERO_OVERRIDE: LLMRetryOverride = {
  connect: ZERO_RULE,
  empty: ZERO_RULE,
  stream: ZERO_RULE,
};
const ZERO_POLICY: LLMRetryPolicy = {
  ...ZERO_OVERRIDE,
  breaker: ZERO_RULE,
  intent: ZERO_RULE,
};

type LayerMeta = {
  title: string;
  /** この層の再試行がどこで誰によって実行されるか */
  where: string;
  /** どんなエラーがこの層に来るか——ステータスコードまで具体的に、推測させない */
  trigger: string;
  /** 似ているが【この層に来ない】エラー。設定しても効かずバグだと誤解しないように */
  skips?: string;
  desc: string;
  attemptsLabel: string;
  /** 回数が空欄のときの既定値（プレースホルダ用） */
  defAttempts: number;
  /** 間隔が空欄のときの既定ポリシー（プレースホルダ用） */
  defInterval: string;
  /** 回数に -1 を入れた意味 */
  offHint: string;
};

export const RETRY_LAYERS = {
  connect: {
    title: "接続確立の再試行",
    where: "SDK · 200 を受け取るまで",
    trigger:
      "接続できない、またはまだ 200 を受け取っていない：接続リセット / 読み書きタイムアウト / DNS 失敗などのネットワーク層エラー、および HTTP 408、429、500、502、503、504。",
    skips: "その他のステータスコード（400 / 401 / 403 / 404 / 413 / 422 など）は確定的な拒否で、再送しても同じく失敗するためそのまま上位へ送出します。",
    desc: "同じリクエストをそのまま再送します。ストリームが始まった後（200 受信済み）の途中切断はこの層の対象外です。",
    attemptsLabel: "再試行回数",
    defAttempts: 3,
    defInterval: "0.5s→1s→2s 指数（上限 8s）",
    offHint: "-1 = 一度も再試行せず、失敗は即座に上位へ",
  },
  empty: {
    title: "空レスポンスの再試行",
    where: "SDK · openai 形式のみ",
    trigger:
      "HTTP 200、finish_reason は正常な stop なのに、レスポンス全体にコンテンツブロックが 1 つもない——ゲートウェイの空フレーム、思考フィールドのフレーム欠落、サンプリングの一時的な不調がこの形になります。",
    skips: "max_tokens による打ち切りで内容がない場合は対象外（出力上限を上げて解決するもので、再送しても同じ所で止まるだけ）。",
    desc: "prompt 全体を再送するため、長いコンテキストではコストが高い。回数は大きくしない方がよい。",
    attemptsLabel: "再試行回数",
    defAttempts: 2,
    defInterval: "0.5s→1s→2s 指数（上限 8s）",
    offHint: "-1 = 空レスポンスをそのまま返す",
  },
  stream: {
    title: "同一 provider 安全ウィンドウの再試行",
    where: "本プロジェクト · 出力を渡す前",
    trigger:
      "ストリーム確立（200 受信）後に問題が出た場合：接続の途中切断、プロバイダの overloaded、ストリーム内の 429 / 5xx エラーイベント——しかも token を 1 つも呼び出し元へ渡していないとき。",
    skips:
      "クォータ枯渇（402 / insufficient_quota、ポーリングによる設定切替へ）、コンテキスト長超過（413 / context length、圧縮へ）、400 / 401 / 403 / 404 / 422 の確定的な拒否は、いずれも再試行しません。",
    desc: "同じ設定で同じリクエストを再生します。まだ出力を渡していないため、再生でモデル出力やツール実行が重複することはありません。",
    attemptsLabel: "再試行回数",
    defAttempts: 2,
    defInterval: "0.5s→1s 指数（上限 4s）",
    offHint: "-1 = 断流は外側の意図再実行に委ねる",
  },
  breaker: {
    title: "ポーリング遮断",
    where: "本プロジェクト · プロセス単位、グローバルに 1 つ",
    trigger:
      "一時的な失敗（429、5xx、ネットワークエラー）が連続してしきい値に達すると遮断。残高不足（402）、キー失効（401 / 403）、モデル不在（404）といった確定的な失敗はしきい値を見ず、1 回目で遮断します。",
    skips: "1 回成功すれば 0 に戻るため、たまに不調になる程度の設定が積み上がって遮断されることはありません。",
    desc: "遮断後はクールダウンに入り、その間ポーリングはこの設定を飛ばします。状態は DB に保存され、再起動しても失われません。",
    attemptsLabel: "連続失敗が何回で遮断",
    defAttempts: 3,
    defInterval: "1min→5min→30min の段階",
    offHint: "-1 = 一時的な失敗では遮断しない（確定的な失敗は遮断）",
  },
  intent: {
    title: "意図の再実行",
    where: "本プロジェクト · プロセス単位、グローバルに 1 つ",
    trigger:
      "内側の層で受け止めきれなかった場合：worker が model_error で終了——内側の再試行をすべて使い切った、またはストリームが出力を渡し始めてから切れた（その時点の再生は安全でなく、全体をやり直すしかない）。",
    skips: "クォータ枯渇はポーリングの設定切替で処理済みのため、ここでは再実行しません。タスクが一時停止 / 終了 / 終盤処理に入ったら即座に譲り、バックオフ時間を占有しません。",
    desc: "意図全体を最初からやり直します。最外層なので、1 回の再実行で内側の層の回数がさらに掛け算になります。",
    attemptsLabel: "再実行回数",
    defAttempts: 2,
    defInterval: "固定 3s",
    offHint: "-1 = 再実行せず、その意図は blocked と判定",
  },
} satisfies Record<string, LayerMeta>;

type LayerKey = keyof typeof RETRY_LAYERS;

/** ミリ秒を人が読める表記に。入力欄の横に表示するだけで、ゼロを数えなくて済むように。 */
function humanMs(ms: number) {
  if (!Number.isFinite(ms) || ms <= 0) return "";
  if (ms < 1000) return `${ms}ms`;
  if (ms < 60_000) return `${Number((ms / 1000).toFixed(2))}s`;
  return `${Number((ms / 60_000).toFixed(2))}min`;
}

/** 制御された数値入力：空文字 ↔ 0、中間状態（"-"、"1e"）はそのままローカルに保持し、親を煩わせない。 */
function NumField({
  id,
  value,
  onChange,
  placeholder,
  min,
}: {
  id: string;
  value: number;
  onChange: (n: number) => void;
  placeholder: string;
  min: number;
}) {
  const [text, setText] = React.useState(value === 0 ? "" : String(value));
  // 親が値一式を入れ替えたとき（ポリシー読み込み、設定切替）は追従する。自分で入力しているときはここに来ない。
  // そのとき value はローカルテキストを parse した結果と既に等しいため。
  React.useEffect(() => {
    const incoming = value === 0 ? "" : String(value);
    setText((cur) => (Number(cur || 0) === value ? cur : incoming));
  }, [value]);
  return (
    <Input
      id={id}
      type="number"
      min={min}
      className="w-28 shrink-0"
      value={text}
      placeholder={placeholder}
      onChange={(e) => {
        setText(e.target.value);
        const n = Number(e.target.value);
        onChange(e.target.value.trim() === "" || !Number.isFinite(n) ? 0 : Math.trunc(n));
      }}
    />
  );
}

/** 1 層の再試行の 2 つのつまみ。idPrefix は同一ページに複数出るとき label の htmlFor を保つために使う。 */
export function RetryRuleFields({
  layer,
  idPrefix,
  value,
  onChange,
  compact,
}: {
  layer: LayerKey;
  idPrefix: string;
  value: LLMRetryRule;
  onChange: (r: LLMRetryRule) => void;
  /** true = 設定ドロワー用のコンパクト版：展開説明を省き、「どんなエラーがこの層に来るか」の 1 文だけ残す */
  compact?: boolean;
}) {
  const meta = RETRY_LAYERS[layer];
  const human = humanMs(value.interval_ms);
  return (
    <div className={compact ? "grid gap-2" : "grid gap-3 rounded-lg border p-3"}>
      <div className="grid gap-0.5">
        <div className="flex flex-wrap items-baseline gap-2">
          <Label className="text-sm">{meta.title}</Label>
          <span className="text-muted-foreground text-xs">{meta.where}</span>
        </div>
        {/* どのエラーがこの層に来るか、ステータスコードまで具体的に——つまみを設定しても効かないときは、そもそもこの層に来ていないことが多い。 */}
        <p className="text-muted-foreground text-xs">
          <span className="font-medium text-foreground">トリガー</span>：{meta.trigger}
        </p>
        {!compact && meta.skips && (
          <p className="text-muted-foreground text-xs">
            <span className="font-medium text-foreground">この層に来ない</span>：{meta.skips}
          </p>
        )}
        {!compact && <p className="text-muted-foreground text-xs">{meta.desc}</p>}
      </div>
      <div className="flex flex-wrap items-center gap-x-4 gap-y-2">
        <div className="flex items-center gap-2">
          <Label htmlFor={`${idPrefix}-${layer}-n`} className="text-muted-foreground text-xs">
            {meta.attemptsLabel}
          </Label>
          <NumField
            id={`${idPrefix}-${layer}-n`}
            min={-1}
            value={value.attempts}
            placeholder={`既定 ${meta.defAttempts}`}
            onChange={(n) => onChange({ ...value, attempts: n })}
          />
        </div>
        <div className="flex items-center gap-2">
          <Label htmlFor={`${idPrefix}-${layer}-ms`} className="text-muted-foreground text-xs">
            間隔 ms
          </Label>
          <NumField
            id={`${idPrefix}-${layer}-ms`}
            min={0}
            value={value.interval_ms}
            placeholder="既定のバックオフ"
            onChange={(n) => onChange({ ...value, interval_ms: n })}
          />
          <span className="text-muted-foreground text-xs">{human ? `固定 ${human}` : meta.defInterval}</span>
        </div>
      </div>
      {!compact && <p className="text-muted-foreground text-xs">空欄 = 既定値、{meta.offHint}。</p>}
    </div>
  );
}

/** モデル設定ドロワー内の 3 層の上書き（エンドポイントに紐づく 3 層）。 */
export function ProfileRetryFields({
  value,
  onChange,
}: {
  value: LLMRetryOverride;
  onChange: (o: LLMRetryOverride) => void;
}) {
  return (
    <div className="grid gap-3 rounded-lg border p-3">
      <div className="grid gap-0.5">
        <Label className="text-sm">再試行の上書き</Label>
        <p className="text-muted-foreground text-xs">
          この設定にのみ適用され、「再試行とバックオフ」のグローバル既定を上書きします。各欄が空欄 = グローバルに従う、回数に -1 = この層の再試行をオフ、
          間隔を入れると指数バックオフの代わりに固定間隔を使います。遮断と意図の再実行はプロセス単位で、グローバルのページでのみ調整できます。
        </p>
      </div>
      {(["connect", "empty", "stream"] as const).map((k) => (
        <div key={k} className="border-t pt-3 first:border-t-0 first:pt-0">
          <RetryRuleFields
            compact
            layer={k}
            idPrefix="pf"
            value={value[k]}
            onChange={(r) => onChange({ ...value, [k]: r })}
          />
        </div>
      ))}
    </div>
  );
}

/** 「再試行とバックオフ」タブ：5 層のグローバル既定値。 */
export function RetryPolicyPanel() {
  const [policy, setPolicy] = React.useState<LLMRetryPolicy>(ZERO_POLICY);
  const [loading, setLoading] = React.useState(true);
  const [saving, setSaving] = React.useState(false);

  const load = React.useCallback(async () => {
    setLoading(true);
    try {
      const p = await api.llmRetryPolicy();
      setPolicy({ ...ZERO_POLICY, ...p });
    } catch (e) {
      toast.error(`再試行ポリシーの読み込みに失敗しました：${(e as Error).message}`);
    } finally {
      setLoading(false);
    }
  }, []);

  React.useEffect(() => {
    void load();
  }, [load]);

  async function save() {
    if (saving) return;
    setSaving(true);
    try {
      // バックエンドは範囲外の値を範囲内に丸めて返すため、返却値をそのまま使って更新する（見たままが保存内容）。
      const saved = await api.saveLLMRetryPolicy(policy);
      setPolicy({ ...ZERO_POLICY, ...saved });
      toast.success("保存しました。即時反映されます（実行中の呼び出しは旧パラメータのまま）");
    } catch (e) {
      toast.error(`保存に失敗しました：${(e as Error).message}`);
    } finally {
      setSaving(false);
    }
  }

  const set = (k: LayerKey) => (r: LLMRetryRule) => setPolicy((p) => ({ ...p, [k]: r }));

  if (loading) {
    return (
      <div className="flex items-center gap-2 rounded-lg border border-dashed p-10 text-muted-foreground text-sm">
        <Loader2Icon className="size-4 animate-spin" /> 再試行ポリシーを読み込み中…
      </div>
    );
  }

  return (
    <div className="grid gap-4">
      <div className="rounded-lg border bg-muted/30 p-3 text-muted-foreground text-xs leading-relaxed">
        1 回のモデル呼び出しの失敗は、内側から外側へ 5 層の再試行を順に通ります：
        <span className="text-foreground"> 接続確立 → 空レスポンス → 同一 provider 安全ウィンドウ → ポーリング遮断 → 意図の再実行</span>
        。内側を使い切ってから外側に回るため、回数は
        <span className="text-foreground">掛け算</span>
        になる——各層をすべて最大にすると、一度の不調で数十回のリクエストを消費します。
        すべて空欄なら現在の既定値で、このページがないときの動作と完全に一致します。内側 3 層はモデル設定ごとに個別に上書きできます。
      </div>

      <div className="grid gap-3 md:grid-cols-2 xl:grid-cols-3">
        {(Object.keys(RETRY_LAYERS) as LayerKey[]).map((k) => (
          <RetryRuleFields key={k} layer={k} idPrefix="gl" value={policy[k]} onChange={set(k)} />
        ))}
      </div>

      <div className="flex gap-2">
        <Button onClick={save} disabled={saving}>
          {saving ? <Loader2Icon className="animate-spin" /> : <SaveIcon />}
          保存
        </Button>
        <Button variant="outline" onClick={() => setPolicy(ZERO_POLICY)} disabled={saving}>
          すべて既定に戻す
        </Button>
      </div>
    </div>
  );
}
