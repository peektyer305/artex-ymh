"use client";

import { useEffect, useRef, useState } from "react";

import { useRouter } from "next/navigation";

import { AlertTriangle, ShieldCheck } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { Dialog, DialogClose, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { api } from "@/lib/api";
import { auth } from "@/lib/auth";

export default function LoginPage() {
  const router = useRouter();
  const [password, setPassword] = useState("");
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(false);
  const [checking, setChecking] = useState(true);
  const [agreed, setAgreed] = useState(false);
  const [termsOpen, setTermsOpen] = useState(false);
  const [readToEnd, setReadToEnd] = useState(false);
  const termsBodyRef = useRef<HTMLDivElement>(null);

  // 規約の最下部までスクロールした場合（スクロール不要で全文表示される場合を含む）に「同意」を押せるようにします。
  function handleTermsScroll() {
    const el = termsBodyRef.current;
    if (!el) return;
    if (el.scrollTop + el.clientHeight >= el.scrollHeight - 8) setReadToEnd(true);
  }

  useEffect(() => {
    if (!termsOpen) return;
    // 開いたときにリセットし、内容が一画面に満たずスクロールが発生しない場合も処理します。
    setReadToEnd(false);
    const el = termsBodyRef.current;
    if (el && el.scrollHeight <= el.clientHeight + 8) setReadToEnd(true);
  }, [termsOpen]);

  useEffect(() => {
    // ログイン済みなら直接メイン画面へ（静的エクスポートでは middleware がこの遷移を代行しません）。
    const token = auth.getToken();
    if (token) {
      // localStorage に認証情報が残っていても cookie は失われている場合があります。先に同期し、新しいリクエストを発行して、
      // サーバー側のガードやルートキャッシュにより checking 状態のログインページへ戻されるのを防ぎます。
      auth.setToken(token);
      window.location.replace("/function/tasks");
      return;
    }
    api
      .authStatus()
      .then(({ initialized }) => {
        if (!initialized) router.replace("/setup");
      })
      .catch(() => setError("バックエンドサービスに接続できません"))
      .finally(() => setChecking(false));
  }, [router]);

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault();
    if (!agreed) {
      setError("先に「利用規約」を読んで同意してください");
      return;
    }
    setLoading(true);
    setError("");
    try {
      const { token } = await api.login("ARTEX", password);
      auth.setToken(token);
      window.location.replace("/function/tasks");
    } catch {
      setError("ユーザー名またはパスワードが正しくありません");
    } finally {
      setLoading(false);
    }
  }

  if (checking) {
    return (
      <div role="status" className="flex min-h-dvh items-center justify-center text-muted-foreground">
        ログイン状態を確認中…
      </div>
    );
  }

  return (
    <div className="flex h-dvh">
      {/* Left panel */}
      <div className="hidden flex-col items-center justify-center bg-primary p-12 text-center lg:flex lg:w-1/3">
        <div className="relative flex items-center justify-center">
          <div className="absolute size-80 rounded-full border border-primary-foreground/10" />
          <div className="absolute size-60 rounded-full border border-primary-foreground/15" />
          <div className="absolute size-40 rounded-full border border-primary-foreground/20" />
          {/* eslint-disable-next-line @next/next/no-img-element */}
          <img src="/logo.png" alt="ARTEX" width={160} height={160} className="relative brightness-0 invert" />
        </div>
      </div>

      {/* Right panel */}
      <div className="flex w-full items-center justify-center bg-background p-8 lg:w-2/3">
        <div className="w-full max-w-md space-y-10 py-24 lg:py-32">
          <div className="space-y-4 text-center">
            <h2 className="text-2xl font-medium tracking-tight">ログイン</h2>
            <p className="mx-auto max-w-xl text-muted-foreground">おかえりなさい。ARTEXの利用を続けるにはパスワードを入力してください</p>
          </div>
          <form onSubmit={handleSubmit} className="flex flex-col gap-4">
            <div className="space-y-1.5">
              <Label htmlFor="username">ユーザー名</Label>
              <Input id="username" value="ARTEX" readOnly className="bg-muted text-muted-foreground" />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="password">パスワード</Label>
              <Input
                id="password"
                type="password"
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                placeholder="パスワードを入力してください"
                autoFocus
                autoComplete="current-password"
              />
            </div>
            <div className="flex items-start gap-2">
              <Checkbox
                id="agree-terms"
                checked={agreed}
                onCheckedChange={(v) => setAgreed(v === true)}
                className="mt-0.5"
              />
              <Label htmlFor="agree-terms" className="text-sm font-normal leading-relaxed text-muted-foreground">
                以下を読み、同意します
                <button
                  type="button"
                  onClick={() => setTermsOpen(true)}
                  className="mx-0.5 font-medium text-primary underline-offset-4 hover:underline"
                >
                  「利用規約の全文」
                </button>
              </Label>
            </div>
            {error && <p className="text-sm text-destructive">{error}</p>}
            <Button type="submit" className="w-full" disabled={loading || !password || !agreed}>
              {loading ? "ログイン中..." : "ログイン"}
            </Button>
          </form>
        </div>
      </div>

      <Dialog open={termsOpen} onOpenChange={setTermsOpen}>
        <DialogContent className="gap-0 p-0 sm:max-w-2xl">
          <DialogHeader className="flex-row items-center gap-3 border-b px-6 py-4">
            <div className="flex size-10 shrink-0 items-center justify-center rounded-lg bg-primary/10 text-primary">
              <ShieldCheck className="size-5" />
            </div>
            <div className="space-y-0.5">
              <DialogTitle className="text-base">ARTEX 利用規約・免責事項</DialogTitle>
              <p className="text-xs text-muted-foreground">
                バージョン v1.0 · 施行日 2026-09-18 · ログイン前に以下の全条項をお読みください
              </p>
            </div>
          </DialogHeader>

          <div
            ref={termsBodyRef}
            onScroll={handleTermsScroll}
            className="max-h-[60vh] space-y-5 overflow-y-auto px-6 py-5 text-sm leading-relaxed text-muted-foreground"
          >
            <p className="rounded-lg border bg-muted/40 p-3 text-foreground/80">
              本「利用規約・免責事項」（以下「本規約」といいます）は、本ソフトウェアの利用に関して、お客様と ARTEX
              プロジェクトの作者および貢献者との間で成立する合意です。利用前に、各条項、とくに太字や色付きで示された免責、責任制限および禁止条項をよく読み、十分に理解してください。
              <span className="font-medium text-foreground">
                {" "}
                本ソフトウェアをダウンロード、インストール、アクセスその他いかなる方法で利用した時点で、本規約のすべてを読み、理解し、その拘束を受けることに同意したものとみなします。
              </span>
            </p>

            <section className="space-y-1.5">
              <h4 className="flex items-center gap-2 font-medium text-foreground">
                <span className="flex size-5 items-center justify-center rounded-md bg-muted text-xs font-semibold text-muted-foreground">
                  1
                </span>
                第一条 · 定義とオープンソースライセンス
              </h4>
              <p className="pl-7">
                本ソフトウェア（ARTEX）は、GNU Affero General Public License
                v3.0（AGPL-3.0）に基づいて公開されたオープンソースプログラムです。本ライセンスに従い、本ソフトウェアを自由に使用、複製、変更、配布できます。ただし、派生物（ネットワーク経由で第三者に提供するオンラインサービスを含む）は、同じく
                AGPL-3.0 でオープンソース化し、利用者に対応する完全なソースコードを公開する必要があります。AGPL-3.0 の完全な条項は同梱の LICENSE ファイルに従います。
              </p>
            </section>

            <section className="space-y-1.5">
              <h4 className="flex items-center gap-2 font-medium text-foreground">
                <span className="flex size-5 items-center justify-center rounded-md bg-muted text-xs font-semibold text-muted-foreground">
                  2
                </span>
                第二条 · 許諾される利用範囲
              </h4>
              <p className="pl-7">
                本ソフトウェアは、個人の学習、コードの調査、セキュリティ技術の原理に関する検討、およびご自身で構築したローカルの隔離環境での技術検証にのみ利用できます。学習、学術研究、コードレビューなど、攻撃的でなく破壊的でない用途に適します。本条で明示的に許諾された場合を除き、その他の目的に本ソフトウェアを利用してはなりません。
              </p>
            </section>

            <section className="space-y-2">
              <h4 className="flex items-center gap-2 font-medium text-destructive">
                <span className="flex size-5 items-center justify-center rounded-md bg-destructive/10 text-xs font-semibold text-destructive">
                  3
                </span>
                <AlertTriangle className="size-4" />
                第三条 · 禁止される行為
              </h4>
              <ul className="ml-7 list-decimal space-y-1.5 rounded-lg border border-destructive/20 bg-destructive/5 p-3 pl-8 text-foreground/80 marker:text-destructive/70">
                <li>
                  いかなるウェブサイト、オンラインサービス、他人または第三者が所有するネットワークシステムに対しても、スキャン、探索、悪用、攻撃を行ってはなりません（権限の有無、ご自身の資産かどうかを問いません）。
                </li>
                <li>本ソフトウェアを実際のペネトレーションテスト、攻防演習、レッドチーム演習または本番環境で使用してはなりません。</li>
                <li>本ソフトウェアを不正侵入、データ窃取、身代金要求、サービス拒否（DoS/DDoS）その他の破壊的・犯罪的行為に使用してはなりません。</li>
                <li>本ソフトウェアおよびその出力に含まれる著作権、ライセンス、セキュリティに関する告知を削除、改変または回避してはなりません。</li>
                <li>お住まいの国または地域の法律、法令および監督規定に違反する行為を行ってはなりません。</li>
              </ul>
            </section>

            <section className="space-y-1.5">
              <h4 className="flex items-center gap-2 font-medium text-foreground">
                <span className="flex size-5 items-center justify-center rounded-md bg-muted text-xs font-semibold text-muted-foreground">
                  4
                </span>
                第四条 · 知的財産権の帰属
              </h4>
              <p className="pl-7">
                本ソフトウェアの著作権および関連する知的財産権は、プロジェクトの作者および貢献者に帰属し、AGPL-3.0
                が定める範囲でお客様に権利が許諾されます。本規約は、同ライセンスで明示的に許諾された権利を除き、明示・黙示を問わずいかなる権利も許諾しません。
              </p>
            </section>

            <section className="space-y-1.5">
              <h4 className="flex items-center gap-2 font-medium text-foreground">
                <span className="flex size-5 items-center justify-center rounded-md bg-muted text-xs font-semibold text-muted-foreground">
                  5
                </span>
                第五条 · データとプライバシー
              </h4>
              <p className="pl-7">
                本ソフトウェアは自身でデプロイできるオープンソースプログラムであり、作者は集中型サービスを運用せず、利用データの収集や送信も行いません。利用中に生成、処理、接触したすべてのデータは、ご自身で管理し、その合法性と安全性に責任を負うものとします。不適切なデータ処理による結果は、すべてご自身の責任となります。
              </p>
            </section>

            <section className="space-y-1.5">
              <h4 className="flex items-center gap-2 font-medium text-foreground">
                <span className="flex size-5 items-center justify-center rounded-md bg-muted text-xs font-semibold text-muted-foreground">
                  6
                </span>
                第六条 · コンプライアンスと法的責任
              </h4>
              <p className="pl-7">
                お住まいの国または地域のサイバーセキュリティ、データセキュリティと個人情報保護、コンピュータ犯罪などに関するすべての法令を遵守してください（中国大陸では「サイバーセキュリティ法」「データセキュリティ法」「個人情報保護法」および関連する司法解釈を含みますが、これらに限りません）。
                <span className="font-medium text-foreground">
                  {" "}
                  上記の法令または本規約に違反して生じたすべての法的責任と結果は、ご本人が単独で負うものとし、本ソフトウェアの作者および貢献者とは無関係です。
                </span>
              </p>
            </section>

            <section className="space-y-1.5">
              <h4 className="flex items-center gap-2 font-medium text-foreground">
                <span className="flex size-5 items-center justify-center rounded-md bg-muted text-xs font-semibold text-muted-foreground">
                  7
                </span>
                第七条 · 免責事項と責任制限
              </h4>
              <p className="pl-7">
                本ソフトウェアは「現状（AS IS）」および「提供可能な状態（AS
                AVAILABLE）」で提供され、明示・黙示を問わずいかなる保証も伴いません。商品性、特定目的への適合性、正確性および非侵害性に関する保証を含みますが、これらに限りません。適用法が許す最大限の範囲で、本ソフトウェアの作者および貢献者は、本ソフトウェアの使用または使用不能（使用方法の適否を問わず）によって生じた直接的、間接的、偶発的、特別または結果的損害について責任を負いません。データの消失、システムの破損、業務の中断、利益の喪失、法的紛争などを含みますが、これらに限りません。
              </p>
            </section>

            <section className="space-y-1.5">
              <h4 className="flex items-center gap-2 font-medium text-foreground">
                <span className="flex size-5 items-center justify-center rounded-md bg-muted text-xs font-semibold text-muted-foreground">
                  8
                </span>
                第八条 · 条項の変更と最終解釈
              </h4>
              <p className="pl-7">
                作者は、法令またはプロジェクトの発展に応じて本規約を随時更新できます。更新後の版はプロジェクトとともに公開され、公表日から効力を生じます。更新後も本ソフトウェアを使い続けた場合、改定後の条項に同意したものとみなします。法律が許す範囲で、本規約の最終解釈権はプロジェクトの作者に帰属します。本規約のいずれかの条項が無効と判断されても、残りの条項の効力には影響しません。
              </p>
            </section>
          </div>

          <DialogFooter className="mx-0 mb-0 flex-col items-stretch gap-2 rounded-b-xl px-6 sm:flex-row sm:items-center sm:justify-between">
            <p className="text-xs text-muted-foreground">
              {readToEnd ? "すべての条項を確認しました" : "条項を最下部までスクロールしてから確認してください"}
            </p>
            <DialogClose asChild>
              <Button
                type="button"
                disabled={!readToEnd}
                onClick={() => {
                  setAgreed(true);
                  setError("");
                }}
              >
                すべての条項を読み、同意します
              </Button>
            </DialogClose>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}
