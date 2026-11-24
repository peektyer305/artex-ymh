---
name: chains-app
description: モバイル/クライアントアプリの反問意思決定チェーン原文インデックス(APK リバースエンジニアリング/プロテクトのアンパック/Frida hook/プロトコル暗号化の復元/ローカル鍵/ミニプログラム/C-S パケットキャプチャ)。App を分析する、パケットが取れない、hook やアンパックが必要なときに参照する。
---

# chains-app · App クライアント反問意思決定チェーン(L4 原文インデックス)

## 使用するタイミング

- 未知の APK/IPA を入手し、静的偵察の順序と切り口を定める(app-apk-recon、app-frida)
- App のプロテクトを疑い、パッカー判定、ベンダー識別、アンパック方針の選択を行う(app-unpack)
- 暗号化・復号/検証ロジックを hook したい、hook の階層(Java か native か)を定めたい(app-frida、app-proto-crypto)
- C/S アーキテクチャでパケットが取れない、またはプロトコルが読めない(app-cs-capture、app-proto-crypto)
- クライアントのローカルストレージ/ハードコードされた鍵・認証情報を探したい(app-local-secrets)
- 対象がミニプログラム/H5/モバイル API 攻撃面である(app-miniapp)

## 鉄則

- まず静的偵察でマップを作り(Manifest、言語スタック、lib 配下の特徴 so)、それから hook 対象を決める。いきなり frida-trace から始めない(refs/app-frida.md)
- メイン dex に業務コードが見つからなければほぼパッカー。パッカーの Loader を読むのに時間を浪費しない(refs/app-unpack.md)
- プロテクトのベンダーがアンパックの打ち手を決める:SO 名/パッケージ名/挙動フィンガープリントでベンダーを特定し、特徴が曖昧なら挙動フィンガープリントに移る(refs/app-unpack.md)
- 静的解析で暗号化アルゴリズムが分からなければ、動的かつ広域の hook(Cipher/Mac/MessageDigest/Base64)に移り、ランタイムにアルゴリズムを語らせる(refs/app-frida.md)
- frida-server とホスト側のバージョン、アーキテクチャは厳密に一致させる。`frida-ps -U` でプロセスが列挙できて初めて経路が通っているとみなす(refs/app-frida.md)

## refs/ 原文リスト

- `refs/app-apk-recon.md` — APK をリバースエンジニアリングして API と鍵を抽出
- `refs/app-cs-capture.md` — C/S アーキテクチャのパケットキャプチャとプロトコル解析
- `refs/app-frida.md` — Frida/hook 動的解析の意思決定
- `refs/app-local-secrets.md` — クライアントのローカルストレージとハードコードの悪用
- `refs/app-miniapp.md` — ミニプログラム/H5/モバイル API 攻撃面
- `refs/app-proto-crypto.md` — 通信プロトコルと暗号化の復元
- `refs/app-unpack.md` — プロテクトのアンパックの判断

## 使い方

まず本インデックスで篇目と場面節(`### 場面句`)を特定し、対応ファイルを Read する。回答/意思決定時は、その中の判定基準と方向転換ルールを優先して従う。worker には Glob/Grep がないため、上記の正確な相対パスで直接 Read する。
