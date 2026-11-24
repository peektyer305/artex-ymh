# ADドメイン内 · Kerberos 攻撃:Kerberoast/AS-REP/委任

### ドメイン内シェルを着地したら、まず Kerberoast か別の手か
**Q1**: 一般ドメインユーザーの認証情報/セッションを取得した。攻撃面を素早く「オフラインで hash をブルートフォース」へ転換したい。最初に何をすべきか?
**A1**: まず三つを確認する。(1)クロックと DC のずれが 5 分以内か。(2)手持ちの認証情報で kinit/TGT リクエストが成功するか。(3)LDAP で servicePrincipalName 属性を読めるか。三つが通ってから Kerberoast を考える。そうでなければ先に前提を解決する。
**Q2**: なぜ直接 Rubeus/GetUserSPNs を一括で投げないのか?
**A2**: Kerberoast は 4769 イベントをトリガーし、全 SPN を盲スキャンすると SOC に「完全な攻撃者プロファイル」を渡すことになる。先にオフラインでフィルタする。マシンアカウント(パスワードは 120 文字乱数)を除外し、krbtgt を除外し、サービスアカウントを優先する(sAMAccountType=805306368 かつ SPN 非空)。
**Q3**: LDAP 照会が制限された、または返すフィールドが極めて少ない場合は?
**A3**: 「匿名 SPN 列挙」の代替経路へ転換する。既知の TGT で直接 sname を推測して TGS をリクエストする(一部の旧ドメインは未知 SPN に KDC_ERR_S_PRINCIPAL_UNKNOWN を返し、存在性 oracle に使える)。または DNS SRV/SCP レコードでサービスを逆引きする。
**Q4**: この経路全体が遮られた(ドメインが FAST/Kerberos Armoring を有効化、または全サービスアカウントが AES256 かつ強パスワード)場合は?
**A4**: ためらわず【直交転換】する。Roast を諦め、以下へ転換する。(a)AS-REP Roasting で DONT_REQ_PREAUTH アカウントを探す。(b)委任チェーン利用(非制約/制約/RBCD)。(c)ADCS 証明書テンプレート悪用(ESC1/ESC8)。(d)OWA/SMB へのパスワードスプレー。オフラインブルートフォースだけが唯一の換金方式ではない。
**Q5**: 現在の認証情報が「列挙できるほど十分」かをどう知る?
**A5**: 判定基準は、自分が属する OU のオブジェクトへ `ldapsearch` でき、objectClass=user の数が 0 より大きく、かつ servicePrincipalName 属性が非空のオブジェクトを少なくともひとつ読めるか。読めなければ ACL に制限されており、先に横展開で別のトークンを取る必要がある。

