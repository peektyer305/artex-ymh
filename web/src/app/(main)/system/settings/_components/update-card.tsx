"use client";

import * as React from "react";

import {
  CheckCircle2Icon,
  DownloadIcon,
  ExternalLinkIcon,
  RefreshCwIcon,
  RotateCcwIcon,
  TriangleAlertIcon,
} from "lucide-react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Progress } from "@/components/ui/progress";
import { api, sseUrl } from "@/lib/api";
import { safeHref } from "@/lib/safe-href";
import type { UpdateCheck, UpdateProgress } from "@/lib/types";

/** 新バージョンが立ち上がるまでの最大待ち時間。1 回の更新でプロセスは 3 回起動する（ステージング → 差し替え → 新版）。
 *  いずれも秒単位なので、3 分あれば低速ディスクや Docker コンテナの再作成もカバーできる。 */
const RESTART_TIMEOUT_MS = 180_000;

function humanSize(n?: number): string {
  if (!n || n <= 0) return "";
  const units = ["B", "KB", "MB", "GB"];
  let v = n;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v.toFixed(i === 0 ? 0 : 1)} ${units[i]}`;
}

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

export function UpdateCard() {
  const [info, setInfo] = React.useState<UpdateCheck | null>(null);
  const [checking, setChecking] = React.useState(true);
  const [progress, setProgress] = React.useState<UpdateProgress | null>(null);
  // progress とは別に管理する。ステージングが終わるとプロセスが終了して SSE が切れるため、
  // その時点で /api/health のポーリングに切り替える。
  const [restarting, setRestarting] = React.useState(false);
  const [busy, setBusy] = React.useState(false);

  // quiet はバックエンドキャッシュを迂回するかどうかも決める。ページ表示時の自動チェックはキャッシュを使い（上部バーが直前に確認済み）、
  // ユーザーが「更新を確認」を手動で押したときは強制的に取得し直す。そうしないと公開直後のバージョンはキャッシュの期限まで見えない。
  const check = React.useCallback((quiet = false) => {
    setChecking(true);
    api
      .checkUpdate(!quiet)
      .then((r) => {
        setInfo(r);
        if (!quiet) {
          if (r.error) toast.error("更新の確認に失敗しました：" + r.error);
          else if (r.has_update) toast.success(`新しいバージョンがあります：${r.latest}`);
          else if (r.comparable) toast.success("現在のバージョンが最新です");
        }
      })
      .catch((e) => {
        if (!quiet) toast.error("更新の確認に失敗しました：" + (e as Error).message);
      })
      .finally(() => setChecking(false));
  }, []);

  React.useEffect(() => {
    check(true);
  }, [check]);

  // バージョンが変わるまで /api/health をポーリングする。
  //
  // 判定基準は「バージョンが変わった」ことであって「接続できた」ことではない。差し替え中は旧バージョンが一瞬だけ再起動する
  // （その 1 回は artex.new を差し替えて即座に終了するだけ）。接続性だけ見ると成功と誤判定してしまう。
  const waitForNewVersion = React.useCallback(async (fromVersion: string) => {
    setRestarting(true);
    const deadline = Date.now() + RESTART_TIMEOUT_MS;
    while (Date.now() < deadline) {
      await sleep(2000);
      try {
        const r = await fetch("/api/health", { cache: "no-store" });
        if (r.ok) {
          const j = (await r.json()) as { version?: string };
          if (j.version && j.version !== fromVersion) {
            toast.success(`${j.version} に更新しました。ページを再読み込みします`);
            await sleep(800);
            window.location.reload();
            return;
          }
        }
      } catch {
        // 再起動中は接続できないのが想定内なので、ポーリングを続ける。
      }
    }
    setRestarting(false);
    toast.error("サービスの再起動待ちがタイムアウトしました。バックエンドのログを確認するか、artex が start.sh / start.bat 経由で起動されているか確認してください。");
  }, []);

  // 更新進捗を購読する。SSE は Next の /api リライトを通さない（その層がバッファしてイベントを流せない）。
  // F6: 認証情報は 60 秒のワンタイム ticket。更新中はバックエンドが再起動するため再接続しない（/api/health のポーリングに任せる）。
  const openStream = React.useCallback(
    async (fromVersion: string) => {
      const es = new EventSource(await sseUrl("/api/update/stream"));
      es.onmessage = (ev) => {
        let p: UpdateProgress;
        try {
          p = JSON.parse(ev.data) as UpdateProgress;
        } catch {
          return;
        }
        setProgress(p);
        if (p.phase === "failed") {
          es.close();
          setBusy(false);
          toast.error("更新に失敗しました：" + (p.error || p.message));
          return;
        }
        if (p.phase === "staged") {
          es.close();
          void waitForNewVersion(fromVersion);
        }
      };
      es.onerror = () => {
        // プロセス終了時は SSE が必ず切断される。すでに再起動待ちに入っていれば正常な動作であり、
        // /api/health のポーリングに判定を任せればよい。
        es.close();
      };
      return es;
    },
    [waitForNewVersion],
  );

  const doUpdate = () => {
    if (!info) return;
    const from = info.current;
    const ok = window.confirm(
      `${info.latest} に更新しますか？\n\n` +
        "更新するとプログラムが再起動し、実行中のタスクは中断されます。\n" +
        (info.mode === "docker"
          ? "\n注意：コンテナ内の更新はプログラム本体のみを差し替え、イメージ内の playwright / nmap などのツールチェーンは更新しません。" +
            "新バージョンが新しいツールに依存する場合は docker compose build を使用してください。"
          : ""),
    );
    if (!ok) return;

    setBusy(true);
    setProgress({ phase: "downloading", percent: 0, message: "準備中…" });
    void (async () => {
      let es: EventSource;
      try {
        es = await openStream(from);
      } catch (e) {
        setBusy(false);
        setProgress(null);
        toast.error("更新進捗ストリームの接続に失敗しました：" + (e as Error).message);
        return;
      }
      api.applyUpdate().catch((e) => {
        es.close();
        setBusy(false);
        setProgress(null);
        toast.error("更新の開始に失敗しました：" + (e as Error).message);
      });
    })();
  };

  const doRollback = () => {
    if (!info) return;
    if (
      !window.confirm(
        "前のバージョンにロールバックしますか？\n\nプログラムが再起動し、実行中のタスクは中断されます。\n注意：データベース構造は元に戻らないため、旧バージョンが新版の書き込んだデータを認識できない場合があります。",
      )
    )
      return;
    const from = info.current;
    setBusy(true);
    api
      .rollbackUpdate()
      .then(() => {
        toast.success("前のバージョンに切り替えました。再起動中…");
        void waitForNewVersion(from);
      })
      .catch((e) => {
        setBusy(false);
        toast.error("ロールバックに失敗しました：" + (e as Error).message);
      });
  };

  const phase = progress?.phase;
  const showProgress = busy || restarting;
  // 実際のパーセントが取れるのはダウンロード段階のみ（Content-Length から算出）。検証／展開／再起動待ちは
  // 所要時間が不明な段階なので、プログレスバーを満杯にしてパルスアニメーションを付け「処理中だが完了時期は不明」を表す。
  const downloading = !restarting && phase === "downloading";
  const pct = downloading ? Math.max(progress?.percent ?? 0, 0) : 100;
  // html_url は GitHub API のレスポンス由来。href に入れてよいのは http(s) のみとし、異常値のときはリンクを描画しない。
  const changelogHref = safeHref(info?.html_url);

  return (
    // 設定ページは段組みのウォーターフォールレイアウトなので、カード自身が行間隔を持ち、段の途中で分断されないようにする（page.tsx のコメント参照）。
    <Card className="mb-4 break-inside-avoid md:mb-6">
      <CardHeader>
        <CardTitle className="flex items-center gap-2 text-base">
          <DownloadIcon className="size-4" />
          バージョンと更新
        </CardTitle>
        <CardDescription>GitHub から新しいバージョンを確認してインストールします。更新するとプログラムが再起動し、実行中のタスクは中断されます。</CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        <div className="flex flex-wrap items-center gap-2 text-sm">
          <span className="text-muted-foreground">現在のバージョン</span>
          <Badge variant="secondary" className="font-mono">
            {info?.current ?? "…"}
          </Badge>
          {info && (
            <>
              <Badge variant="outline" className="font-mono">
                {info.os}/{info.arch}
              </Badge>
              <Badge variant="outline">{info.mode === "docker" ? "Docker" : "独立プログラム"}</Badge>
            </>
          )}
          {info?.latest && (
            <>
              <span className="text-muted-foreground">最新バージョン</span>
              <Badge variant={info.has_update ? "default" : "secondary"} className="font-mono">
                {info.latest}
              </Badge>
            </>
          )}
          {changelogHref && (
            <a
              href={changelogHref}
              target="_blank"
              rel="noreferrer"
              className="inline-flex items-center gap-1 text-xs text-muted-foreground underline-offset-4 hover:underline"
            >
              変更履歴 <ExternalLinkIcon className="size-3" />
            </a>
          )}
        </div>

        {info?.boot_notice && (
          <p className="flex items-start gap-2 rounded-md border border-amber-500/40 bg-amber-500/10 p-2 text-xs text-amber-700 dark:text-amber-400">
            <TriangleAlertIcon className="mt-0.5 size-3.5 shrink-0" />
            {info.boot_notice}
          </p>
        )}

        {info?.error && (
          <p className="flex items-start gap-2 rounded-md border border-destructive/40 bg-destructive/10 p-2 text-xs text-destructive">
            <TriangleAlertIcon className="mt-0.5 size-3.5 shrink-0" />
            GitHub に接続できません：{info.error}
            {"　"}上でグローバルプロキシを設定してから再試行してください。
          </p>
        )}

        {info && !info.comparable && info.reason && <p className="text-xs text-muted-foreground">{info.reason}</p>}

        {info?.has_update && info.asset_available === false && (
          <p className="flex items-start gap-2 rounded-md border border-destructive/40 bg-destructive/10 p-2 text-xs text-destructive">
            <TriangleAlertIcon className="mt-0.5 size-3.5 shrink-0" />
            バージョン {info.latest} の {info.os}/{info.arch} 向けリリースパッケージが見つかりません（{info.asset} が欠落）。自動更新できません。
          </p>
        )}

        {info?.has_update && info.asset_available !== false && (
          <p className="text-xs text-muted-foreground">
            <span className="font-mono">{info.asset}</span>
            {info.size ? `（${humanSize(info.size)}）` : ""}をダウンロードし、SHA256 を検証してスモークテスト後に差し替えます。失敗時は現在のバージョンを保持します。
          </p>
        )}

        {info && !info.has_update && info.comparable && !info.error && (
          <p className="flex items-center gap-2 text-xs text-muted-foreground">
            <CheckCircle2Icon className="size-3.5 text-emerald-600" />
            現在のバージョンが最新です。
          </p>
        )}

        {info?.mode === "docker" && info.has_update && (
          <p className="text-xs text-muted-foreground">
            Docker での更新はプログラム本体のみを差し替え、イメージ内の playwright / nmap などのツールチェーンは更新しません。また
            <span className="font-mono"> docker compose up -d </span>
            でコンテナを再作成するとイメージ同梱のバージョンに戻ります。イメージごとアップグレードする場合は
            <span className="font-mono"> docker compose build artex &amp;&amp; docker compose up -d artex</span>
            を実行してください。
          </p>
        )}

        {showProgress && (
          <div className="space-y-1.5">
            <Progress value={pct} className={downloading ? undefined : "animate-pulse"} />
            <p className="text-xs text-muted-foreground">
              {restarting ? "再起動して新バージョンを適用中です。しばらくお待ちください（ページは自動で再読み込みされます）…" : progress?.message}
            </p>
          </div>
        )}

        <div className="flex flex-wrap gap-2">
          <Button variant="outline" size="sm" onClick={() => check(false)} disabled={checking || busy || restarting}>
            <RefreshCwIcon className={checking ? "size-4 animate-spin" : "size-4"} />
            更新を確認
          </Button>
          <Button
            size="sm"
            onClick={doUpdate}
            disabled={busy || restarting || !info?.has_update || info?.asset_available === false}
          >
            <DownloadIcon className="size-4" />
            {info?.has_update ? `${info.latest} に更新` : "今すぐ更新"}
          </Button>
          {info?.has_backup && (
            <Button variant="ghost" size="sm" onClick={doRollback} disabled={busy || restarting}>
              <RotateCcwIcon className="size-4" />
              前のバージョンに戻す
            </Button>
          )}
        </div>

        <p className="text-xs text-muted-foreground">
          ワンクリック更新は監視スクリプトによる再起動に依存します。<span className="font-mono">start.sh</span>（Windows では
          <span className="font-mono"> start.bat</span>）経由で ARTEX を起動してください。artex 本体を直接実行した場合、終了後に自動で起動し直されません。
        </p>
      </CardContent>
    </Card>
  );
}
