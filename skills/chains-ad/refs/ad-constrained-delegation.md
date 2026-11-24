# ADドメイン内 · 委任の悪用:制約付き/非制約/RBCD でドメイン管理者へ到達

> tags: 委任, 制約付き委任, 非制約委任, RBCD, S4U, S4U2Self, S4U2Proxy, マシンアカウント, サービスアカウント, ドメイン権限昇格, ドメイン管理者, DA, impersonate, msDS-AllowedToDelegateTo, getST, kerberoast, ブルートフォース不能, オフラインブルートフォース, access_denied, pass-the-hash, dcsync拒否

### 「委任」という経路をいつ思い出すべきか(オフラインブルートフォースに固執しない)
**Q1**: ドメインメンバー機で SYSTEM を取得し、reg save で本機の【マシンアカウント hash】も取得した。しかしマシンアカウントは DC 上で access_denied になり、Kerberoast で得た hash も辞書がなくてブルートフォースできない。手詰まりだろうか?
**A1**: 違う。これはまさに【委任へ転換】すべきサインである。マシンアカウント hash それ自体が S4U/RBCD の鍵のひとつであり、委任の悪用は【パスワードを一切ブルートフォースする必要がなく】、hash をそのまま Kerberos S4U に使ってドメイン管理者になりすませる。単一の Kerberoast hash のオフラインブルートフォースに固執するのは(特に辞書がない場合)典型的なサンクコストの罠である。同じことを N 通りの破り方で試しても新しい結果は出ない。直交する経路へ移るべきだ。判定基準:すでに【マシンアカウントまたはサービスアカウント】の hash/平文を握っているなら、まずその委任属性を調べること。ブルートフォースに時間を費やさない。
**Q2**: マシンアカウント以外にユーザー認証情報を何も持っていない。委任も使えないだろう?
**A2**: それなら先に【ドメインユーザー認証情報をひとつ収集】してから考える。順序は LSASS dump(対話ログオンしたドメインユーザーの平文/NTLM を取得)→ DCC2 cached(オフライン)→ 任意のドメインユーザーパスワードを取得した後に【パスワードスプレー】で全域からさらにアカウントを取る、となる。委任は「足場となるアカウントを得た後にどう DA へ権限昇格するか」の手段であり、何もないところから始めるものではない。[[ad-cred-harvest]] [[ad-password-spray]] を参照。run の振り返り:モデルは reg save でマシンアカウントだけ取ると Kerberoast に突っ込み、【LSASS を dump したことが一度もなく】実ユーザーの認証情報を取らず、【スプレーも一度もせず】、唯一のマシンアカウントを行き止まり扱いした。実はそれは委任への入場券である。

### 三種類の委任の識別と経路選択
**Q1**: ドメインに悪用可能な委任があるか、どの種類かを素早く判断するには?
**A1**: 三つの属性を調べて振り分ける。①`userAccountControl` に `TRUSTED_FOR_DELEGATION` を含む → 非制約委任(unconstrained)。②オブジェクトが `msDS-AllowedToDelegateTo` を持つ(何らかの SPN を指す)→ 制約付き委任(constrained)。③対象オブジェクトの `msDS-AllowedToActOnBehalfOfOtherIdentity` が書き込み可能 → リソースベース委任(RBCD)。LDAP/PowerView(`Get-DomainComputer -TrustedToAuth`、`-Unconstrained`)で全域を一括調査する。
**Q2**: 手元が【マシンアカウント】で、それが制約付き委任(msDS-AllowedToDelegateTo が DC のあるサービスを指す)を持つ。どう使う?
**A2**: S4U2Self+S4U2Proxy を使う。マシンアカウント hash で「任意のユーザー(ドメイン管理者を選ぶ)として自分にアクセスする」チケットを申請し(S4U2Self)、それを「msDS-AllowedToDelegateTo にある対象 SPN へアクセスする」チケットへ交換する(S4U2Proxy)。得られるのは【ドメイン管理者になりすまして DC の対象サービスへアクセスする】ST である。対象 SPN が cifs/host なら、DC のファイル/コマンド面を直接取得できる。impacket:`getST.py -spn <対象SPN> -impersonate Administrator -hashes :<マシンhash> domain/machine$`。
**Q3**: 制約付き委任が SPN を限定している(たとえば time サービスのみ許可)。しかし cifs でシェルを取りたい。制限されてしまうか?
**A3**: SPN-less / altservice のテクニックを使う。S4U2Proxy が返すチケットの sname は変更できる(古典的な「任意のサービス」悪用)。time/xxx を同一ホストの別サービス cifs/host/ldap に変える。同一アカウント・同一ホストの異なるサービスは鍵を共有するため、チケットは対象に受け入れられる。これで制約付き委任の「サービス限定」は形骸化する。
**Q4**: 既存の委任はないが、あるマシンでローカル管理者である、またはあるオブジェクトの属性を変更できる?
**A4**: RBCD へ転換する。自分が制御できる SPN 付きアカウントをひとつ見つけ(または addcomputer でマシンアカウントを自分で作成し)、それを対象マシンの `msDS-AllowedToActOnBehalfOfOtherIdentity` に書き込み、その制御下アカウントで対象に対して S4U でドメイン管理者になりすます。属性書き込みには通常、対象への GenericWrite/GenericAll が必要である。これが委任がしばしば ACL 悪用と併用される理由でもある。
**Q5**: 非制約委任のマシンがある。どう攻める?
**A5**: 非制約委任マシンは訪問者の TGT をメモリにキャッシュする。coerce(PrinterBug/PetitPotam)で DC のマシンアカウントに認証させ、この非制約マシン上で DC$ の TGT を dump し、それで DCSync する。これは別の直交経路であり、ブルートフォースを必要としない。

### 仕上げ:委任で DA を取得した後
**Q1**: S4U でドメイン管理者になりすまして DC の cifs へアクセスするチケットを取得した。次にどう flag を取得する?
**A1**: このチケット(export KRB5CCNAME 後に -k -no-pass)で DC に対して:①`secretsdump.py -k dc.domain` で DCSync して krbtgt/ドメイン管理者 hash を抜く。②または `wmiexec/psexec -k` で直接 DC 上にコマンドを落として flag を読む。③または smbclient -k で DC 上の Administrator デスクトップ/指定パスを読む。DA 相当物を取得したらそれ以上発散せず、flag の定番位置へ直行する。[[ad-dcsync-flag]] を参照。
**Q2**: チケット系の操作がネットワーク層でよく失敗する(socket error/access denied)。しかし足場マシンでは SYSTEM を持っている?
**A2**: 【ドメイン内の足場マシン上で実行】する。攻撃機から脆弱なトンネルを経由して DC を叩かない。足場はドメインメンバーであり、DC とネイティブに Kerberos 通信できる。getST/Rubeus の動作をこのマシン上で実行する(もともと内部ネットワークセグメント内にある)方が、SOCKS で Kerberos/SMB を転送するよりはるかに安定する。トンネル越しに DC を叩いて繰り返し失敗するときの第一反応は「足場で近接実行に切り替える」ことであり、トンネルを修繕し続けることではない。[[ad-tunnel-friendly]] [[pivot-deep-discovery]] を参照。
