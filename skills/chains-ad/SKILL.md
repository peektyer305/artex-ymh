---
name: chains-ad
description: AD ドメインペネトレーションの反問意思決定チェーン原文インデックス(内部ネットワーク偵察/Kerberos/委任/ADCS/認証情報収集/パスワードスプレー/DCSync/認証不要 CVE/ドメインコントローラ直行 vs 横展開)。ドメイン内の足場を取得し、攻撃経路の選択や方向転換が必要なときに参照する。
---

# chains-ad · AD ドメインペネトレーション反問意思決定チェーン(L4 原文インデックス)

## いつ使うか

- ドメイン内ホストでシェルを取得したばかりで、最初の収集順序を決めたい(ad-recon)
- ドメイン認証情報を手にして、Kerberoast/AS-REP/委任/ADCS の経路選択を進めたい(ad-kerberos、ad-constrained-delegation、ad-adcs)
- 本機/認証情報ストア(mimikatz/SAM/チケット)を収集したい、またはパスワードスプレーを行いたい(ad-cred-harvest、ad-password-spray)
- 「ドメインコントローラへ直行するか、先に横展開で認証情報を収集するか」で迷っている(ad-direct-vs-lateral)
- Zerologon のような認証不要の脆弱性に遭遇し、適用条件とリスクを評価したい(ad-noauth-cve)
- 仕上げの段階に入り、DCSync/flag 取得を行いたい(ad-dcsync-flag)。回線が制限されているため操作のテンポを調整したい(ad-tunnel-friendly)

## 鉄則

- 内部ネットワーク偵察では、まずゼロ・アクティブ・トラフィックの本機セルフチェック(whoami/ipconfig/netstat/arp)を行い、パッシブ情報で「ドメイン名、DC の場所、ネットワークセグメント、ドメイン認証情報の有無」の四つの問いに答えてから、能動スキャンを検討する(refs/ad-recon.md)
- 「ドメインを調べる」ことを「ネットワークをスキャンする」より優先する。net view /domain、SRV レコードで DC を特定する。ドメインそれ自体が既に構築済みのアセット台帳である(refs/ad-recon.md)
- Kerberoast の前に SPN をオフラインでフィルタリングする(マシンアカウント/krbtgt を除外し、サービスアカウントを優先する)。盲目的なスキャンは SOC に完全な攻撃プロファイルを送ることになる(refs/ad-kerberos.md)
- 認証情報で攻撃を始める前に、三つの前提を検証する。クロックずれが 5 分以内であること、kinit ができること、LDAP の SPN を読み取れること(refs/ad-kerberos.md)
- オフラインのブルートフォースだけが唯一の換金手段ではない。Roast が遮断されたら、ためらわず AS-REP/委任チェーン/ADCS/パスワードスプレーに切り替える(refs/ad-kerberos.md)

## refs/ 原文リスト

- `refs/ad-adcs.md` — ADCS ESC 証明書攻撃の経路選択
- `refs/ad-constrained-delegation.md` — 委任の悪用:制約付き/非制約/RBCD でドメイン管理者へ到達
- `refs/ad-cred-harvest.md` — 認証情報収集:mimikatz/SAM/チケット
- `refs/ad-dcsync-flag.md` — DCSync と flag 取得による仕上げ
- `refs/ad-direct-vs-lateral.md` — ドメインコントローラ直行 vs 横展開での認証情報収集の方向転換判断
- `refs/ad-kerberos.md` — Kerberos 攻撃:Kerberoast/AS-REP/委任
- `refs/ad-noauth-cve.md` — Zerologon/認証不要脆弱性の適用とリスク
- `refs/ad-password-spray.md` — 弱いパスワード/パスワードスプレー/再利用のテンポ
- `refs/ad-recon.md` — 内部ネットワーク偵察とホスト分類・優先順位付け
- `refs/ad-tunnel-friendly.md` — 制約のある回線での操作テンポとリクエスト往復コストのトレードオフ

## 使い方

まずこのインデックスで篇目と場面節(`### 場面句`)を特定し、その上で対応するファイルを Read する。回答・意思決定の際は、その中の判定基準と方向転換ルールを優先して従う。worker には Glob/Grep がないため、上記の正確な相対パスで直接 Read する。
