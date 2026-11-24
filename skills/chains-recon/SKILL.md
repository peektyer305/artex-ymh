---
name: chains-recon
description: 情報収集の問い返し式意思決定チェーンの原文索引(アセット調査/フィンガープリント識別/JS API 抽出/漏えいハンティング/OSINT/隠しパラメータ/いつ十分かの判断)。偵察段階、スキャンで止め時がわからないとき、入口を探すときに参照。
---

# chains-recon · 情報収集の問い返し式意思決定チェーン(L4 原文索引)

## いつ使うか

- URL/IP が一つしかないとき、偵察の優先度と順序を定める(recon-asset-surface、recon-fingerprint)
- スキャン結果が大量に出て、続けてスキャンするか、それとも手動検証に移るか迷うとき(recon-when-enough)
- 漏えい面を掘る場合:バックアップ/.git/.env/swagger/JS キー(recon-leak-hunting、recon-js-endpoints)
- 隠しパラメータと未認可 API を掘る場合(recon-param-mining)
- 技術面をやり尽くして、OSINT/人の攻撃面に移る場合(recon-osint)

## 鉄則

- 止め時の判定基準は「まだスキャンしていないポートがあるかどうか」ではなく「手に入れたものの中に攻撃仮説を形成できる手がかりがあるかどうか」。スキャンの限界利益は第1ラウンド以降に急激に減衰する(refs/recon-when-enough.md)
- 「露出即高リスク」の確定的な対象(.git/.svn/.env/一般的なバックアップ/swagger)を先にスキャンし、広範なディレクトリのブルートフォースより優先する(refs/recon-leak-hunting.md)
- 辞書は技術スタックに応じて切り替える(Java なら WEB-INF、PHP なら .php~、Node なら .env/source map)。一つの辞書で全部を片付けようとしない(refs/recon-leak-hunting.md)
- 手動検証は「最小検証コスト」で並べる:1リクエストで真偽を確かめられるものを先に検証し、行き止まりは数分以内に見極める(refs/recon-when-enough.md)
- 漏えい点に当たったら、まず広さのスナップショットを取り、その後に収益の高いものを深掘りする。最初の穴にはまり込まない(refs/recon-leak-hunting.md)

## refs/ 原文一覧

- `refs/recon-asset-surface.md` — 外部ネットワークのアセット調査と攻撃面の確定
- `refs/recon-fingerprint.md` — サブドメイン/ポート/サービスのフィンガープリントの取捨選択と順序
- `refs/recon-js-endpoints.md` — JS とフロントエンド内の API パスと機密情報の抽出
- `refs/recon-leak-hunting.md` — 漏えいハンティング:バックアップ/.git/.env/swagger/JS キー
- `refs/recon-osint.md` — OSINT と人の攻撃面
- `refs/recon-param-mining.md` — 隠しパラメータと未認可 API の掘り出し
- `refs/recon-when-enough.md` — 「情報が十分か、いつ手を付けるか」の判断

## 使い方

まず本索引で篇目と場面節(`### 場面句`)を特定し、次に対応ファイルを Read する。回答/意思決定の際は、その中の判定基準と方向転換ルールを優先して従う。worker には Glob/Grep がないため、上記の正確な相対パスで直接 Read する。
