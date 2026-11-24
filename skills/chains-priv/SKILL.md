---
name: chains-priv
description: 権限昇格と足場の反問決定連鎖の原文インデックス(Linux/Windows 権限昇格決定木/実行原語の等級分け/disable_functions 迂回/コンテナ・クラウドエスケープ/足場の偵察/永続化)。低権限 shell を取得したとき、実行が制限されているとき、権限昇格や足場の固定が必要なときに参照する。
---

# chains-priv · 権限昇格と足場の反問決定連鎖(L4 原文インデックス)

## いつ使うか

- 低権限 shell(www-data/一般ユーザー)に着地した直後で、列挙順序を決めたいとき(priv-linux、priv-foothold-recon)
- 実行が制限されているとき:disable_functions、サンドボックス、spawn が遮断されていて、実行原語を選びたいとき(priv-exec-primitive、priv-disablefunc)
- Linux/Windows の権限昇格決定木を回し、列挙したベクトルに優先順位を付けたいとき(priv-linux、priv-windows)
- 対象がコンテナ/クラウド環境で、エスケープと権限昇格の可能性を判定したいとき(priv-container-cloud)
- shell が不安定、または永続化の手段を比較検討したいとき(priv-persistence)

## 鉄則

- 列挙は「低コスト・高命中」の漏斗で行う:1 コマンドで結論が出るものを先に実行し(id/sudo -l/uname/ss)、大量のファイルを読む必要があるものは後回しにする(refs/priv-linux.md)
- ベクトルは「確実性 × 隠蔽性 × 安定性」で並べる:設定由来の権限昇格(sudo/suid/cron/cap)を絞り尽くしてからカーネル exp を検討する(refs/priv-linux.md)
- 先に原語を等級分けする:ファイル読み取り < ファイル書き込み < 制限付き評価 < 任意コード実行 < コマンド実行。確証できた等級から計画を始め、最初から最上位を前提にしない(refs/priv-exec-primitive.md)
- spawn が遮断されたら「コマンド実行」への執着を「何を達成したいか」(認証情報の読み取り/横展開の接続/永続化)に格下げする。言語層の原語で主要なニーズの大半をカバーできる(refs/priv-exec-primitive.md)
- 盲状態では帯域外(OOB)が最も信頼できる「原語の存在」の証拠であり、時間ブラインドより安定している(refs/priv-exec-primitive.md)

## refs/ 原文リスト

- `refs/priv-container-cloud.md` — コンテナ/クラウド環境のエスケープと権限昇格の判定
- `refs/priv-disablefunc.md` — disable_functions/制限付き実行の迂回
- `refs/priv-exec-primitive.md` — コマンド実行が制限されたときの実行原語の選択
- `refs/priv-foothold-recon.md` — 足場を確保した後の最初の内部ネットワーク情報収集
- `refs/priv-linux.md` — Linux 権限昇格決定木
- `refs/priv-persistence.md` — 永続化と安定性の取捨選択
- `refs/priv-windows.md` — Windows 権限昇格決定木

## 使い方

先にこのインデックスで篇目と場面節(`### 場面句`)を特定し、対応するファイルを Read する。回答/判断の際は、その中の判定基準と方向転換のルールを優先して従う。worker には Glob/Grep が無いため、上の正確な相対パスで直接 Read する。
