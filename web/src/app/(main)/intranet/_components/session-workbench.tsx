"use client";

import * as React from "react";

import { FileIcon, FolderIcon, Loader2Icon, PlayIcon, RefreshCwIcon, Trash2Icon } from "lucide-react";
import { toast } from "sonner";

import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from "@/components/ui/sheet";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Textarea } from "@/components/ui/textarea";
import { api } from "@/lib/api";
import type { SessionFsEntry, ShellSession } from "@/lib/types";
import { cn } from "@/lib/utils";

import { fmtBytes } from "./format";

const TIMEOUT_OPTIONS = [10, 30, 60, 120];

// UTF-8 セーフな base64 エンコード・デコード（btoa/atob は Latin-1 のみ対応）。
function toBase64(s: string): string {
  return btoa(String.fromCharCode(...new TextEncoder().encode(s)));
}
function fromBase64(b64: string): Uint8Array {
  return Uint8Array.from(atob(b64), (c) => c.charCodeAt(0));
}

// ---------------------------------------------------------------------------
// コマンドターミナル：履歴はコンポーネント状態に保持（セッション切替時は key でリセット）、出力は等幅フォント。
// ---------------------------------------------------------------------------
interface TermEntry {
  seq: number;
  cmd: string;
  stdout?: string;
  stderr?: string;
  ms?: number;
  timedOut?: boolean;
  /** HTTP リクエスト自体の失敗（ネットワーク・認証など）。 */
  error?: string;
  /** チャネル単位のエラー：HTTP は成功したがレスポンスに error が含まれる場合（セッション切断など）。 */
  execError?: string;
}