### Kerberoast で大量の TGS を取得。どれを先に解くか
**Q1**: 手元に 30 個以上の $krb5tgs$ hash がある。オフライン算力が限られる。どれを先に走らせる?
**A1**: 順序戦略:(1)暗号種別 etype=23 (RC4-HMAC) を優先。解読速度は AES の数十倍。(2)ユーザー名に svc/sql/backup/iis/web/mssql などの業務プレフィックスを含むものを優先。業務アカウントのパスワード複雑度は管理アカウントより一般に低い。(3)pwdLastSet が 3 年前より古いものを優先。古いパスワードほど弱いパスワードの可能性が高い。
**Q2**: 対象ドメインが AES only を強制し、etype=17/18 ばかり。オフライン速度が二桁落ちた場合は?
**A2**: 辞書を縮めるのであって無理に走らせない。OSINT + 対象ドメイン名 + 会社略称で小さく精緻なルール辞書(会社名+年+!@#、部門略称+123)を生成し、rockyou 全走査はしない。同時にもう一本の線を並行で開き、算力に吊られない。
**Q3**: 全部走らせてもひとつも解けなかった。この経路は死んだか?
**A3**: 必ずしもそうではない。まず三点を確認する。(a)hash 形式がツールで誤って切り詰められていないか(Rubeus/impacket の出力形式のバージョン差)。(b)msDS-AllowedToDelegateTo 上に間接的に露出する SPN を見落としていないか。(c)gMSA アカウントがあるか(msDS-ManagedPassword を読めればブルートフォースを完全に迂回)。
**Q4**: 全部試してゼロと確認した。次の方向は?
**A4**: 【転換】AS-REP + 委任 + ADCS の三択。特に列挙時に msDS-AllowedToActOnBehalfOfOtherIdentity または TRUSTED_FOR_DELEGATION フラグを見たなら、直接委任経路へ飛ぶ。hash を解き続けるより ROI がはるかに高い。
**Q5**: 「低権限サービスアカウントのパスワード」をひとつ解いた。どう昇格する?
**A5**: このアカウント自体で止まらない。それで自分がアクセスできるリソースをリクエストし、どのマシンの local admin グループにいるか、高価値オブジェクトへの GenericWrite/WriteDACL を持つか、ある制約付き委任チェーンの途中の踏み台かを調べる。パスワード自体は鍵であって終点ではない。

### AS-REP Roasting:アカウントリストなしでどう始めるか
**Q1**: ドメインアカウントの認証情報を一切持たず、ドメイン名と DC IP だけを知っている。AS-REP Roasting をしたい。ユーザー名はどこから?
**A1**: 三経路を並行する。(1)OSINT/会社公式サイト/LinkedIn から社員名簿を収集し、firstname.lastname / f.lastname / flastname の三パターンで候補を生成。(2)SMB 空セッション/RID 循環列挙(SMB が null session を許可する場合)。(3)Kerberos ユーザー名列挙。KDC へ AS-REQ を送り、存在すれば KDC_ERR_PREAUTH_REQUIRED、存在しなければ KDC_ERR_C_PRINCIPAL_UNKNOWN を返すため、oracle として使える。
**Q2**: Kerberos で 5000 ユーザー名を列挙するとアラートを誘発しないか?
**A2**: 誘発する。速度は必ず毎秒 1〜2 回以下にし、発信元 IP を分散する。より良いのは、候補リストを先に OSINT で 200 以下に精緻化してから打つこと。デフォルトの Rubeus 全速列挙は一瞬で SIEM を点灯させる。
**Q3**: 存在するユーザー名をひととおり列挙したが、全員へ AS-REQ を送っても事前認証が必要と返る。誰も DONT_REQ_PREAUTH でない場合は?
**A3**: 想定内である。通常はごく少数のアカウントしか事前認証を無効化していない(歴史的遺留サービスアカウントに多い)。ひとつもなければ、AS-REP 経路は死と判定し、【転換】パスワードスプレーへ進む。2〜3 個の意味的弱いパスワードテンプレート(組織名/略称+年+記号、季節+年、「ようこそ/初期」類の語+数字)で全員をスプレーし、単点はブルートフォースしない(アカウントをロックする)。具体的な値は対象の文脈に即して生成し、ハードコードしない。
**Q4**: $krb5asrep$ hash をひとつ取得した。オフライン解読がずっと出ない。パスワードが本当に強いのか?
**A4**: 必ずしもそうではない。(a)暗号種別が AES でないか(一部ツールはデフォルトで RC4 をリクエストする。etype=23 を明示指定して KDC に RC4 版を返させられる。ポリシーが許可していれば)。(b)ユーザー名の大文字小文字と realm の大文字が hash 内と一致するか(一部の解読器はこれに敏感)。
**Q5**: この経路が完全に死んだ。次の直交方向は?
**A5**: 【転換】無認証情報条件下で、Kerberos 側にはまだ以下が残る。(1)プリンタ spooler bug + 非制約委任マシンで TGT を捕捉。(2)NTLM 中継(PetitPotam / Coerce → LDAP/ADCS/SMB)。(3)CVE 級(Zerologon/PrintNightmare/samAccountName)。(4)匿名 LDAP を試す。AS-REP で死闘しない。

### 非制約委任:識別できた。どう換金するか
**Q1**: LDAP 列挙で、あるサーバーの userAccountControl に TRUSTED_FOR_DELEGATION(非制約委任)が含まれるのを発見した。次は?
**A1**: 目標は「このマシンを利用する」ことではなく「高権限アカウントにそれへ認証させ、TGT をその LSASS に落とす」こと。前提:このマシンのローカル管理者権限(メモリ dump 可能)か SYSTEM で実行できる必要がある。したがって第一歩は「このマシンへ上がる」問題を先に解決する。
**Q2**: 既にこのマシンの admin だ。DC や DA に認証させ誘導するには?
**A2**: 古典は Spooler bug(SpoolSample/PrinterBug)で対象マシンアカウントに逆接続させる。PetitPotam/DFSCoerce/ShadowCoerce でマシン認証を強制もできる。トリガー後、本機メモリで TGT を待つ。
**Q3**: トリガーしたがメモリに TGT が見えず、TGS だけ見える場合は?
**A3**: 誘導対象が S4U を経由したか、既に制約付き委任で制限されているか、本機の Kerberos SSPI が TGT をキャッシュしていない(forwardable のみキャッシュ)ことを示す。(a)トリガー対象が本当に本機へ解決するか。(b)対象 SPN が本機を指すか。(c)対象アカウントが「機微、委任不可」とマークされていないか(Protected Users グループまたは NOT_DELEGATED フラグ)を確認する。
**Q4**: 対象 DA が Protected Users に加入、または NOT_DELEGATED フラグ付きで、転送可能 TGT を取れない場合は?
**A4**: 【転換】対象を変える。必ずしも DA でなくてよい。DCSync 権限を持ち Protected Users にいないアカウント(Exchange、バックアップサービス、古い運用アカウント)を探す。または【直交転換】RBCD/ADCS ESC 経路へ。
**Q5**: 「非制約委任マシン」と「似ているが実は RODC」をどう区別する?
**A5**: RODC も似たフラグを持つが、有用な TGT を発行できない。判定基準:(a)primaryGroupID が 521(RODC)か。(b)msDS-RevealOnDemandGroup / msDS-NeverRevealGroup が非空か。そうなら RODC であり、時間を無駄にしない。

### 制約付き委任 S4U:低権限サービスアカウントを取得後
**Q1**: サービスアカウントのパスワード/hash を取得した。その msDS-AllowedToDelegateTo が CIFS/DC01.domain を指す。何ができる?
**A1**: これは【制約付き委任 + Protocol Transition】の換金窓である。S4U2Self で任意ユーザー名(例: Administrator)として自分へのチケットを申請し、S4U2Proxy で CIFS/DC01 への TGS に交換する。前提:そのアカウントが TRUSTED_TO_AUTH_FOR_DELEGATION(T2A4D)フラグを持つ。持たなければ S4U2Self は forwardable チケットを取れない。
**Q2**: T2A4D フラグがなく、S4U2Self が non-forwardable を取り、S4U2Proxy が KRB_AP_ERR_BADMATCH を報告する場合は?
**A2**: CVE-2020-17049(Bronze Bit)を使う。S4U2Proxy リクエストで forwardable ビットを手動で反転させると、パッチ前の KDC は受け入れる。ドメインが未パッチなら通る。パッチ済みなら別経路へ。
**Q3**: パッチ済みで、対象ユーザーが Protected Users で S4U2Self が直接拒否される場合は?
**A3**: 【転換】RBCD または委任チェーンを探す。委任は連鎖できる。A が B の CIFS へ委任でき、B 上で動くサービスアカウントが DC へ委任できれば、連結できる。BloodHound の CanRDP/AllowedToDelegate エッジでチェーンを探す。
**Q4**: msDS-AllowedToDelegateTo 内の SPN が http/xxx で cifs/xxx でない。ファイルアクセスに変えられるか?
**A4**: 厳密には SPN 種別は検証されない(クライアントはリクエストした SPN に従う)が、KDC はパッチ適用版では sname 一致を検証する。sname substitution(Rubeus /altservice)で http を cifs/host/ldap に変えられる。古いバージョンの KDC は許可する。
**Q5**: このチェーンが完全に通らない。次の方向は?
**A5**: 【転換】SPN から逆算する。このサービスアカウントが委任権限を持つなら、それ自体も高価値アセットの owner である可能性が高い。どのオブジェクトに GenericWrite/WriteDACL を持つかを直接探し、他者の msDS-AllowedToActOnBehalfOfOtherIdentity を直接変更して RBCD できるかもしれない。

### リソースベース制約付き委任 RBCD:いつ最適解か
**Q1**: ある対象マシンオブジェクトに GenericWrite/GenericAll/WriteProperty を行えるアカウントを取得した。なぜ RBCD を優先する?
**A1**: RBCD は「被害者側」の設定であり、DA 権限なしで変更できる。msDS-AllowedToActOnBehalfOfOtherIdentity に書き込めば、自分が制御する「SPN 付きアカウント」が任意ユーザーとして対象へアクセスできる。
**Q2**: 「SPN 付きアカウント」がない。どう作る?
**A2**: デフォルトのドメインポリシーは一般ユーザーに 10 個のマシンアカウント作成を許可する(ms-DS-MachineAccountQuota=10)。impacket の addcomputer.py でマシンアカウントを作成すれば、マシンアカウントは自動的に SPN を持ち、直接使える。quota が 0 なら、既存の、自分がパスワード変更できるサービスアカウントで代用する。
**Q3**: MachineAccountQuota が既に 0 で、制御できる SPN アカウントもない場合は?
**A3**: 制御するアカウント自身が SPN を持つかを確認する(ValidatedWrite/servicePrincipalName 権限があれば自分で SPN を追加できる)。だめなら【転換】shadow credentials(msDS-KeyCredentialLink)+ PKINIT を使う。RBCD に依存せず対象の hash を取る。
**Q4**: msDS-AllowedToActOnBehalfOfOtherIdentity 書き込み後、S4U が KRB_AP_ERR_MODIFIED を報告する場合は?
**A4**: よくある原因:(a)対象マシンが古い委任設定をキャッシュしている。KDC チケットキャッシュ期限切れか 15 分待つ。(b)誤った KDC を使った(対象ドメインの DC を指すべき)。(c)クロックずれ。逐項排除する。経路が誤りだとは決めつけない。
**Q5**: 対象が DC だ。DC へ RBCD できるか?
**A5**: 技術的にはできる(DC オブジェクトに write 権限があれば)。ただし通常 DC の ACL は厳格に守られている。本当にあれば、直接 S4U で DC の cifs/ldap へ行き、DCSync で完了。これは「一撃必殺」級の機会で、無駄にしない。
**Q6**: 全部通らない場合は?
**A6**: 【直交転換】shadow credentials(msDS-KeyCredentialLink 利用)は対象オブジェクトへの GenericWrite だけでよく、直接 NT hash を交換できる。RBCD よりクリーンで動きが小さい。

### Kerberos クロックずれで KRB_AP_ERR_SKEW に拒否される
**Q1**: TGT をリクエストすると clock skew too great が繰り返される。自分の攻撃機と DC が 8 分ずれている。どうする?
**A1**: Kerberos はデフォルトで ±5 分を許容する。`rdate`/`ntpdate`/`net time \\dc` で DC 時刻へ同期する。公衆 NTP は使わない。対象ドメインと時刻がずれる可能性がある(ドメインが意図的にずらしている、または内網 NTP を使っている)。
**Q2**: 本機のクロックを変更する権限がない(コンテナ/サンドボックス)場合は?
**A2**: faketime のようなツールでプロセスレベルで時刻を偽造するか、ツールを変更してプロトコル層で DC のタイムスタンプを使う(impacket は -k をサポートするがやはりシステム時刻に依存)。最も省力なのは攻撃機の時刻を DC へ同期すること。
**Q3**: DC へ同期してもまだ skew と出る。防御に識別されたか?
**A3**: (a)タイムゾーン。Kerberos は UTC を使うがツールがローカルオフセットを付けることがある。(b)誤った KDC を打ったか(例: クロスドメインの DC)。(c)DC 自体が仮想マシンで時刻がドリフトしていないか(稀だが存在)。
**Q4**: クロックを直せない場合は?
**A4**: 【転換】NTLM 経路へ進む。NTLM はクロックに敏感でない。または既にドメイン内にあり、クロックが正しい中継機を探し、Kerberos 対話をその上で行う。

### TGT/TGS を取得後、どう着地して使うか
**Q1**: 手元に .ccache または .kirbi チケットがある。Linux 攻撃機で impacket に使いたい。どうする?
**A1**: `export KRB5CCNAME=/path/to/ticket.ccache` とし、全 impacket スクリプトに `-k -no-pass` を付け、`-dc-ip` で対象 DC を指定し、同時に /etc/hosts に対象マシンと DC の FQDN 解決を追加する(Kerberos は厳格に SPN に従い、IP 直結は失敗する)。
**Q2**: kirbi を ccache へ変換するツールは?
**A2**: impacket の ticketConverter.py、または Rubeus で直接 /ptt で現在のセッションへ注入。注意:kirbi はリトルエンディアン保存の ASN.1 で、手で改変しない。
**Q3**: チケットを注入したが `klist` が空と表示する場合は?
**A3**: (a)KRB5CCNAME が正しい場所を指すか(一部システムはデフォルト /tmp/krb5cc_UID)。(b)権限ビットが 600 か。(c)ccache 形式が FILE: か(一部システムはデフォルト KEYRING:)。
**Q4**: 対象 SMB が KDC_ERR_S_PRINCIPAL_UNKNOWN を報告。チケットは正常のはず?
**A4**: SPN 不一致。チケットリクエスト時の sname がアクセス時の SPN と一致しなければならない(cifs/hostname.fqdn であって cifs/IP ではない)。FQDN を使い、大文字小文字を SPN 登録と一致させる。
**Q5**: ずっとプロトコル層エラーだ。環境がそもそも非対応なのか?
**A5**: 【転換】Kerberos 着地を諦め、PtH/NTLM で着地する。Kerberos への潔癖は自ら招いたもの。場面が NTLM を許すなら NTLM を使う。純粋性のために強行しない。

### Silver Ticket:サービスアカウント hash を取得後の一段階昇天
**Q1**: MSSQL サービスアカウントの NT hash を解読した。直接何ができる?
**A1**: Silver Ticket を偽造して、その SPN に対応するサービスへ直接アクセスする。KDC を迂回し、4768/4769 イベントを生まない(対象機上の 4624 のみ)。「低調・単点・データ深掘り」の場面に適する。
**Q2**: Silver Ticket で CIFS へアクセスしてエラー。hash が誤りか?
**A2**: (a)このアカウントが本当に cifs/ SPN を登録しているか(mssql/ ではなく)。Silver Ticket は SPN ごとに偽造する。SPN が違えば使えない。(b)ドメイン SID が正しいか。(c)サービスアカウントが AES を有効化しているか。有効なら NT hash ではなく aes256 key を使う。
**Q3**: 対象が PAC 署名検証を有効化している(KB5008380/CVE-2022-37967)。Silver Ticket はまだ使える?
**A3**: パッチ後、PAC 完全署名と拡張 KDC 署名には krbtgt key が必要で、単純なサービスアカウント hash で偽造した Silver Ticket は拒否される。【転換】本物の TGS(overpass-the-hash で正常な KDC を経由して交換)または Golden Ticket へ切り替える。
**Q4**: ではこの hash はまだ何に使える?
**A4**: 最低限 overpass-the-hash で本物の TGS をリクエストし、そのアカウントの local admin のマシンへ横展開できる。また、GPP パスワードや旧アプリ接続文字列を解く鍵かもしれない。hash を Silver Ticket 材料としてだけ見ない。

### Golden Ticket をいつ作るべきか、いつ作らないべきか
**Q1**: krbtgt hash を取得した。今すぐ Golden Ticket を作って全域漫遊する?
**A1**: まず冷静になる。Golden Ticket は極めて目立つ(デフォルト 10 年有効期間、PAC 内の group を満載、TGT が稀な etype を使う)。現代の EDR/SIEM には専用ルールがある。作る前に:(a)対象ドメインが PAC 検証パッチを強制していないことを確認。(b)有効期間を合理的な値(数時間から数日)に変更。(c)暗号種別とユーザー名を本物の KDC 出力と一致させる。
**Q2**: 作ったが使えない。KRB_AP_ERR_MODIFIED を報告する場合は?
**A2**: 九割は domain SID の誤り、krbtgt バージョン(kvno)の誤り、または krbtgt パスワードが既に一度ローテーションされ hash が失効している。SID は `whoami /user` のドメイン部分で照合し、kvno は mimikatz で DC 上から読む。
**Q3**: Golden Ticket を作って 5 分未満で蹴られた場合は?
**A3**: リアルタイム監視がある(4769 内の TGS リクエスト源が偽造 TGT である判別に基づく可能性)。【転換】Golden を減らし Silver を増やすか、DCSync + Skeleton Key へ進むか、krbtgt hash を静かに使ってより隠密な Diamond/Sapphire Ticket(必要なフィールドだけを偽造し、PAC を本物の TGT からコピー)を作る。
**Q4**: Golden Ticket が最適解になるのはどんな場面?
**A4**: 一度きりのタスク(競技/演習の「旗取り」段階)、任意ユーザーになりすましたクロスドメイン漫遊、krbtgt を取得済みで相手が明日ドメインを再起動する窓期。通常のレッドチーム実戦では Silver + DCSync の組み合わせを好む。持続的でより隠密。

### DCSync 権限判定基準
**Q1**: BloodHound が、あるアカウントが GetChanges + GetChangesAll を持つと教えた。即 DCSync すべきか?
**A1**: するが、先にコスト収益を計算する。DCSync は DC 上に 4662 イベント(DS-Replication-Get-Changes GUID 付き)を生み、防御側のルールは非常に標準的。DCSync するなら DC から攻撃機へデータを引く経路が要る。経路がファイアウォールを通れるかを先に確認する。
**Q2**: 経路が通らない(DC の出口が封鎖)場合は?
**A2**: ドメイン内のマシンに impacket-secretsdump を落とし、DIT を中継機へ引いてから回収する。または単一ユーザーだけ同期する(secretsdump は -just-dc-user krbtgt をサポート)。データ量と特徴を減らす。
**Q3**: DCSync が DRSUAPI RPC access denied を報告。権限は確かにあるのに?
**A3**: (a)GetChangesInFilteredSet も同時に必要か(ドメインが confidentiality bit を有効にし、一部属性を読むときに要求)。(b)RODC か(RODC は与えられない)。(c)誤った DC を打ったか(対象ドメインの rw DC を打つべき)。
**Q4**: 権限がないと確認した。どう「曲線取得」する?
**A4**: 【転換】(a)Domain オブジェクトに WriteDACL を持つアカウントを探し、自分に DCSync 権限を追加する(動きが大きい)。(b)DC ホストから直接 NTDS.dit + SYSTEM を取る(マシン admin が必要)。(c)shadow copy / ntdsutil へ進む。

### 委任+ADCS 連動:いつ切り替えるか
**Q1**: 委任チェーンを進めているが、ドメインに ADCS があり Enterprise CA があると判明した。切り替えるべきか?
**A1**: 強く評価を推奨する。ADCS ESC1/ESC4/ESC8 の一部は「任意ユーザーを一段階で DA に」できる。委任チェーンは二〜三ホップ要る。判定基準:(a)certutil または Certipy で申請可能なテンプレートを列挙。(b)UPN カスタマイズ可/ClientAuth EKU/低申請権限があれば、ESC をひとつ選んで打つ。
**Q2**: ADCS がない、またはテンプレートが全部硬い。委任を続けるべきか?
**A2**: 続ける。新奇な技術のために無理に切り替えない。委任チェーンは ADCS が不在のドメインで最も安定した権限昇格方式である。
**Q3**: 両経路とも通る。どちらを選ぶ?
**A3**: 隠密性:ADCS の証明書リクエストイベントは通常のアラートに少ないが、4886/4887 は専門防御に捕捉される。委任チェーンの 4769 リクエストは頻繁だが、正常トラフィックに埋もれる。対象ドメインのログ成熟度による。
**Q4**: ADCS を進めて半分で CA サーバーがオフラインと判明した場合は?
**A4**: 【転換】即座に委任または shadow credentials(CA 不要で DC PKINIT だけでよい)へ切り替える。サービスの修復を待つようなことに時間を費やさない。

### プリンタ Spooler 強制認証が無効化された
**Q1**: SpoolSample で DC に非制約委任マシンへ逆接続させたいが、spooler サービスが無効化されている。どうする?
**A1**: 【直交転換】他の coerce 技術。PetitPotam(EFSRPC)、DFSCoerce(NetrDfsAddStdRoot)、ShadowCoerce(FSRVP)、CheeseOunce(WebClient)。それぞれ依存するサービスとパッチバージョンが異なる。DC が開放する named pipe / サービスを列挙してから選ぶ。
**Q2**: 全 coerce を試したが反応がない。WebClient 系パッチで塞がれたか?
**A2**: (a)DC のパッチバージョン(KB 番号)。(b)SMB 署名が強制か(強制なら relay も通らない)。(c)EDR が EFSRPC を能動遮断していないか。全部塞がれば【転換】coerce 非依存の経路へ。AS-REP、パスワードスプレー、ADCS。
**Q3**: coerce は成功したが hash/ticket を取得できない?
**A3**: リスニング端(ntlmrelayx / Responder / krbrelayx)の設定が正しいか、リスニングポートがファイアウォールに遮られていないか、SMB 署名要求を確認する。取れないのはチェーンが閉じていないのであって、coerce の失敗ではない。

### samAccountName Spoofing(CVE-2021-42287/42278)の利用可能性判定基準
**Q1**: 一般ドメインユーザーを持ち、DC が 2021 年 11 月以降のパッチを当てていない。一段階で DA へ行けるか?
**A1**: 見込みあり。前提:(a)MachineAccountQuota > 0 でマシンアカウントを作成できる。(b)自分のアカウントの sAMAccountName 属性を変更できる。noPac 攻撃フロー:fake マシンアカウント作成 → その名前を DC に変更($ を外す)→ PAC 付き TGT をリクエストすると KDC が PAC 内の sid を本物の DC に設定 → S4U2Self で DA チケットを取得。
**Q2**: パッチ済み(2021-11 以降)でも使えるか?
**A2**: ほぼ封鎖。時間を無駄にせず、【転換】他の CVE(Zerologon も未パッチかもしれない。先に試す)または ADCS/委任へ。
**Q3**: MachineAccountQuota=0 の場合は?
**A3**: sAMAccountName を変更できる既存アカウント(WriteProperty が必要)か、Kerberoast で得たサービスアカウントを探し、同じテクニックを適用する。核心は「マシンアカウントの作成」ではなく「KDC にマシンとして扱われるアカウントの制御」である。
**Q4**: マシンアカウントを作ったが改名でエラー?
**A4**: (a)命名衝突(DC ホスト名が既存)。衝突しない過渡名を使う。(b)sAMAccountName 属性が GPO で変更制限されていないか。(c)ACL に書き込み拒否されていないか。

### パスワードスプレーと Kerberos の組み合わせ:まず SMB か Kerberos か
**Q1**: パスワードスプレーをしたいが、SMB がアカウントロックポリシーを誘発するのが心配だ。Kerberos でできるか?
**A1**: できる。推奨もする。Kerberos AS-REQ でスプレーする(kerbrute passwordspray)。プロトコル層が一〜二段少なく、4771 を生む(4625 ではない)。ただし badPwdCount はやはり累積する。
**Q2**: ではロックリスクは?
**A2**: やはりある。badPwdCount が閾値に達すればアカウントをロックする。戦略:各ラウンドの試行回数 <= (lockout threshold - 2) とし、ラウンド間に observation window が過ぎるのを待つ(デフォルト 30 分)。速度が遅いのは必要な代償。
**Q3**: ドメインに明確なロックポリシーがない(閾値 0)場合は?
**A3**: 飛べる。ただし並行数を制御する。4771 の洪峰はやはりアラートする。合理速度は毎分 10 回以下、発信元を分散。
**Q4**: 一ラウンドスプレーして全 preauth failed、ひとつも成功しない?
**A4**: (a)ユーザー名形式が UPN(user@REALM)であるべきか、sAMAccountName ではないか。(b)マシンアカウントも一緒にスプレーしていないか(全部失敗)。(c)realm の大文字小文字。全体ゼロヒットはしばしば形式の問題であってパスワードの問題ではない。

### gMSA パスワードの取得経路
**Q1**: LDAP 列挙で gMSA アカウント(msDS-ManagedServiceAccount)を見つけた。通常のサービスアカウントと何が違う?
**A1**: パスワードは KDC が 30 日ごとに自動ローテーションし、複雑度が極めて高い(240 バイト)。Kerberoast はオフライン解読がほぼ絶望。ただし msDS-ManagedPassword 属性は特定グループ(PrincipalsAllowedToRetrieveManagedPassword)が読め、平文 blob を直接取得できる。
**Q2**: PrincipalsAllowedToRetrieveManagedPassword にいない場合は?
**A2**: その中にいるアカウントを探すか、gMSA オブジェクトに WriteProperty 権限を持つアカウントを探して自分を追加する。gMSAdumper ツールが blob を読み NT hash へ変換できる。
**Q3**: blob を読んだが gMSAdumper がエラー。どうする?
**A3**: (a)LDAP over SSL が通るか(msDS-ManagedPassword は confidential attribute で LDAPS 必須)。(b)クロックずれ。(c)currentPassword セグメントを読んでいるか(blob 構造に current/previous/query interval がある)。
**Q4**: 完全に読めない場合は?
**A4**: 【転換】gMSA は通常、あるマシン上でサービスを動かす。そのマシンの SYSTEM 権限を取れば、マシン自身が blob を読める。ACL を迂回する方式は「この gMSA を使うマシンへ行く」こと。

### クロスドメイン/クロスフォレスト信頼下の Kerberos 攻撃
**Q1**: sub.corp.local を取った。目標は corp.local(親ドメイン)。Kerberos 経路はどう進む?
**A1**: 古典経路は SID History 付き Golden Ticket の偽造。前提:子ドメイン krbtgt hash を取得し、TGT 偽造時に --sids に親ドメインの Enterprise Admins SID(S-1-5-21-...-519)を加える。パッチ前は KDC が SID Filtering を検証しない。
**Q2**: 親ドメインが SID Filtering(quarantine)を有効化している場合は?
**A2**: 【転換】(a)子ドメイン DA で親ドメインへ信頼アカウント(TDO、trust key)を探して trust ticket を取り、inter-realm TGT を forge。(b)非制約委任で親ドメイン DA に逆接続させる。(c)親ドメイン CA が子ドメインユーザーを信頼するなら ADCS へ。
**Q3**: フォレスト信頼(ドメイン信頼でない)は?
**A3**: フォレスト内親子ドメイン信頼はデフォルト双方向・推移的で、SID History が通る。フォレスト間信頼はデフォルト SID Filtering が有効で、難度が急上昇。フォレスト間は非制約委任 + coerce、または設定誤りの TGT delegation trust 属性を利用する。
**Q4**: 全部通らない。対象フォレストへ横展開する必要はまだあるか?
**A4**: あるが、Kerberos を経由しない可能性がある。両フォレスト間に共有の Web/SSO/Federation(ADFS/Azure AD Connect)があるかを確認する。それらは Kerberos 以外の横展開経路である。

### Kerberos リクエストをプロキシ経由で: SOCKS か impacket -k か
**Q1**: C2 内にいて、SOCKS でしか内網へ行けない。impacket -k は使えるか?
**A1**: 使えるが落とし穴が多い。Kerberos は厳格に DNS と時刻に依存する。SOCKS は TCP をサポートしなければならない(一部の proxychains 版は UDP サポートが限定的で、Kerberos AS-REQ は UDP 88 を使うと失敗する)。proxychains-ng を使い、TCP 88 を強制する(KDC の大部分は TCP fallback をサポート)。
**Q2**: TCP 88 は通ったが DNS が通らない場合は?
**A2**: 手動で /etc/hosts に FQDN を追加し、環境変数または krb5.conf で KDC アドレスを指定して、DNS SRV ルックアップを迂回する。
**Q3**: プロキシ帯域が小さく、DCSync で全域を引くのが遅い場合は?
**A3**: 重要アカウント(krbtgt、DA グループメンバー)だけ dump し、secretsdump の -just-dc-user を使う。全域を一括で引かない。
**Q4**: プロキシが不安定で断続的に切れる。Kerberos 対話がいつも失敗する?
**A4**: 【転換】Kerberos の段を内網中継機へ置き、C2 はコマンド送信だけにする。リモート実行で得た hash/チケットを中継機のディスクへ落とし、その後単一ファイルで回送する。往復を減らす。

### Kerberoast 出力の hash 形式が認識されない
**Q1**: Rubeus が出力する $krb5tgs$23$ 形式を hashcat が認識しない。どうする?
**A1**: バージョン問題。hashcat のバージョン(-m 13100)と hash の先頭形式を確認する。一部の Rubeus 版はフィールド順序やチェックサム位置に出力差がある。impacket の GetUserSPNs.py で再度取得して比較できる。
**Q2**: hash 形式は合っているが hashcat が no hashes loaded と言う?
**A2**: よくあるのはファイルエンコーディングが UTF-16(Rubeus が Windows コンソールへ出力し BOM が付きうる)。dos2unix / iconv で UTF-8 の BOM なしへ変換する。
**Q3**: 全部合っているが解読結果が出ない?
**A3**: john に切り替える。一部の etype=18 の hash は john の方が対応が良い。または hash 内の realm、user の大文字小文字が本物と一致するか確認する。
**Q4**: 全部切り替えても解けない?
**A4**: このアカウントのパスワードが本当に複雑か、既に gMSA を使っていることを示す。作業を終えて対象を変えるべき。算力で時間を消費しない。

### mimikatz のないマシンで TGT を抜く
**Q1**: 対象マシンに EDR があり、mimikatz が即死。メモリ内の TGT をどう抜く?
**A1**: 【転換】mimikatz を使わない。(a)Rubeus の monitor / triage / dump で LSA から抜く(ただし EDR に遮られうる)。(b)comsvcs.dll MiniDump で procdump lsass.exe の dmp を取り、ローカルで解析。(c)nanodump/dumpert のような syscall 直呼びツール。(d)ディスクから registry hive(SAM/SECURITY)を取る。TGT はないがアカウント hash はある。
**Q2**: LSASS が PPL 保護で dump できない場合は?
**A2**: (a)mimidrv ドライバをロードして迂回(admin が必要、動きが大きい)。(b)signed driver の LOL(既知の脆弱ドライバ BYOVD)を探す。(c)直接 dump を諦め、shadow credentials や delegation でプロトコル層からチケットを取る。
**Q3**: 全部だめな場合は?
**A3**: 【直交】パスワードから入るのであってチケットからではない。キーロガー、ブラウザ認証情報、DPAPI、SCCM クライアント設定ファイルの平文パスワード。これらは LSASS を経由しない。

### マシンアカウント hash だけで何ができるか
**Q1**: MACHINE$ の NT hash(一般ユーザーでない)を取得した。何ができる?
**A1**: マシンアカウントも自分の TGT をリクエストできる。(a)マシン身元になりすまして、それがアクセス権を持つリソースへアクセス。(b)このマシンが DC なら直接 DCSync。(c)このマシンが RBCD/委任チェーンに設定されていれば利用を続ける。
**Q2**: マシンアカウントのパスワードはランダム 120 文字。hash をどう使う?
**A2**: hash があれば十分で、平文は不要。overpass-the-hash で hash から TGT をリクエストするか、直接 pass-the-hash で SMB へ行く(マシンアカウントは自分のマシン上では SYSTEM 相当)。
**Q3**: マシン hash が時間経過で失効する。どうする?
**A3**: マシンアカウントはデフォルト 30 日ごとにパスワードをローテーションする。鮮度を保つには:(a)抽出後即使用。(b)ローテーションを無効化(GPO またはレジストリ DisablePasswordChange を変更。動きが大きい)。(c)より良いのは、hash を取得したら即永続化(shadow credential 作成、RBCD 追加)し、hash 自体に依存しないこと。
**Q4**: マシンアカウント hash で他マシンへ横展開できない場合は?
**A4**: 想定内である。マシンアカウントはデフォルトで自分にしか権限がない。その価値は「S4U/RBCD の中間ブリッジとして使う」ことであって、直接横展開ではない。

### shadow credentials(msDS-KeyCredentialLink)をいつ使うか
**Q1**: あるアカウントに GenericWrite がある。hash を取るのに targetedKerberoast か shadow credentials か?
**A1**: shadow credentials の方が強い。相手アカウントに自分の制御する公開鍵を直接追加し、PKINIT で NT hash と TGT を交換する。相手のログオンを待たず、ブルートフォースも不要。前提:ドメインに ADCS があるか、DC が 2016+ で PKINIT をサポート。
**Q2**: ドメインに ADCS がない場合は?
**A2**: DC が PKINIT をサポートすればよい。ADCS は必ずしも要らない(DC は KDC 認証用の自己署名証明書を持つ)。pywhisker/certipy で msDS-KeyCredentialLink を追加し、gettgtpkinit.py で TGT + PAC_REQUESTOR から NT hash を取る。
**Q3**: PKINIT リクエストが KDC_ERR_CLIENT_NOT_TRUSTED を報告?
**A3**: (a)自分が追加した keyCredential 構造が正しいか(DeviceID/KeyMaterial)。(b)DC 時刻。(c)DC が PKINIT を無効化していないか(極めて稀)。または書き込み可能な別対象を直接試す。
**Q4**: 対象アカウントの msDS-KeyCredentialLink に既に値がある。上書きすると正常ログオンを壊さないか?
**A4**: 多値属性であり、追記は上書きしない。ただし操作後に清掃する。レッドチームはロールバック可能性を重んじる。

### サービスアカウントのパスワードポリシー推測
**Q1**: 長く打ってもサービスアカウントのパスワードが複雑だ。ポリシーが強いのか?
**A1**: ドメインパスワードポリシーを確認する(net accounts /domain または LDAP で pwdProperties を読む)。同時に細粒度パスワードポリシー(PSO)がサービスアカウント OU を個別に緩めていないかを見る。ドメイン全体が強くても、歴史的遺留のサービスアカウント OU に 4 桁数字ポリシーがあることがある。
**Q2**: PSO を読む権限がない。どう推測する?
**A2**: 解けた pattern から逆算する。解けたのが全部 8 桁ならポリシー最小は 8 の可能性が高い。全部大文字小文字数字付きなら複雑度有効。全部特殊記号なしなら記号要件が未有効の可能性。サンプル蓄積は有用。
**Q3**: 完全に解けない場合は?
**A3**: 【転換】誰がこれらのアカウントのパスワードを変更できるかを見る。サービスアカウントに ForceChangePassword 権限を持つアカウントを制御していれば、解くのではなく直接変更する。BloodHound に専用エッジがある。

### Kerberos over HTTPS(Kerberos KDC Proxy)は使えるか
**Q1**: 外網は 443 しか露出していない。KDC Proxy で公網から Kerberos できるか?
**A1**: できる。対象ドメインが KKDCP(Kerberos Key Distribution Center Proxy Protocol、MS-KKDCP)を有効化していれば。判定基準:/KdcProxy エンドポイントが開放されているか(HTTPS POST)。
**Q2**: エンドポイントは開いているがリクエストが拒否される?
**A2**: KKDCP は realm 一致と証明書検証を厳格に要求する。対象が自社 VPN クライアント専用で、IP ホワイトリストの場合がある。
**Q3**: KKDCP が使えない。外網から Kerberos を打ちたい場合は?
**A3**: 【転換】ADFS/Azure AD ハイブリッド身元の経路、または先に VPN/tunnel を張って内網へ入ってから打つ。無理に続けない。

### Kerberos 攻撃を打ち終えた後の清掃
**Q1**: ptt でチケットを注入した。作業後どう消す?
**A1**: `klist purge`(Windows)または `kdestroy` / rm ccache ファイル(Linux)。ディスクに残さない。メモリ注入は再起動で消える。
**Q2**: DC 上の 4768/4769/4771 ログは既に生成された。消せるか?
**A2**: 通常できない。DC イベントログを消すには DA 権限が必要で、動きが巨大。ログ消去自体が極めて強いアラートシグナルである。レッドチームの戦略は「テンポを正常トラフィックに隠す」ことであってログ消去ではない。
**Q3**: 偽造した Golden Ticket を使い終えた。痕跡を消すために krbtgt をリセットすべきか?
**A3**: 自分がやるべきではない。レッドチームは防御側の仕事をしない。ただし認識すべき:krbtgt が一度漏えいすると、防御側は krbtgt を連続二回リセット(kvno+2)して初めて完全に消せる。自分が永続化でこれを利用したなら、まだ窓がある。

### Kerberos 攻撃が EDR のインライン hook で全滅
**Q1**: Rubeus が EDR の前で即死し、チケットすらリクエストできない。どうする?
**A1**: プロトコル層実装へ転換する。impacket を Linux 攻撃機から SOCKS で内網へ走らせ、Windows 上で Rubeus/mimikatz を一切実行しない。全 Kerberos 対話をプロキシの反対側で完結させる。
**Q2**: SOCKS が通らない。どうしても Windows 上でやる場合は?
**A2**: (a)Rubeus のソースレベルで文字列変更+コンパイル。(b)C2 経由の execute-assembly でメモリロード。(c)SharpKatz/他冷門実装へ切り替え。(d)必要な一歩だけを行う自前の小ツールを書く(例: S4U だけ)。
**Q3**: 上記全部が殺される場合は?
**A3**: 【直交転換】Kerberos をやらない。LDAP 攻撃(GenericWrite ACL abuse は LDAP だけでよく、KDC に触れない)、または ADCS で HTTPS、または SMB で relay へ。Kerberos は唯一の次元ではない。

### 委任チェーン計画:BloodHound のエッジをどう読むか
**Q1**: BloodHound 図で、AllowedToDelegate/AllowedToAct/AddAllowedToAct/ForceChangePassword/HasSPN をどう組み合わせるのが最適?
**A1**: 目標は「自分」から「対象」への最短経路。経路スコアリング:各エッジに「コスト」(要る操作数)がある。ForceChangePassword = 1 手(パスワード変更。ただし業務に影響)、AllowedToDelegate = 1 手(直接 S4U)、AddAllowedToAct = 2 手(RBCD 変更 + S4U)。コストが小さく業務影響の小さいものを選ぶ。
**Q2**: BloodHound データが古い(3 日前)。今も正確か?
**A2**: ACL 変化は頻繁で、グループメンバー変化はさらに頻繁。重要な飛び点は再列挙する(SharpHound -c Session,DACL で確認)。旧図を直接信じて打たない。
**Q3**: BloodHound が経路を見つけない。本当にないのかデータ不全か?
**A3**: 高確率でデータ不全。SharpHound はデフォルトで一部エッジ(ACL 深度、GPO チェーン)を採取しない。-c All で再採取するか、手動 LDAP で nTSecurityDescriptor を照会して補完する。

### Kerberos etype 降格:RC4 を強制する
**Q1**: 対象アカウントが AES をサポートするが、KDC に RC4 暗号で返させたい。できるか?
**A1**: クライアントは AS-REQ/TGS-REQ で etype 順序を指定し、KDC はクライアントがサポートし、アカウントもサポートする最初のものを選ぶ。したがってリクエストで etype=23 だけを宣言すれば、アカウントの msDS-SupportedEncryptionTypes が RC4 を許可する限り、KDC は RC4 暗号の hash を返す。
**Q2**: アカウントが AES only を強制(msDS-SupportedEncryptionTypes が aes のみ)の場合は?
**A2**: RC4 降格はできない。AES を解くか、【転換】他のアカウントへ。
**Q3**: ドメインポリシー「Kerberos RC4 を使用しない構成」が有効化されている場合は?
**A3**: KDC は RC4 リクエストを拒否する。判定基準は KDC_ERR_ETYPE_NOSUPP。この種のドメインは防御成熟度が高いことが多い。全体の経路切り替えを検討する。

### RODC 攻撃の反噬リスク
**Q1**: RODC を識別した。直接 dump してドメイン内全ユーザー hash を取れるか?
**A1**: できない。RODC は msDS-RevealedList 内のアカウント(通常は当該サイトの少数ユーザー)だけをキャッシュし、DA をキャッシュしない。RODC を dump しても一部アカウントしか取れない。
**Q2**: では RODC は何に使える?
**A2**: (a)RODC 上に KRBTGT_XXXXX アカウント(番号付き)があり、この hash で「部分ドメイン」の Golden Ticket を作れる。署名した TGT は RODC にしか認められず、rw DC には認められない。ただし既知の脆弱性で検証を迂回する場合を除く。(b)RODC から msDS-Reveal-OnDemand-Group を見て、露出した機微アカウントを逆算する。
**Q3**: RODC を打つ手は軽いか重いか?
**A3**: 軽い。RODC は通常支社にあり、監視が本社ほど厳しくない。ただしその hash 漏えいは全域漫遊に直接つながらない。過度に期待しない。

### 委任チェーンが Kerberos Armoring(FAST)に遭遇
**Q1**: ドメインが Kerberos Armoring / Compound Identity を有効化している。委任攻撃は全滅か?
**A1**: 全滅ではないが難度が大幅に上がる。FAST は AS-REQ を armor TGT で包み、事前認証部分にオフライン解読可能な素材がなくなり、AS-REP Roasting は直接死ぬ。ただし Kerberoast(TGS-REP)はまだ使える可能性があり、S4U もまだ使える。
**Q2**: 判定基準は何か?
**A2**: GPO で「KDC サポートのクレーム、複合身元検証と Kerberos Armoring」が「常にクレームを提供」または「装甲されていない認証リクエストを失敗させる」に設定されているか。klist またはパケット取得で AS-REQ に armor フィールドがあるかを見る。
**Q3**: FAST があるが AS-REP 系の操作をしたい場合は?
**A3**: 【転換】パスワードスプレー(4771 は生じるが解読可能な blob はない)または Kerberos を完全に迂回する経路(NTLM relay で LDAP/HTTP/ADCS へ)。

### 「SPN があるように見える」アカウントの TGS リクエストが失敗する
**Q1**: LDAP があるアカウントに SPN があると示すが、その TGS をリクエストすると KDC_ERR_S_PRINCIPAL_UNKNOWN を報告する。どうする?
**A1**: 可能性:(a)SPN がこのアカウントに登録されているが servicePrincipalName 属性のデータが汚れている(重複/大文字小文字異常)。(b)アカウントが disabled だが SPN が消えておらず、KDC が拒否。(c)SPN 内のホスト名が DNS 解決しない。
**Q2**: 全部排查しても失敗する?
**A2**: UPN リクエストに切り替える(sname を SPN ではなく sAMAccountName@REALM にする)、または Rubeus の targetedKerberoast でアカウントに一時的に SPN を追加してからリクエストする(書き込み権限が必要)。
**Q3**: 自分に SPN を追加したいが拒否される?
**A3**: ValidatedWrite/SPN 関連の書き込み権限を確認するか、SPN duplicate チェックがあるかを見る。一部のドメインは「SPN 全域一意」を強制し、適当な追加が失敗する。

### Protected Users グループの高価値対象に遭遇
**Q1**: DA グループメンバーが全員 Protected Users グループにいる。まだ打てるか?
**A1**: Protected Users メンバーには多重制限がある。委任不可、RC4 不可、TGT は 4 時間のみ、NTLM 認証不可、DPAPI をオフライン解読不可。委任経路はほぼ封鎖。
**Q2**: ではどうする?
**A2**: 【転換】(a)Protected Users にいないが DCSync 権限を持つアカウントを探す。(b)マシンアカウント + RBCD へ進む(マシンアカウントは通常 Protected Users にいない)。(c)ADCS ESC へ進む(証明書認証は Protected Users の影響を受けない)。(d)直接 DC 上へ着地して lsass を取る。
**Q3**: 判定基準を先に確認するか?
**A3**: する。LDAP で memberOf=CN=Protected Users を照会する。推測に頼らない。Protected Users グループが存在しても使われていない(空グループ)ことがある。

### 手元に hash が山ほどあるが、どのマシンに入れるか分からない
**Q1**: 10 アカウントの hash を解いた。どのアカウントがどのマシンに local admin か分からない。どうする?
**A1**: crackmapexec / netexec で一括。hash + ホスト名リストで SMB を並行探索する。Pwn3d! が返ったものが local admin。平文パスワードは使わず、直接 pass-the-hash。
**Q2**: 全部 STATUS_ACCESS_DENIED を返す。hash が無用か?
**A2**: 必ずしもそうではない。(a)ドメインアカウントだが UAC リモート制限が遮っていないか(--local-auth を閉じるか named pipe 経由が必要)。(b)SMB signing が強制か(それは pth に影響しないが relay に影響)。(c)対象が別ドメイン/ワークグループに加入していないか。
**Q3**: 全部 local admin がない。hash はまだ使える?
**A3**: 使える。それらで TGT をリクエスト(overpass-the-hash)し、Kerberos で各種サービスの TGS をリクエストする。これらのアカウントが特定のアプリサービス、SQL、SharePoint へのアクセス権を持つかもしれない。業務側で換金する。

### 時間予算:Kerberos 経路をいつ諦めるか
**Q1**: Kerberoast/AS-REP/委任に既に 6 時間費やした。成果なし。続けるか転換するか?
**A1**: 判定基準:(a)各ステップで「防御が効いている」シグナル(FAST/AES only/Protected Users)に遭遇しているか。そうなら防御が成熟しており、転換。(b)単に「運が悪く弱いパスワードを解けていない」だけか。そうならさらに 2 時間の価値がある可能性があるが、ハード上限を設ける。
**Q2**: どこへ転換するのが最も時間節約か?
**A2**: 優先順位:ADCS ESC 列挙(15 分で結果) > BloodHound ACL 経路(30 分で結果) > NTLM relay + coerce(1 時間) > Web 面から Windows Auth アプリ。単一次元に死闘しない。
**Q3**: いつが「本当に死んだ」シグナルか?
**A3**: ドメインが完全 Tier 化、PAW あり、Protected Users 全員、AES only、FAST 強制、ADCS に ESC なし、coerce 可能なサービスなし、EDR がインライン hook で全 lsass 保護。四項目以上現れたら、手法を変えるより対象を変えることを検討する。
