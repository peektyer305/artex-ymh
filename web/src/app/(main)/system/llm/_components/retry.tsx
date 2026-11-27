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
  /** どんなエラーがこの層に来るか。ステータスコードまで具体的に、推測させない */
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
    where: "HTTP 200を受け取る前",
    trigger:
      "HTTP 200を受け取る前の接続リセット、読み書きのタイムアウト、DNSエラーと、HTTP 408・429・500・502・503・504が対象です。",
    skips: "HTTP 400・401・403・404・413・422など、同じリクエストを再送しても解消しないエラーは、この段階では再試行せず、呼び出し元へ返します。",
    desc: "同じリクエストを再送します。HTTP 200を受け取った後の切断は、次の段階で処理します。",
    attemptsLabel: "再試行回数",
    defAttempts: 3,
    defInterval: "0.5s→1s→2s 指数（上限 8s）",
    offHint: "-1：再試行せず、エラーを返します",
  },
  empty: {
    title: "空レスポンスの再試行",
    where: "OpenAI互換形式のみ",
    trigger:
      "HTTP 200で、finish_reasonがstopなのに、応答内容のブロックが1つもない場合が対象です。ゲートウェイの空フレームや思考情報の欠落などで起こります。",
    skips: "出力上限に達して内容がない場合は対象外です。この場合は、出力トークン数の上限を見直してください。",
    desc: "プロンプト全体を再送します。長い会話では使用量が増えるため、再試行回数を増やしすぎないようにしてください。",
    attemptsLabel: "再試行回数",
    defAttempts: 2,
    defInterval: "0.5s→1s→2s 指数（上限 8s）",
    offHint: "-1 = 空レスポンスをそのまま返す",
  },
  stream: {
    title: "応答開始前の再試行",
    where: "出力を受け取る前",
    trigger:
      "HTTP 200を受け取った後の切断、プロバイダーの過負荷、ストリーム内の429・5xxエラーが対象です。まだ出力トークンを呼び出し元へ渡していない場合に限ります。",
    skips:
      "残高・利用枠の不足（402 / insufficient_quota）は設定の切り替えで、コンテキスト長の超過（413 / context length）は圧縮で処理します。HTTP 400・401・403・404・422も、この段階では再試行しません。",
    desc: "同じ設定で同じリクエストを再送します。まだ出力を渡していない段階なので、表示やツール実行の重複を避けられます。",
    attemptsLabel: "再試行回数",
    defAttempts: 2,
    defInterval: "0.5s→1s 指数（上限 4s）",
    offHint: "-1：この段階での再試行を止め、実行指示の再実行に任せます",
  },
  breaker: {
    title: "障害時の一時停止",
    where: "全体で共通",
    trigger:
      "429・5xx・ネットワークエラーが指定回数続いたら、この設定を一時停止します。残高不足（402）、無効なAPIキー（401・403）、存在しないモデル（404）は、1回目で停止します。",
    skips: "一度成功すると連続失敗回数は0に戻ります。",
    desc: "一時停止中は、この設定を自動切り替えの対象から外します。停止状態はデータベースに保存されるため、再起動後も維持されます。",
    attemptsLabel: "一時停止までの連続失敗回数",
    defAttempts: 3,
    defInterval: "1min→5min→30min の段階",
    offHint: "-1：一時的な失敗では停止しません。確定的なエラーでは停止します",
  },
  intent: {
    title: "実行指示の再実行",
    where: "全体で共通",
    trigger:
      "接続や応答の再試行で復旧できず、ワーカーがmodel_errorで終了した場合が対象です。出力が始まった後に切断された場合も、実行指示を最初からやり直します。",
    skips: "残高・利用枠の不足は設定の切り替えで処理するため、ここでは再実行しません。タスクが一時停止、終了、終了処理に入ったら、待機を打ち切ります。",
    desc: "実行指示を最初からやり直します。再実行のたびに、内側の接続や応答の再試行も行います。",
    attemptsLabel: "再実行回数",
    defAttempts: 2,
    defInterval: "固定 3s",
    offHint: "-1：再実行せず、実行指示をブロック状態にします",
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
        {/* どのエラーがこの層に来るか、ステータスコードまで具体的に。つまみを設定しても効かないときは、そもそもこの層に来ていないことが多い。*/}
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
          このLLM設定にだけ適用し、全体の「再試行とバックオフ」設定より優先します。空欄は全体設定を使い、回数の-1はその段階の再試行を止めます。間隔を指定すると、指数バックオフの代わりに固定の待ち時間を使います。設定の一時停止と実行指示の再実行は、全体の設定画面で変更します。
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
        モデル呼び出しに失敗した場合は、次の順で対処します。
        <span className="text-foreground"> 接続 → 空の応答 → 応答開始前の再試行 → 設定の一時停止 → 実行指示の再実行</span>
        。再試行を使い切っても復旧しない場合は、設定を一時停止するか、実行指示をやり直します。再試行の回数設定によっては、
        <span className="text-foreground">一度の不調で数十回</span>
        リクエストします。すべて空欄の場合は既定値を使います。最初の3段階はLLM設定ごとに変更できます。
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