function Terminal({ session }: { session: ShellSession }) {
  const [history, setHistory] = React.useState<TermEntry[]>([]);
  const [cmd, setCmd] = React.useState("");
  const [timeoutSec, setTimeoutSec] = React.useState(30);
  const [running, setRunning] = React.useState(false);
  const seqRef = React.useRef(0);
  const scrollRef = React.useRef<HTMLDivElement>(null);

  // 新しい出力が来たら最下部までスクロールします。
  React.useEffect(() => {
    const el = scrollRef.current;
    if (el && history.length > 0) el.scrollTop = el.scrollHeight;
  }, [history]);

  const run = () => {
    const command = cmd.trim();
    if (!command || running) return;
    setRunning(true);
    setCmd("");
    api
      .sessionExec(session.id, command, timeoutSec)
      .then((r) => {
        seqRef.current += 1;
        setHistory((h) => [
          ...h,
          {
            seq: seqRef.current,
            cmd: command,
            stdout: r.stdout,
            stderr: r.stderr,
            ms: r.ms,
            timedOut: r.timed_out,
            execError: r.error,
          },
        ]);
      })
      .catch((e: unknown) => {
        seqRef.current += 1;
        setHistory((h) => [
          ...h,
          { seq: seqRef.current, cmd: command, error: e instanceof Error ? e.message : "実行に失敗しました" },
        ]);
      })
      .finally(() => setRunning(false));
  };

  return (
    <div className="flex min-h-0 flex-1 flex-col">
      <div ref={scrollRef} className="min-h-0 flex-1 overflow-auto bg-neutral-950 p-3 font-mono text-xs">
        {history.length === 0 && <div className="text-neutral-500">コマンドを入力して実行します（現在の WebShell チャネル経由）。</div>}
        {history.map((e) => (
          <div key={e.seq} className="mb-3 last:mb-0">
            <div className="text-emerald-400">$ {e.cmd}</div>
            {e.error ? (
              <pre className="text-red-400 break-all whitespace-pre-wrap">リクエストに失敗しました：{e.error}</pre>
            ) : (
              <>
                {e.execError && (
                  <pre className="text-red-400 break-all whitespace-pre-wrap">実行エラー：{e.execError}</pre>
                )}
                {e.stdout && <pre className="text-neutral-200 break-all whitespace-pre-wrap">{e.stdout}</pre>}
                {e.stderr && <pre className="text-red-400 break-all whitespace-pre-wrap">{e.stderr}</pre>}
                {!e.execError && !e.stdout && !e.stderr && <div className="text-neutral-600">（出力なし）</div>}
                <div className={cn("text-[10px]", e.timedOut ? "text-amber-400" : "text-neutral-600")}>
                  {e.timedOut ? `タイムアウト（>${e.ms ?? 0}ms、切り捨ての可能性）` : `${e.ms ?? 0}ms`}
                </div>
              </>
            )}
          </div>
        ))}
      </div>
      <form
        className="flex items-center gap-2 border-t p-2"
        onSubmit={(e) => {
          e.preventDefault();
          run();
        }}
      >
        <span className="pl-1 font-mono text-xs text-emerald-600">$</span>
        <Input
          value={cmd}
          onChange={(e) => setCmd(e.target.value)}
          placeholder="whoami / ipconfig / ls ..."
          className="h-8 flex-1 font-mono text-xs"
          disabled={running}
        />
        <Select value={String(timeoutSec)} onValueChange={(v) => setTimeoutSec(Number(v))}>
          <SelectTrigger size="sm" className="w-24" title="タイムアウト">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {TIMEOUT_OPTIONS.map((n) => (
              <SelectItem key={n} value={String(n)}>
                {n}s タイムアウト
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        <Button type="submit" size="sm" className="h-8 gap-1" disabled={running || !cmd.trim()}>
          {running ? <Loader2Icon className="size-3.5 animate-spin" /> : <PlayIcon className="size-3.5" />}
          実行
        </Button>
      </form>
    </div>
  );
}

// ---------------------------------------------------------------------------
// ファイルマネージャ：パス入力 + ディレクトリ一覧 / 読み取り / 書き込み。
// ---------------------------------------------------------------------------

// joinPath は既存のパス形式（\ または /）で子パスを結合します。
function joinPath(base: string, name: string): string {
  const sep = base.includes("\\") ? "\\" : "/";
  const trimmed = base.endsWith(sep) ? base.slice(0, -1) : base;
  return `${trimmed}${sep}${name}`;
}

function FileManager({ session }: { session: ShellSession }) {
  const [path, setPath] = React.useState("");
  const [entries, setEntries] = React.useState<SessionFsEntry[] | null>(null);
  const [listing, setListing] = React.useState(false);

  // 読み取り・書き込みパネル：ファイルを選ぶと内容をエディタに読み込み、編集してそのまま書き戻せます。
  const [filePath, setFilePath] = React.useState("");
  const [fileContent, setFileContent] = React.useState("");
  const [fileMeta, setFileMeta] = React.useState<{ truncated: boolean; binary: boolean } | null>(null);
  const [fileBusy, setFileBusy] = React.useState(false);
  const [writing, setWriting] = React.useState(false);

  const list = (p: string) => {
    const target = p.trim();
    if (!target || listing) return;
    setListing(true);
    api
      .sessionList(session.id, target)
      .then((es) => {
        setEntries(es);
        setPath(target);
      })
      .catch((e: unknown) => toast.error(e instanceof Error ? e.message : "ディレクトリ一覧の取得に失敗しました"))
      .finally(() => setListing(false));
  };

  const read = (p: string) => {
    if (fileBusy) return;
    setFileBusy(true);
    api
      .sessionRead(session.id, p)
      .then((r) => {
        setFilePath(p);
        setFileMeta({ truncated: r.truncated, binary: !r.content && !!r.content_base64 });
        if (typeof r.content === "string") {
          setFileContent(r.content);
        } else if (r.content_base64) {
          // バイナリ：UTF-8 でデコードして表示を試み、失敗した場合はバイナリとして扱います。
          try {
            setFileContent(new TextDecoder("utf-8", { fatal: true }).decode(fromBase64(r.content_base64)));
          } catch {
            setFileContent("");
          }
        } else {
          setFileContent("");
        }
      })
      .catch((e: unknown) => toast.error(e instanceof Error ? e.message : "読み取りに失敗しました"))
      .finally(() => setFileBusy(false));
  };

  const write = () => {
    const target = filePath.trim();
    if (!target || writing) return;
    setWriting(true);
    api
      .sessionWrite(session.id, target, toBase64(fileContent))
      .then((r) => toast.success(`書き込みました：${r.bytes} バイト → ${target}`))
      .catch((e: unknown) => toast.error(e instanceof Error ? e.message : "書き込みに失敗しました"))
      .finally(() => setWriting(false));
  };

  return (
    <div className="flex min-h-0 flex-1 flex-col divide-y">
      {/* ディレクトリ閲覧 */}
      <div className="flex min-h-0 flex-1 flex-col">
        <form
          className="flex items-center gap-2 p-2"
          onSubmit={(e) => {
            e.preventDefault();
            list(path);
          }}
        >
          <Input
            value={path}
            onChange={(e) => setPath(e.target.value)}
            placeholder="パス（例：C:\inetpub\wwwroot または /var/www/html）"
            className="h-8 flex-1 font-mono text-xs"
          />
          <Button type="submit" size="sm" className="h-8 gap-1" disabled={listing || !path.trim()}>
            {listing ? <Loader2Icon className="size-3.5 animate-spin" /> : <RefreshCwIcon className="size-3.5" />}
            一覧表示
          </Button>
        </form>
        <div className="min-h-0 flex-1 overflow-auto">
          {entries === null ? (
            <div className="py-10 text-center text-xs text-muted-foreground">パスを入力して「一覧表示」を押します。</div>
          ) : entries.length === 0 ? (
            <div className="py-10 text-center text-xs text-muted-foreground">ファイルやフォルダーはありません</div>
          ) : (
            <Table>
              <TableHeader className="sticky top-0 z-10 bg-card">
                <TableRow>
                  <TableHead>名前</TableHead>
                  <TableHead className="w-[90px] text-right">サイズ</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {entries.map((en) => (
                  <TableRow
                    key={en.name}
                    className="cursor-pointer"
                    onClick={() => (en.is_dir ? list(joinPath(path, en.name)) : read(joinPath(path, en.name)))}
                  >
                    <TableCell className="max-w-0">
                      <span className="flex items-center gap-2">
                        {en.is_dir ? (
                          <FolderIcon className="size-3.5 shrink-0 text-amber-500" />
                        ) : (
                          <FileIcon className="size-3.5 shrink-0 text-muted-foreground" />
                        )}
                        <span className="truncate font-mono text-xs" title={en.name}>
                          {en.name}
                        </span>
                      </span>
                    </TableCell>
                    <TableCell className="text-right font-mono text-xs text-muted-foreground tabular-nums">
                      {en.is_dir ? "—" : fmtBytes(en.size)}
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </div>
      </div>

      {/* 読み取り / 書き込み */}
      <div className="flex min-h-0 flex-1 flex-col gap-2 p-2">
        <div className="flex items-center gap-2">
          <Input
            value={filePath}
            onChange={(e) => setFilePath(e.target.value)}
            placeholder="ファイルパス（上の一覧をクリックで自動入力、手入力も可）"
            className="h-8 flex-1 font-mono text-xs"
          />
          <Button
            variant="outline"
            size="sm"
            className="h-8"
            disabled={fileBusy || !filePath.trim()}
            onClick={() => read(filePath.trim())}
          >
            {fileBusy ? <Loader2Icon className="size-3.5 animate-spin" /> : "読み取り"}
          </Button>
          <Button size="sm" className="h-8" disabled={writing || !filePath.trim()} onClick={write}>
            {writing ? <Loader2Icon className="size-3.5 animate-spin" /> : "書き込み"}
          </Button>
        </div>
        {fileMeta?.binary && (
          <p className="text-[11px] text-amber-600 dark:text-amber-400">
            テキストとして表示できないファイルは、エディターが空欄になります。保存すると元のファイルを上書きするため、内容を確認してください。
          </p>
        )}
        {fileMeta?.truncated && (
          <p className="text-[11px] text-amber-600 dark:text-amber-400">内容が長すぎるためバックエンドで切り捨てられました。書き込み前に完全性を確認してください。</p>
        )}
        <Textarea
          value={fileContent}
          onChange={(e) => setFileContent(e.target.value)}
          placeholder="ファイル内容（読み取り後に編集し、「書き込み」でターゲットへ保存）"
          className="min-h-0 flex-1 resize-none font-mono text-xs"
        />
      </div>
    </div>
  );
}

// ---------------------------------------------------------------------------
// ワークベンチ Sheet：ターミナル / ファイルの 2 タブ + セッション削除。履歴などの状態は key=session.id でセッションごとにリセットされます。
// ---------------------------------------------------------------------------
export function SessionWorkbench({
  session,
  onOpenChange,
  onDeleted,
}: {
  session: ShellSession | null;
  onOpenChange: (open: boolean) => void;
  onDeleted: () => void;
}) {
  const [confirming, setConfirming] = React.useState(false);
  const [deleting, setDeleting] = React.useState(false);

  const doDelete = () => {
    if (!session) return;
    setDeleting(true);
    api
      .deleteSession(session.id)
      .then(() => {
        toast.success(`セッション #${session.id} を削除しました`);
        setConfirming(false);
        onOpenChange(false);
        onDeleted();
      })
      .catch((e: unknown) => toast.error(e instanceof Error ? e.message : "削除に失敗しました"))
      .finally(() => setDeleting(false));
  };

  return (
    <Sheet open={session !== null} onOpenChange={onOpenChange}>
      <SheetContent className="w-full! max-w-none! gap-0 p-0 sm:w-[52rem]! sm:max-w-[52rem]!">
        {session && (
          <>
            <SheetHeader className="border-b px-5 py-4">
              <SheetTitle className="pr-8">セッションワークベンチ #{session.id}</SheetTitle>
              <SheetDescription className="truncate font-mono text-xs">{session.url}</SheetDescription>
              <div className="flex flex-wrap items-center gap-2 pt-1">
                <Badge variant="secondary" className="font-mono text-xs">
                  {session.kind || "-"}
                </Badge>
                {session.lang && (
                  <Badge variant="outline" className="font-mono text-xs">
                    {session.lang}
                  </Badge>
                )}
                {session.platform && (
                  <Badge variant="outline" className="text-xs">
                    {session.platform}
                  </Badge>
                )}
                <Badge variant="outline" className="font-mono text-xs">
                  {session.host_asset_id}
                </Badge>
                <Button
                  variant="ghost"
                  size="sm"
                  className="ml-auto h-7 gap-1 text-muted-foreground hover:text-red-600"
                  onClick={() => setConfirming(true)}
                >
                  <Trash2Icon className="size-3.5" />
                  セッションを削除
                </Button>
              </div>
            </SheetHeader>
            <Tabs defaultValue="terminal" className="flex min-h-0 flex-1 flex-col gap-0">
              <TabsList className="mx-5 mt-3 w-fit">
                <TabsTrigger value="terminal">コマンドターミナル</TabsTrigger>
                <TabsTrigger value="files">ファイル管理</TabsTrigger>
              </TabsList>
              <TabsContent value="terminal" className="flex min-h-0 flex-1 flex-col px-5 pt-3 pb-5">
                <div className="flex min-h-0 flex-1 flex-col overflow-hidden rounded-lg border">
                  <Terminal key={session.id} session={session} />
                </div>
              </TabsContent>
              <TabsContent value="files" className="flex min-h-0 flex-1 flex-col px-5 pt-3 pb-5">
                <div className="flex min-h-0 flex-1 flex-col overflow-hidden rounded-lg border">
                  <FileManager key={session.id} session={session} />
                </div>
              </TabsContent>
            </Tabs>

            <AlertDialog open={confirming} onOpenChange={setConfirming}>
              <AlertDialogContent>
                <AlertDialogHeader>
                  <AlertDialogTitle>セッション #{session.id} を削除しますか？</AlertDialogTitle>
                  <AlertDialogDescription>
                    このWebShellセッションを切断して削除します。接続先に置いたWebShellファイルは残ります。この操作は取り消せません。
                  </AlertDialogDescription>
                </AlertDialogHeader>
                <AlertDialogFooter>
                  <AlertDialogCancel disabled={deleting}>キャンセル</AlertDialogCancel>
                  <AlertDialogAction
                    variant="destructive"
                    disabled={deleting}
                    onClick={(e) => {
                      e.preventDefault();
                      doDelete();
                    }}
                  >
                    {deleting ? "削除中…" : "削除の確認"}
                  </AlertDialogAction>
                </AlertDialogFooter>
              </AlertDialogContent>
            </AlertDialog>
          </>
        )}
      </SheetContent>
    </Sheet>
  );
}
