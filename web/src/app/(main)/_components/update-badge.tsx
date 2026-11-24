"use client";

import * as React from "react";

import Link from "next/link";

import { ArrowUpCircleIcon } from "lucide-react";

import { api } from "@/lib/api";

/**
 * トップバーの「新しいバージョンがあります」通知：ページ全体の読み込み時に一度確認し、
 * 更新があればバージョン番号の横に表示し、クリックでシステム設定ページの
 * 「バージョンと更新」カードへ直接移動する。
 *
 * バックエンドの GitHub への問い合わせ結果は 30 分キャッシュされるため、
 * マウントのたびに確認しても問題ない。未認証の GitHub API は 60 回/時間/IP の
 * 制限があり、そのキャッシュがなければタブをいくつか開くだけでクォータを使い切る。
 *
 * 問い合わせの失敗はすべて無視する：トップバーはエラーを出す場所ではなく、
 * 設定ページで「更新を確認」を押せば理由が分かる。
 */
export function UpdateBadge() {
  const [latest, setLatest] = React.useState("");

  React.useEffect(() => {
    let alive = true;
    api
      .checkUpdate()
      .then((r) => {
        // has_update には「バージョン番号を比較可能」の判定が含まれるため、開発ビルドではこの通知は表示されない。
        if (alive && r.has_update && r.latest) setLatest(r.latest.replace(/^v(?=\d)/, ""));
      })
      .catch(() => {
        // 無視する：ネットワーク不通 / GitHub のレート制限をトップバーでエラー表示しない。
      });
    return () => {
      alive = false;
    };
  }, []);

  if (!latest) return null;

  return (
    <Link
      href="/system/settings"
      title={`新しいバージョン ${latest} が見つかりました。クリックで更新へ`}
      className="inline-flex items-center gap-1.5 rounded-full bg-primary px-2.5 py-1 font-medium text-primary-foreground text-xs transition-opacity hover:opacity-90"
    >
      {/* 呼吸するドット：トップバーは要素が多く、文字だけでは見落とされやすいので、アニメーションで目立たせる。 */}
      <span className="relative flex size-1.5">
        <span className="absolute inline-flex size-full animate-ping rounded-full bg-primary-foreground opacity-75" />
        <span className="relative inline-flex size-1.5 rounded-full bg-primary-foreground" />
      </span>
      <ArrowUpCircleIcon className="size-3.5" />
      <span className="hidden sm:inline">新バージョン {latest}</span>
      <span className="sm:hidden">新バージョン</span>
    </Link>
  );
}
