"use client";

import * as React from "react";

import { CpuIcon, KeyboardIcon, RadioTowerIcon, SearchIcon, ShieldAlertIcon } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";
import { api } from "@/lib/api";
import { CHAT_SEND_MODE_OPTIONS, type ChatSendMode, setChatSendMode, useChatSendMode } from "@/lib/chat-send-mode";
import type { Settings } from "@/lib/types";

import { UpdateCard } from "./_components/update-card";

export default function SystemSettingsPage() {
  const [trafficCapture, setTrafficCapture] = React.useState(false);
  const [agentTrafficBinding, setAgentTrafficBinding] = React.useState(false);
  const [webSearch, setWebSearch] = React.useState(false);
  const [backend, setBackend] = React.useState("ddgs");
  const [braveKeySet, setBraveKeySet] = React.useState(false);
  const [braveKeyInput, setBraveKeyInput] = React.useState("");
  const [tavilyKeySet, setTavilyKeySet] = React.useState(false);
  const [tavilyKeyInput, setTavilyKeyInput] = React.useState("");
  const [savingTavilyKey, setSavingTavilyKey] = React.useState(false);
  const [proxyInput, setProxyInput] = React.useState("");
  const [savingProxy, setSavingProxy] = React.useState(false);
  const [globalProxyInput, setGlobalProxyInput] = React.useState("");
  const [savingGlobalProxy, setSavingGlobalProxy] = React.useState(false);
  const [testing, setTesting] = React.useState(false);
  const [loaded, setLoaded] = React.useState(false);
  const [saving, setSaving] = React.useState(false);
  const [savingKey, setSavingKey] = React.useState(false);
  const [pyInterp, setPyInterp] = React.useState("");
  const [workers, setWorkers] = React.useState("3");
  const [savingWorkers, setSavingWorkers] = React.useState(false);
  // 操作制約の注入範囲（デフォルトはすべてオン）。
  const [injectPlanner, setInjectPlanner] = React.useState(true);
  const [injectWorker, setInjectWorker] = React.useState(true);
  // フロントエンドのみの設定。/api/settings を経由せず localStorage を直接読み書きする。
  const sendMode = useChatSendMode();

  const apply = React.useCallback((s: Settings) => {
    setTrafficCapture(!!s.traffic_capture);
    setAgentTrafficBinding(!!s.agent_traffic_binding);
    setWebSearch(!!s.web_search_enabled);
    setBackend(s.web_search_backend || "ddgs");
    setBraveKeySet(!!s.brave_key_set);
    setTavilyKeySet(!!s.tavily_key_set);
    setProxyInput(s.web_search_proxy ?? "");
    setGlobalProxyInput(s.global_proxy ?? "");
    setPyInterp(s.python_interpreter ?? "");
    setWorkers(String(s.workers ?? 3));
    setInjectPlanner(s.constraints_inject_planner !== false);
    setInjectWorker(s.constraints_inject_worker !== false);
  }, []);

  const saveWorkers = () => {
    const n = Number(workers);
    if (!Number.isInteger(n) || n <= 0) {
      toast.error("同時実行数は 0 より大きい整数にしてください");
      return;
    }
    setSavingWorkers(true);
    api
      .setSettings({ workers: n })
      .then((s) => {
        apply(s);
        toast.success("作業 agent の同時実行数を保存しました（以後に起動するタスクに適用）");
      })
      .catch((e) => toast.error("保存に失敗しました：" + (e as Error).message))
      .finally(() => setSavingWorkers(false));
  };

  const savePython = () => {
    setSaving(true);
    api
      .setSettings({ python_interpreter: pyInterp.trim() })
      .then((s) => {
        apply(s);
        toast.success("Python インタプリタ設定を保存しました");
      })
      .catch((e) => toast.error("保存に失敗しました：" + (e as Error).message))
      .finally(() => setSaving(false));
  };
  const detectPython = () => {
    setSaving(true);
    api
      .detectPython()
      .then((r) => setPyInterp(r.python_interpreter))
      .catch(() => undefined)
      .finally(() => setSaving(false));
  };

  React.useEffect(() => {
    api
      .settings()
      .then(apply)
      .catch(() => undefined)
      .finally(() => setLoaded(true));
  }, [apply]);

  const toggleTraffic = (v: boolean) => {
    setTrafficCapture(v); // optimistic
    setSaving(true);
    api
      .setSettings({ traffic_capture: v })
      .then(apply)
      .catch(() => setTrafficCapture(!v)) // revert on failure
      .finally(() => setSaving(false));
  };

  const toggleInjectPlanner = (v: boolean) => {
    setInjectPlanner(v); // optimistic
    api
      .setSettings({ constraints_inject_planner: v })
      .then(apply)
      .catch(() => setInjectPlanner(!v)); // revert on failure
  };

  const toggleAgentTrafficBinding = (v: boolean) => {
    setAgentTrafficBinding(v);
    setSaving(true);
    api
      .setSettings({ agent_traffic_binding: v })
      .then((s) => {
        apply(s);
        toast.success(v ? "Agent の自動トラフィック紐付けをオンにしました" : "Agent の自動トラフィック紐付けをオフにしました");
      })
      .catch((e) => {
        setAgentTrafficBinding(!v);
        toast.error(`保存に失敗しました：${(e as Error).message}`);
      })
      .finally(() => setSaving(false));
  };

  const toggleInjectWorker = (v: boolean) => {
    setInjectWorker(v); // optimistic
    api
      .setSettings({ constraints_inject_worker: v })
      .then(apply)
      .catch(() => setInjectWorker(!v)); // revert on failure
  };

  // Persist a web-search patch (enable and/or backend). Optimistic with refetch.
  const saveWebSearch = (patch: Partial<Settings>) => {
    setSaving(true);
    api
      .setSettings(patch)
      .then((s) => {
        apply(s);
        toast.success("Web 検索設定を保存しました");
      })
      .catch((e) => {
        toast.error("保存に失敗しました：" + (e as Error).message);
        api
          .settings()
          .then(apply)
          .catch(() => undefined);
      })
      .finally(() => setSaving(false));
  };

  const saveBraveKey = () => {
    setSavingKey(true);
    api
      .setSettings({ brave_search_api_key: braveKeyInput })
      .then((s) => {
        apply(s);
        setBraveKeyInput("");
        toast.success("Brave API Key を保存しました");
      })
      .catch((e) => toast.error("保存に失敗しました：" + (e as Error).message))
      .finally(() => setSavingKey(false));
  };

  const saveTavilyKey = () => {
    setSavingTavilyKey(true);
    api
      .setSettings({ tavily_search_api_key: tavilyKeyInput })
      .then((s) => {
        apply(s);
        setTavilyKeyInput("");
        toast.success("Tavily API Key を保存しました");
      })
      .catch((e) => toast.error("保存に失敗しました：" + (e as Error).message))
      .finally(() => setSavingTavilyKey(false));
  };

  const saveProxy = () => {
    setSavingProxy(true);
    api
      .setSettings({ web_search_proxy: proxyInput.trim() })
      .then((s) => {
        apply(s);
        toast.success(proxyInput.trim() ? "出口プロキシを保存しました" : "出口プロキシをクリアしました（直接接続に変更）");
      })
      .catch((e) => toast.error("保存に失敗しました：" + (e as Error).message))
      .finally(() => setSavingProxy(false));
  };

  const saveGlobalProxy = () => {
    setSavingGlobalProxy(true);
    api
      .setSettings({ global_proxy: globalProxyInput.trim() })
      .then((s) => {
        apply(s);
        toast.success(globalProxyInput.trim() ? "グローバルプロキシを保存しました" : "グローバルプロキシをクリアしました（直接接続に変更）");
      })
      .catch((e) => toast.error("保存に失敗しました：" + (e as Error).message))
      .finally(() => setSavingGlobalProxy(false));
  };

  // Run a real "test" search ("test") against the CURRENT form values (backend +
  // proxy + entered key), falling back to saved values server-side. Toasts result.
  const runTest = () => {
    setTesting(true);
    api
      .testWebSearch({
        web_search_backend: backend,
        web_search_proxy: proxyInput.trim(),
        brave_search_api_key: braveKeyInput,
        tavily_search_api_key: tavilyKeyInput,
      })
      .then((r) => {
        if (r.ok) toast.success(`検索テスト成功 · ${r.backend} が ${r.count} 件の結果を返しました`);
        else toast.error("検索テストに失敗しました：" + (r.error || "不明なエラー"));
      })
      .catch((e) => toast.error("検索テストに失敗しました：" + (e as Error).message))
      .finally(() => setTesting(false));
  };

  // brave-free selected but no key stored and none being entered → tool stays off.
  const braveNeedsKey = webSearch && backend === "brave-free" && !braveKeySet;

  return (
    <div className="flex flex-1 flex-col gap-4 md:gap-6">
      <div>
        <h1 className="text-xl font-semibold tracking-tight">システム設定</h1>
        <p className="text-muted-foreground text-sm">グローバルな実行時スイッチ</p>
      </div>

      {/* grid ではなく段組み：Web 検索カードは他より数倍高く、高さも選択中のバックエンドで変化する
          （brave/tavily の key 入力は条件付きレンダリング）。grid だと最も高いカードで行全体が埋まり、
          横に大きな空白が残る。段組みなら内容の高さに応じて自動的に均等に埋まる。カード間隔は gap ではなく
          mb で取る——段組みでは column-gap は列間隔のみで、行間隔は子要素側で指定する必要がある。 */}
      <div className="columns-1 gap-4 md:gap-6 lg:columns-2">
        <UpdateCard />

        <Card className="mb-4 break-inside-avoid md:mb-6">
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-base">
              <RadioTowerIcon className="size-4" />
              流量キャプチャ
            </CardTitle>
            <CardDescription>
              オンにすると、すべての Agent の HTTP トラフィックを記録プロキシ経由で漏れなく DB に保存し、Agent に traffic_search / traffic_get
              ツールとプロキシ設定を注入します（プロンプトにプロキシの説明を含む）。
              <br />
              オフ（デフォルト）のときはトラフィックを一切記録しません。Agent は
              プロキシ設定とトラフィックツールを<b>受け取らず</b>、プロンプトにもプロキシ関連の内容を<b>含みません</b>。切り替え後は Agent を即時再構築して反映します。
            </CardDescription>
          </CardHeader>
          <CardContent className="flex items-center justify-between gap-4">
            <Label htmlFor="traffic-capture" className="text-sm font-normal text-muted-foreground">
              {trafficCapture ? "オン · トラフィックを記録しプロキシも注入中" : "オフ · 記録もプロキシ注入もなし"}
            </Label>
            <Switch
              id="traffic-capture"
              checked={trafficCapture}
              disabled={!loaded || saving}
              onCheckedChange={toggleTraffic}
            />
          </CardContent>
        </Card>

        <Card className="mb-4 break-inside-avoid md:mb-6">
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-base">
              <RadioTowerIcon className="size-4" />
              Agent の自動トラフィック紐付け
            </CardTitle>
            <CardDescription id="agent-traffic-binding-description">
              デフォルトはオフ。オンにすると、脆弱性の登録時に起動するレポート Agent が既存の HTTP リクエスト/レスポンスを照合し、対応するトラフィックを紐付けてからレポートを作成します。
              <b>パケットの確認と追加のツール呼び出しにより Token 消費が増えます。</b>
              <br />
              TCP、未キャプチャ、または一致するトラフィックがない場合も通常どおり報告できます。このスイッチはトラフィックキャプチャ、手動紐付け、保存済みエビデンスの閲覧には影響しません。次ターンの Agent から
              有効になります。オフにすると新しい自動紐付けは即座に拒否されます。
            </CardDescription>
          </CardHeader>
          <CardContent className="flex items-center justify-between gap-4">
            <Label htmlFor="agent-traffic-binding" className="text-sm font-normal text-muted-foreground">
              {agentTrafficBinding ? "オン · Token 消費が増えます" : "オフ · 手動で紐付け可能"}
            </Label>
            <Switch
              id="agent-traffic-binding"
              aria-describedby="agent-traffic-binding-description"
              checked={agentTrafficBinding}
              disabled={!loaded || saving}
              onCheckedChange={toggleAgentTrafficBinding}
            />
          </CardContent>
        </Card>

        <Card className="mb-4 break-inside-avoid md:mb-6">
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-base">
              <RadioTowerIcon className="size-4" />
              グローバルプロキシ
            </CardTitle>
            <CardDescription>
              すべての Agent の<b>対象トラフィック</b>をこのプロキシ経由で外部へ出します（送信元 IP を隠す／踏み台経由）。<b>http / https / socks5</b> に対応し、{" "}
              <code>user:pass</code> 認証も可能です。空欄=直接接続。
              <br />
              <b>トラフィックキャプチャ</b>がオンのときは記録プロキシの<b>上流</b>として動作します（トラフィックは漏れなく DB に保存されたうえで、このプロキシ経由で外部へ出ます）。キャプチャがオフのときは
              Agent の bash / WebFetch に直接注入して外部へ出します。Web 検索プロキシ、LLM プロキシとは独立しています。
              <br />
              <b>ヒント</b>：socks5 は<b>キャプチャがオフ</b>のとき、各コマンドラインツールの <code>ALL_PROXY</code> 対応に依存します（curl
              は可、一部ツールは無視する場合あり）。 socks5 を主に使う場合はトラフィックキャプチャの有効化を推奨します——この経路は MITM
              自身が接続するため、ツールに意識させず安定して動作します。
            </CardDescription>
          </CardHeader>
          <CardContent className="flex flex-col gap-2">
            <Label htmlFor="global-proxy" className="text-sm font-normal text-muted-foreground">
              プロキシアドレス
            </Label>
            <div className="flex items-center gap-2">
              <Input
                id="global-proxy"
                autoComplete="off"
                placeholder="socks5://user:pass@host:1080 または http://host:port（空欄=直接接続）"
                value={globalProxyInput}
                disabled={!loaded || savingGlobalProxy}
                onChange={(e) => setGlobalProxyInput(e.target.value)}
              />
              <Button type="button" onClick={saveGlobalProxy} disabled={!loaded || savingGlobalProxy}>
                保存
              </Button>
            </div>
            <p className="text-muted-foreground text-xs">
              {globalProxyInput.trim() ? "設定済み · すべての対象トラフィックがこのプロキシ経由で外部へ" : "未設定 · 対象トラフィックは直接外部へ"}
            </p>
          </CardContent>
        </Card>

        <Card className="mb-4 break-inside-avoid md:mb-6">
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-base">
              <ShieldAlertIcon className="size-4" />
              操作制約の注入
            </CardTitle>
            <CardDescription>
              オンにすると、各タスクの<b>操作制約</b>（タスク概要の「操作制約」で管理している allow/deny 項目）を対応する Agent
              のシステムプロンプトに組み込み、探索の境界を定めます（例：「現在のポートのみテスト」「ブルートフォース禁止」）。
              <br />
              注入先は <b>プランナー（planner）</b>と <b>ワーカー（worker）</b>を個別に制御
              できます。デフォルトは両方オン。切り替えは即時反映され（次のターンで読み込み）、Agent の再構築は不要です。オフにするとその Agent は制約を参照しなくなります。
            </CardDescription>
          </CardHeader>
          <CardContent className="flex flex-col gap-4">
            <div className="flex items-center justify-between gap-4">
              <Label htmlFor="inject-planner" className="text-sm font-normal text-muted-foreground">
                プランナー（planner）への注入{injectPlanner ? " · オン" : " · オフ"}
              </Label>
              <Switch
                id="inject-planner"
                checked={injectPlanner}
                disabled={!loaded}
                onCheckedChange={toggleInjectPlanner}
              />
            </div>
            <div className="flex items-center justify-between gap-4">
              <Label htmlFor="inject-worker" className="text-sm font-normal text-muted-foreground">
                ワーカー（worker）への注入{injectWorker ? " · オン" : " · オフ"}
              </Label>
              <Switch
                id="inject-worker"
                checked={injectWorker}
                disabled={!loaded}
                onCheckedChange={toggleInjectWorker}
              />
            </div>
          </CardContent>
        </Card>

        <Card className="mb-4 break-inside-avoid md:mb-6">
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-base">
              <SearchIcon className="size-4" />
              Web 検索
            </CardTitle>
            <CardDescription>
              ここは Web 検索の<b>総スイッチ + ソース設定</b>です。オンにすると、<b>各 Agent の設定</b>で個別に
              <b>web_search</b> を有効にするか選べます（タイトル/リンク/要約のみ返し、本文は取得しません。取得は WebFetch が担当）。Web 検索は記録プロキシを<b>経由せず</b>、
              トラフィックキャプチャとは独立しています。
              <br />
              ソースは <b>DuckDuckGo（ddgs）</b>（Key 不要）、<b>Brave（無料版）</b>（Brave API Key の入力が必要）、{" "}
              <b>Tavily</b>（Tavily API Key の入力が必要）、<b>DeepSeek</b>（現在の LLM 設定を再利用）から選択できます。総スイッチがオフの間は、各
              Agent の Web 検索スイッチは使用できません。
            </CardDescription>
          </CardHeader>
          <CardContent className="flex flex-col gap-4">
            <div className="flex items-center justify-between gap-4">
              <Label htmlFor="web-search" className="text-sm font-normal text-muted-foreground">
                {webSearch ? "総スイッチはオン · 各 Agent 設定で個別に有効化可能" : "オフ · 各 Agent は Web 検索を有効化できません"}
              </Label>
              <Switch
                id="web-search"
                checked={webSearch}
                disabled={!loaded || saving}
                onCheckedChange={(v) => {
                  setWebSearch(v); // optimistic
                  saveWebSearch({ web_search_enabled: v });
                }}
              />
            </div>

            {webSearch && (
              <div className="flex items-center justify-between gap-4">
                <Label className="text-sm font-normal text-muted-foreground">検索ソース</Label>
                <Select
                  value={backend}
                  disabled={!loaded || saving}
                  onValueChange={(v) => {
                    setBackend(v); // optimistic
                    saveWebSearch({ web_search_backend: v });
                  }}
                >
                  <SelectTrigger className="w-48 shrink-0">
                    <SelectValue placeholder="ソースを選択" />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="ddgs">DuckDuckGo（ddgs · 無料、Key 不要）</SelectItem>
                    <SelectItem value="brave-free">Brave（無料版 · Key 必須）</SelectItem>
                    <SelectItem value="tavily">Tavily（Key 必須）</SelectItem>
                    <SelectItem value="deepseek">DeepSeek（公式）</SelectItem>
                  </SelectContent>
                </Select>
              </div>
            )}

            {webSearch && backend === "deepseek" && (
              <div className="border-border/60 bg-muted/30 flex flex-col gap-2 rounded-md border p-3">
                <p className="text-sm font-medium">DeepSeek 公式の Web 検索</p>
                <p className="text-muted-foreground text-xs leading-relaxed">
                  このソースは<b>現在アクティブな LLM 設定</b>をそのまま再利用します。そのため
                  <b>DeepSeek 公式モデルのみ対応</b>し、その設定は<b>anthropic プロトコル必須</b>
                  です——DeepSeek の OpenAI プロトコルのエンドポイントはサーバー側検索に対応していません。LLM 設定を切り替えるとこのソースは使えなくなる場合があります。
                </p>
                <p className="text-muted-foreground text-xs leading-relaxed">
                  他のソースと異なり、検索は <b>DeepSeek のサーバー側で実行</b>されます。検索のたびにモデル呼び出しが 1 回増え（Token
                  費用が発生）、検索リクエストは<b>上記の出口プロキシを経由せず</b>、<b>トラフィックにも記録されません</b>。返却される結果は<b>タイトルとリンクのみ</b>
                  （要約なし）で、本文が必要な場合は WebFetch が取得します。
                </p>
                <p className="text-muted-foreground text-xs leading-relaxed">
                  上記の条件を満たすかはご自身で確認してください。システム側のチェックは行いません。下の「テスト検索」ボタンで実際に 1 回実行して確認できます。
                </p>
              </div>
            )}

            {webSearch && backend === "brave-free" && (
              <div className="flex flex-col gap-2">
                <Label htmlFor="brave-key" className="text-sm font-normal text-muted-foreground">
                  Brave Search API Key
                  {braveKeySet && <span className="ml-2 text-xs text-emerald-500">設定済み</span>}
                </Label>
                <div className="flex items-center gap-2">
                  <Input
                    id="brave-key"
                    type="password"
                    autoComplete="off"
                    placeholder={braveKeySet ? "設定済み（空欄なら変更なし）" : "Brave API Key を入力"}
                    value={braveKeyInput}
                    disabled={!loaded || savingKey}
                    onChange={(e) => setBraveKeyInput(e.target.value)}
                  />
                  <Button
                    type="button"
                    onClick={saveBraveKey}
                    disabled={!loaded || savingKey || braveKeyInput.trim() === ""}
                  >
                    保存
                  </Button>
                </div>
                {braveNeedsKey && (
                  <p className="text-xs text-amber-500">
                    Brave を選択しましたが Key が未設定です —— Key を保存するまで検索ツールは有効になりません。
                  </p>
                )}
                <p className="text-muted-foreground text-xs">
                  無料版の枠は月約 2,000 回です。Key の取得は https://brave.com/search/api/ へ。
                </p>
              </div>
            )}

            {webSearch && backend === "tavily" && (
              <div className="flex flex-col gap-2">
                <Label htmlFor="tavily-key" className="text-sm font-normal text-muted-foreground">
                  Tavily Search API Key
                  {tavilyKeySet && <span className="ml-2 text-xs text-emerald-500">設定済み</span>}
                </Label>
                <div className="flex items-center gap-2">
                  <Input
                    id="tavily-key"
                    type="password"
                    autoComplete="off"
                    placeholder={tavilyKeySet ? "設定済み（空欄なら変更なし）" : "Tavily API Key を入力（tvly-…）"}
                    value={tavilyKeyInput}
                    disabled={!loaded || savingTavilyKey}
                    onChange={(e) => setTavilyKeyInput(e.target.value)}
                  />
                  <Button
                    type="button"
                    onClick={saveTavilyKey}
                    disabled={!loaded || savingTavilyKey || tavilyKeyInput.trim() === ""}
                  >
                    保存
                  </Button>
                </div>
                {webSearch && backend === "tavily" && !tavilyKeySet && (
                  <p className="text-xs text-amber-500">
                    Tavily を選択しましたが Key が未設定です —— Key を保存するまで検索ツールは有効になりません。
                  </p>
                )}
                <p className="text-muted-foreground text-xs">https://tavily.com で登録して API Key を取得してください。</p>
              </div>
            )}

            {webSearch && (
              <div className="flex flex-col gap-2">
                <Label htmlFor="ws-proxy" className="text-sm font-normal text-muted-foreground">
                  出口プロキシ（任意）
                </Label>
                <div className="flex items-center gap-2">
                  <Input
                    id="ws-proxy"
                    autoComplete="off"
                    placeholder="http://host:port または socks5://host:port（空欄=直接接続）"
                    value={proxyInput}
                    disabled={!loaded || savingProxy}
                    onChange={(e) => setProxyInput(e.target.value)}
                  />
                  <Button type="button" onClick={saveProxy} disabled={!loaded || savingProxy}>
                    保存
                  </Button>
                </div>
                <p className="text-muted-foreground text-xs">
                  独立した出口プロキシで、検索エンドポイントへのアクセスにのみ使用します（VPN/SOCKS など）。トラフィックを記録する MITM プロキシとは無関係です。ネットワークが通らない場合はこのプロキシ経由でアクセスします。
                </p>
              </div>
            )}

            {webSearch && (
              <div className="flex items-center justify-between gap-4 border-t pt-4">
                <p className="text-muted-foreground text-xs">
                  現在の設定（ソース + プロキシ + Key）で実際に「test」を 1 回検索し、利用できるか検証します。
                </p>
                <Button
                  type="button"
                  variant="outline"
                  onClick={runTest}
                  disabled={!loaded || testing}
                  className="shrink-0"
                >
                  {testing ? "テスト中…" : "テスト検索"}
                </Button>
              </div>
            )}
          </CardContent>
        </Card>

        <Card className="mb-4 break-inside-avoid md:mb-6">
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-base">
              <RadioTowerIcon className="size-4" />
              カスタムスクリプト · Python インタプリタ
            </CardTitle>
            <CardDescription>
              カスタムの <b>script</b> タイプツールがこれで Python を実行します。起動時に自動検出（python3 を優先）。ここには venv や
              特定バージョンの絶対パスを手入力でき、空欄なら実行時に自動検出します。
            </CardDescription>
          </CardHeader>
          <CardContent className="flex flex-col gap-3">
            <div className="flex items-center gap-2">
              <Input
                className="font-mono text-sm"
                placeholder="/usr/bin/python3（空欄=自動検出）"
                value={pyInterp}
                disabled={!loaded || saving}
                onChange={(e) => setPyInterp(e.target.value)}
              />
              <Button variant="outline" onClick={detectPython} disabled={!loaded || saving}>
                再検出
              </Button>
              <Button onClick={savePython} disabled={!loaded || saving}>
                保存
              </Button>
            </div>
          </CardContent>
        </Card>

        <Card className="mb-4 break-inside-avoid md:mb-6">
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-base">
              <CpuIcon className="size-4" />
              作業並列度 · Work Agent 数
            </CardTitle>
            <CardDescription>
              タスクごとに並列実行する作業 agent 数（デフォルト 3）。値を大きくすると並列探索が増え、消費も増えます。変更は
              <b>以後に起動するタスクに適用</b>され、実行中のタスクには影響しません。
            </CardDescription>
          </CardHeader>
          <CardContent className="flex flex-col gap-3">
            <div className="flex items-center gap-2">
              <Input
                type="number"
                min={1}
                className="w-32 font-mono text-sm"
                placeholder="3"
                value={workers}
                disabled={!loaded || savingWorkers}
                onChange={(e) => setWorkers(e.target.value)}
              />
              <Button onClick={saveWorkers} disabled={!loaded || savingWorkers}>
                保存
              </Button>
            </div>
          </CardContent>
        </Card>

        <Card className="mb-4 break-inside-avoid md:mb-6">
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-base">
              <KeyboardIcon className="size-4" />
              セッション入力欄の送信キー
            </CardTitle>
            <CardDescription>
              チャットページとタスク詳細のメイン Agent セッション入力欄がこの設定を共用します。選択後は即時反映され、保存は不要です。
              <br />
              この設定は<b>このブラウザにのみ保存</b>され、アカウント同期はされません。ブラウザを変更したりサイトデータを消去したりすると再設定が必要です。
            </CardDescription>
          </CardHeader>
          <CardContent className="flex items-center justify-between gap-4">
            <Label htmlFor="chat-send-mode" className="text-sm font-normal text-muted-foreground">
              送信方法
            </Label>
            <Select value={sendMode} onValueChange={(v) => setChatSendMode(v as ChatSendMode)}>
              <SelectTrigger id="chat-send-mode" className="w-72">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {CHAT_SEND_MODE_OPTIONS.map((option) => (
                  <SelectItem key={option.value} value={option.value}>
                    {option.label}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </CardContent>
        </Card>
      </div>
    </div>
  );
}
