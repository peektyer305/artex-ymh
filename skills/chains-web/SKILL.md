---
name: chains-web
description: Web の脆弱性に関する問い返し型意思決定チェーンの原文索引(SQLi/アップロード/デシリアライズ/SSRF/SSTI/XXE/コマンドインジェクション/ロジック権限外アクセス/認証バイパス/CMS/WAF バイパス/シェル足場確保)。Web 対象を攻撃するとき、payload が遮断されたとき、判定に疑義があるとき、方向転換が必要なときに参照する。
---

# chains-web · Web 脆弱性の問い返し型意思決定チェーン(L4 原文索引)

## いつ使うか

- パラメータ付き API を入手し、注入できるか、どのデータベース/注入タイプかを判定したい(web-sqli)
- payload が遮断された(403/遮断ページ/レート制限)とき、遮断の階層を特定しバイパスの次元を選びたい(web-waf-bypass)
- アップロード/デシリアライズ/SSTI/XXE/SSRF/コマンドインジェクションの入口があり、判定の順序と利用経路を定めたい
- 認証・セッション系の対象:JWT/OAuth/SSO 攻撃、ログインバイパス(web-token-attack、web-auth-bypass)
- CMS を特定できたとき、「特定→既知の脆弱性→管理画面 getshell」の近道を進みたい(web-cms)
- すでに Web の脆弱性があり、システムシェルに足場確保したい(web-to-shell)

## 鉄則

- 注入判定は「再現可能な真偽の分岐」で見るのであって「エラー」ではない。エラーは構文が変わったことしか示さず、分岐こそが式が実行ロジックに入ったことを示す(refs/web-sqli.md)
- ステータスコード 200 は突破と等しくない。WAF は 200 を偽装するため、まず良性/悪意の対照実験をしてからレスポンスを信じる(refs/web-waf-bypass.md)
- アップロードではまず良性ファイルを送って 3 点を見る。パスのエコー、リネーム方針、直接アクセス可否。それから webshell を検討する(refs/web-upload.md)
- 単点で攻めきれないなら、まず注入位置/パラメータ面/プロトコル入口を変える(query→body→header→cookie→JSON)。同じパラメータに固執しない(refs/web-sqli.md)
- シェルを足場確保する前に 3 つの枠を埋める。データベース種別、現在のユーザー権限、DB と Web が同一マシンかどうか(refs/web-to-shell.md)

## refs/ 原文リスト

- `refs/web-auth-bypass.md` — 認証とセッションバイパスの考え方
- `refs/web-cmdi.md` — コマンドインジェクション/コードインジェクションの特定
- `refs/web-cms.md` — CMS の汎用的な攻め方。特定→既知の脆弱性→管理画面 getshell
- `refs/web-deser.md` — デシリアライズ脆弱性の判定とチェーン構築
- `refs/web-logic.md` — ビジネスロジックと権限外アクセス脆弱性
- `refs/web-sqli.md` — SQL インジェクション:判定→利用→読み書き→権限昇格
- `refs/web-ssrf.md` — SSRF の探査と内部ネットワーク攻撃
- `refs/web-ssti.md` — SSTI テンプレートインジェクションの識別と利用
- `refs/web-to-shell.md` — Web の脆弱性からシステムシェルへの足場確保
- `refs/web-token-attack.md` — JWT/OAuth/SSO などの認証メカニズム攻撃
- `refs/web-upload.md` — ファイルアップロードバイパスの意思決定
- `refs/web-waf-bypass.md` — WAF/フィルタリング下でのバイパス意思決定
- `refs/web-xxe.md` — XXE の識別とデータ持ち出し

## 使い方

まず本インデックスで篇目と場面節(`### 場面句`)を特定し、対応するファイルを Read する。回答/意思決定の際は、その中の判定基準と方向転換ルールを優先して従う。worker には Glob/Grep がなく、上の正確な相対パスで直接 Read する。
