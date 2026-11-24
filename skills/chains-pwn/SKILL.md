---
name: chains-pwn
description: PWN バイナリエクスプロイトの問い直し判断チェーンの原文インデックス(スタックオーバーフロー/ヒープ脆弱性 UAF/利用チェーンと保護バイパス)。pwn 問題やバイナリエクスプロイトを解く際、利用方針を決めるときに参照する。
---

# chains-pwn · バイナリエクスプロイト問い直し判断チェーン(L4 原文インデックス)

## 使用するタイミング

- 未知の ELF を入手し、保護機構で利用方針を決めたい(pwn-stack)
- メニュー型の対話プログラムで、ヒープ問題かどうか判定し libc バージョンを決めたい(pwn-heap)
- 保護が全有効で「まず leak してから攻撃する」二段階の利用を計画したい(pwn-stack)
- 利用チェーン構築中に Canary/NX/PIE/RELRO をバイパスしたい(pwn-exploit-chain)

## 鉄則

- checksec の 5 つのスイッチ(Canary/NX/PIE/RELRO/シンボル)が利用の骨格を決める。利用方針は逆アセンブルの詳細より先に決める(refs/pwn-stack.md)
- 保護が全有効 = 「まず leak してから攻撃する」二段階。保護が全無効なら複雑にしすぎない。利用の複雑さは保護の強度に比例させる(refs/pwn-stack.md)
- checksec だけを信じない:vmmap で実行時にセグメント権限を確認し、仮定は実行時に検証する(refs/pwn-stack.md)
- メニューに add/delete + malloc/free の組み合わせがあれば基本的にヒープ問題と判断する。判断後はまず libc バージョンを特定する。バージョンがすべての利用手法を決める(refs/pwn-heap.md)
- 静的コンパイル/musl/uclibc には glibc の手法を当てはめず、「未知の allocator のリバースエンジニアリング」として別に進める(refs/pwn-heap.md)

## refs/ 原文一覧

- `refs/pwn-exploit-chain.md` — 利用チェーン構築と保護バイパスの判断
- `refs/pwn-heap.md` — ヒープ脆弱性(UAF/オーバーフロー)の判断
- `refs/pwn-stack.md` — スタックオーバーフロー脆弱性の特定と利用の取捨選択

## 使い方

まず本インデックスで篇目と場面節(`### 場面句`)を特定し、対応するファイルを Read する。回答・判断の際はその中の判定基準と方向転換ルールを優先して従う。worker に Glob/Grep はないため、上記の正確な相対パスで直接 Read する。
