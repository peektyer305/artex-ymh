# ADドメイン内 · 認証情報収集:mimikatz/SAM/チケット

### 本機の SYSTEM を取得したら、最初に何を収集するか
**Q1**: 既に SYSTEM だ。LSASS にドメインユーザー/サービスアカウントの平文と NTLM があるかもしれないが、今回の権限を無駄にしないために最初に何をすべきか?
**A1**: まず「採取してから操作」する。`whoami /all`、`klist`、現在のプロセス一覧、AV/EDR 名(`Get-MpComputerStatus`、tasklist `/svc`)、および LSA Protection が有効か(RunAsPPL レジストリキー)をスナップショットする。一度 EDR をトリガーすると、LSASS の着地が遮断され、プロセスが kill され、観察窓まで失われる。まず防御態勢を把握してから、直接 dump か迂回かを決める。
**Q2**: 主流 EDR が常駐している。直接 minidump で LSASS を取ると高確率で kill される。どうする?
**A2**: 「LSASS に直接触れない」経路へ転換する。まず他の鉱脈を探す。SAM+SYSTEM レジストリ hive(reg save)、DPAPI master key + ユーザー認証情報/Chrome/RDP 保存、`cmdkey /list`、スケジュールタスク内のアカウント、IIS applicationHost.config、SQL Server 接続文字列、SYSVOL 内の cpassword 遺留。これらは Win32 の正常 API を使うため、EDR のアラート面がはるかに小さい。
**Q3**: 判定基準は何か。いつ LSASS に触れるリスクを冒す価値があるか?
**A3**: 「ドメインコントローラまであとひとつのドメインユーザー/ドメイン管理者認証情報」の状態で、他の鉱脈を全て掘り尽くし、かつ EDR にメモリスキャンがない、または PPL が未開と確認できたときだけ価値がある。そうでなければ、LSASS を一度読むたびに一度のアラートになる。
**Q4**: dump した産物はどう処理する?
**A4**: 対象機で mimikatz を実行しない。minidump を攻撃機へ持ち帰り、オフラインで `sekurlsa::minidump` を解析する。対象機には「ファイルを読む」という動作だけを残す。
**Q5**: hash を取得したら最初に何をする?
**A5**: まず分類する。ローカルアカウント hash(本機のみ+横展開で再利用の可能性)、ドメインアカウント hash(横展開+ドメイン内照会)、マシンアカウント hash(S4U/RBCD/DCSync 条件のひとつ)。種別ごとに次の手が異なる。まとめて全部試さない。
**Q6**: LSASS に何もなく、SYSTEM 自身の token だけの場合は?
**A6**: このマシンには誰も対話ログオンしていないことを示す。「誘導」へ転換する。スケジュールタスク/サービス/管理者ログオンを待つか、coerce(PetitPotam/PrinterBug)で他アカウントを騙して認証させる。認証情報収集で取れないなら、認証情報の流れを作る。

### mimikatz が AV に即殺される場合の回避
**Q1**: `mimikatz.exe` が着地と同時に殺され、connect back すら間に合わない。まずどの層で詰まっているかをどう判断する?
**A1**: 三層の判定基準を分けてテストする。①静的シグネチャ——mimikatz 文字列を含まない loader/コンパイル版に替えてまだ殺されるか。②挙動——`sekurlsa::logonpasswords` という API 呼び出し列自体が hook されているか。③メモリ——AMSI/ETW が遮断していないか。それぞれ最小サンプル(例: `version` だけ)で打ち、どの段階で最初に炸裂するかを見る。
**Q2**: 静的シグネチャだけなら、最小コストの回避は?
**A2**: 優先順位:①BOF/inline .NET 版(SharpKatz、nanodump)で PE 着地を回避。②ソースの文字列微変更+再コンパイル。③加殻/shellcode loader。④白利用のサイドローディング。ひとつ選んだらバフを重ねない。重ねすぎると IoC が増える。
**Q3**: AMSI/ETW に遮断された場合は?
**A3**: 判定基準:PowerShell 版 Invoke-Mimikatz は読み込み即エラーだが exe 版は動く → AMSI。C# 版は Reflection.Load で即死 → CLR AMSI。それぞれ対症する。AMSI patch(メモリで AmsiScanBuffer の返り値を書き換え)、ETW patch(NtTraceEvent)。この patch も EDR の重点監視点であることに注意し、価値があるかを秤にかける。
**Q4**: 全て遮断され、LSASS が読めない。この経路を諦めるべきか?
**A4**: 「その場解析」を諦め、「持ち帰りオフライン」へ転換する。署名済みツール(procdump、comsvcs.dll MiniDump、タスクマネージャ右クリック、silent process exit debugger など)で minidump または lsass メモリ全体を取得し、オフラインで攻撃機の pypykatz/mimikatz で解析する。EDR の procdump に対するアラート等級は通常 mimikatz より低い。
**Q5**: procdump も EDR にブラック判定された。まだ手はある?
**A5**: 「LSASS というプロセスを dump しない」という直交次元へ転換する。LSASS 全体を迂回し、SAM/SECURITY hive、DPAPI オフライン、Kerberoast/AS-REP roast、ADCS 証明書悪用で PKINIT を取る。認証情報は必ずしもメモリから来る必要はない。
**Q6**: このマシンの LSA Protection (RunAsPPL) が有効なら?
**A6**: ユーザーモード dump は全て無駄。レジストリ変更して再起動(動きが大きく、アラートやユーザー気付きを誘発しうる)か、署名済みの脆弱ドライバ(BYOVD)でカーネルから読む。ただしこの経路は IoC が非常に大きく、最後の一歩で、対象が DA か高価値アカウントのときだけ使う。
**Q7**: 時間がなく EDR が強い場合、素早く損切りする戦略は?
**A7**: このマシンでの消費をやめ、SYSVOL/GPP、プリンタ/CSV の古い cpassword、SCCM ネットワークアクセスアカウント、backup サービスアカウントなど「端末に触れずに認証情報を取れる」経路へ転換する。

### SAM/SYSTEM hive でローカル認証情報を回収する
**Q1**: SYSTEM はあるが LSASS が読めない。SAM を狙いたい。最小ノイズで hive を取るには?
**A1**: `reg save HKLM\SAM ...` + `HKLM\SYSTEM ...` + `HKLM\SECURITY ...` の三つが全て必要。SAM はローカルアカウント NTLM、SYSTEM は boot key(SAM 復号用)、SECURITY は LSA secrets(サービスアカウント平文/DPAPI 鍵/マシンアカウント hash など)。SAM だけ dump しない。
**Q2**: 持ち帰った後、ローカルでどう復号する?
**A2**: impacket の `secretsdump.py -sam -system -security LOCAL`(オフラインモード)または pypykatz の `registry`。産物は、ローカルアカウント NTLM、cached domain logons(DCC2、オフラインブルートフォース)、LSA secrets(サービスの平文パスワードの可能性)。
**Q3**: DCC2 hash を取得した。ブルートフォースする価値はあるか?
**A3**: 判定基準:hash 種別(mscash2 は遅い)、パスワード複雑度の事前知識(ドメインポリシー、対象企業の一般的なパスワードスタイル)、時間コスト。DCC2 は一桁遅く、そのアカウントがアクティブユーザー/管理者である可能性が高いときだけ並べてブルートフォースする価値がある。そうでなければ先に LSA secrets のサービスアカウント平文を使ってから考える。
**Q4**: SECURITY hive に LSA secrets がない、または全て空の場合?
**A4**: このマシンにサービスアカウントが設定されておらず、DPAPI 保護のサービスも有効化されていないことを示す。転換する。スケジュールタスク(schtasks /query /xml に `Run As` の認証情報参照がありうる)、Windows Credential Manager(cmdkey /list + vaultcmd)、Chrome/Edge/Firefox パスワード庫(ユーザー DPAPI key が必要)、PuTTY/WinSCP レジストリ。
**Q5**: ローカルアカウント hash しかない。横展開に使えるか?
**A5**: 重要な判定基準は、ローカル Administrator の NTLM が複数マシンで再利用されているか(イメージ展開/LAPS 未有効)。CrackMapExec/nxc の `--local-auth` でこの hash を使って /24 を一段スキャンし、再利用率が高ければ横展開の金鉱。LAPS 有効なら単機で無効となり、ドメインアカウントへ転換する。
**Q6**: reg save が EDR にアラートされる場合は?
**A6**: ボリュームシャドウコピー(vssadmin create shadow → shadow から SAM/SYSTEM ファイルをコピー)へ転換するか、esentutl で占有ファイルを直接コピーする。diskshadow スクリプトモードも使える。三手段は IoC スペクトルが異なるので、順に試す。

### DPAPI 全家:マスターキー、認証情報、Chrome
**Q1**: あるユーザーが保存した Chrome パスワード/RDP 保存認証情報を解きたい。ユーザー SID は既知だ。次は?
**A1**: DPAPI 復号チェーンは、ユーザーパスワード(または NTLM)→ ユーザー master key(`%APPDATA%\Microsoft\Protect\<SID>\` 内)→ blob。したがってまずそのユーザーのログオンパスワード/NTLM を取得できるかを見る。取れなければ domain backup key 経路へ進む。
**Q2**: ユーザーパスワード/NTLM が取れず、domain backup key 経路しかない。前提条件は?
**A2**: ドメイン管理者相当の権限(通常 DA)が要り、ドメインコントローラから `lsadump::backupkeys` でエクスポートする。判定基準:既に DA なら、このユーザーの DPAPI を苦労して解く必要があるか。必要である。なぜなら新しい認証情報面(Azure/クラウド/サードパーティ SaaS の保存パスワード)を得られ、戦果が拡大するからだ。
**Q3**: master key を取得した後、どう使う?
**A3**: `dpapi::masterkey` で平文 master key を解き、`dpapi::chrome` / `dpapi::cred` / `dpapi::rdg` でそれぞれ異なる blob に対処する。Chrome は v80 以降、さらに AES key(local state 内の encrypted_key。同じく DPAPI で保護)を重ねていることに注意。
**Q4**: 対象がオフラインマシン/退職者デスクトップで、ハードディスクイメージしかない?
**A4**: DPAPI は完全にオフライン可能。NTUSER.DAT からパスワードは取れないが、SYSTEM/SECURITY hive からドメインマシンアカウント secret を取り、domain backup key 経路へ進む(ドメインコントローラに触れるか、キャッシュ済みが必要)。またはユーザーパスワードを直接ブルートフォースして master key を解く。
**Q5**: ユーザーがスマートカードログオン/ローカルパスワードなしの場合は?
**A5**: DPAPI ユーザー master key は NTLM 派生キーを使う。PKINIT スマートカードでも、SSO 用に NTLM ハッシュがキャッシュされていることがある。`sekurlsa::pku2u` へ転換するか、そのユーザーの msDS-KeyCredentialLink(Shadow Credentials 攻撃経路)を見る。
**Q6**: DPAPI 経路が完全に通らない場合、認証情報面をさらに広げるには?
**A6**: ブラウザ cookie(パスワード不要で session token を直接使用)、SSH known_hosts + 秘密鍵、KeePass/1Password データベース(マスターパスワードが要るが、keylog やキャッシュから探せる)、企業 IM クライアントのローカルキャッシュ(OAuth token の可能性)へ転換する。

### Kerberoast:どのアカウントを打つ価値があるか
**Q1**: 一般ドメインユーザーだ。Kerberoast をしたい。まず何を探す?
**A1**: SPN を持つユーザーアカウントを探す(マシンアカウントではない。マシンアカウント hash は 128 ビット乱数でブルートフォース不能):`GetUserSPNs.py` または LDAP filter `(&(samAccountType=805306368)(servicePrincipalName=*))`。admincount=1 のもの、命名が svc/sql/backup のものを優先する。
**Q2**: SPN がひとつもない場合は?
**A2**: 「targeted Kerberoast」へ転換する。ある対象ユーザーオブジェクトに GenericWrite/GenericAll があるなら、一時的に SPN を書き、roast し、終わったら削除する。これは権限昇格チェーンの一環であり、無条件ではない。
**Q3**: roast でチケットを取得した。暗号種別が AES256 だ。ブルートフォースできるか?
**A3**: 判定基準:AES は RC4 より一桁遅く、企業パスワードポリシーが弱くないと勝ち目がない。対象アカウントが AES を強制している(msDS-SupportedEncryptionTypes)なら、まず「降格 roast」ができるかを見る。一部の場面ではクライアントが RC4(etype 23)をリクエストできる。できなければ roast 経路を諦める。
**Q4**: 全アカウントが AES でパスワードも複雑、一週間ブルートフォースしても結果なし?
**A4**: 「辞書を回す」という直交次元を諦め、「ブルートフォース不要の認証情報取得」へ転換する。AS-REP roasting(DONT_REQ_PREAUTH アカウントを探す)、ADCS ESC1-8(証明書が身元)、Unconstrained Delegation coerce、RBCD。
**Q5**: 平文パスワードをひとつブルートフォースできたが、低権限サービスアカウントだった。がっかりした。どうする?
**A5**: 焦らない。まず三つを行う。①このパスワードで他アカウントをスプレーする(パスワード再利用/規則)。②このサービスアカウントがローカル管理者や他オブジェクトへの ACL を持つかを見る。③S4U に悪用できるか(SPN がある場合)、またはその保護リソース(SQL/共有)へアクセスできるかを見る。低権限認証情報もてこにできる。
**Q6**: 判定基準:いつ Kerberoast をやめて別へ転換するか?
**A6**: 三つのシグナルのいずれかが出たら止める。①対象ドメインが全 AES でパスワードポリシーが強い(14 桁以上の複雑)。②既に 24 時間以上進展なし。③ドメイン内 SPN アカウントが 10 未満で全てブルートフォース済み。AS-REP、証明書、DCSync 前提条件収集へ転換する。

### AS-REP Roasting の判定基準
**Q1**: AS-REP roast をしたい。有効な認証情報なしでもできるか?
**A1**: 不要。88 番ポートへ接続でき、ユーザー名リストを知っていればよい。`GetNPUsers.py -no-pass` を使う。したがってユーザー名列挙が前提である。まず RID cycling(null session)、SMB 匿名、LDAP 匿名、Kerberoast 偵察 GetUserSPNs の副産物、外部漏えいユーザー名を行う。
**Q2**: 一通り打ったが、全アカウントが事前認証を有効化していて hash が取れない?
**A2**: 現代の AD はデフォルトで事前認証を開く。歴史的遺留/旧アプリ互換でのみオフになる。判定基準:この経路は「低コストの拾い物」であり、取れなければそのまま諦め、時間を増やさない。Kerberoast かパスワードスプレーへ転換する。
**Q3**: AS-REP hash を大量に取得した。どれからブルートフォースする?
**A3**: 順序戦略:①人名らしい命名 > サービスらしい命名(サービスパスワードは通常乱数)。②admincount=1 を優先。③作成時刻が古いアカウント(古いアカウントはパスワードが弱い可能性)。④最近パスワード変更したアカウント(アクティブであることを示す)。
**Q4**: AS-REP がひとつブルートフォースできたが、一般社員らしい。深掘りする価値は?
**A4**: ある。第一歩:そのユーザーがローカル管理者か(グループポリシー/通常展開)、あるワークステーションの「主ユーザー」か(LAPS 前の時代は個人機管理者権限がよくある)。第二歩:BloodHound で DA への outbound path が何ホップかを確認する。
**Q5**: hash 種別が etype 17/18 (AES) で、ブルートフォース速度が極端に遅い。どうする?
**A5**: 判定基準は「高強度暗号+強パスワードはオンラインブルートフォースを基本諦める」という通則と同じ(Kerberoast 節の判定基準を参照)。ここでの追加の直交動作は、AS-REP hash を取ったアカウントを「ユーザープロファイル」(作成時刻/命名習慣/所属 OU)し、ブルートフォース算力を「プロファイル上最も弱いパスワードの可能性が高い」少数アカウントだけに費やす。全テーブルを回さない。
**Q6**: AS-REP も失敗。他に「無認証情報」で始められる手は?
**A6**: 転換する。LLMNR/NBNS/mDNS 毒入れ → Responder で NetNTLMv2 を取得、IPv6 mitm6 + ntlmrelay で LDAPS/ADCS を打つ、Web 側漏えい(Outlook Web Access ユーザー名検証、Autodiscover)。これらは初期認証情報を必要としない。

### 無認証情報で NetNTLMv2 を取得:Responder のタイミング
**Q1**: 同一ネットワークセグメントで NetNTLMv2 を取得したい。まず何を見る?
**A1**: 判定基準の三問:①同一ブロードキャストドメインか(別 VLAN なら諦める)。②SMB 署名強制があるか(強制署名なら relay は無駄だが hash 取得は可能)。③夜か昼か(ユーザーが活発な時間帯は hash が多い)。まず 10 分間パッシブスニッフィングし、LLMNR/NBNS リクエスト量を見てから汚染を開始するか決める。
**Q2**: リクエスト量が少なく、毒入れしても魚がかからない?
**A2**: 能動的に「誘導」する。WPAD 関連リクエスト(ブラウザのプロキシ自動発見)、SMB 空共有パス(Word に UNC を埋め込む、メールに UNC 画像を埋め込む)、Web アプリの SSRF で自分の SMB を叩く。トラフィックを作ってから釣る。
**Q3**: hash を取得したがブルートフォースできない。他に使い道は?
**A3**: relay へ転換する。ntlmrelayx で LDAP(ACL 変更、マシン追加)、ADCS Web Enrollment(証明書取得)、SMB(署名非強制が必要)を打つ。hash 取得は入口に過ぎず、relay が本命である。
**Q4**: 全ネットワークで SMB 強制署名 + EPA + LDAP 署名、relay も無駄?
**A4**: 「署名を下げる」またはプロトコル次元を変える。MS-DFSCOERCE / PetitPotam / PrinterBug でマシンアカウントに逆接続させる(マシンアカウントの逆接続 LDAP は署名制限なし)。または HTTP → LDAPS のクロスプロトコル(署名ネゴシエーションの差異)へ進む。
**Q5**: 判定基準:いつ毒入れを止めるか?
**A5**: ブルーチームが Responder-detection(偽名毒入れリクエスト)を仕掛けている可能性が高い。自分の偽造 hostname リクエストが返ってきたら即座に止める。これはハニーポットのシグナルである。
**Q6**: 取得/毒入れ/relay が全くできない。他に内網無認証情報経路は?
**A6**: 未認証 CVE へ転換する。zerologon(パッチはタイムラインで確認)、PrintNightmare、Exchange ProxyLogon/Shell、SMBGhost、EternalBlue(古いネットワークにはありうる)。認証情報収集が通らないなら脆弱性次元へ進む。

### シルバーチケット vs ゴールデンチケット:場面の選び方
**Q1**: ドメインユーザー hash を取得した。チケット系攻撃をしたい。シルバーかゴールデンか?
**A1**: シルバーチケット = サービスアカウント hash で ST を偽造。そのサービスのみに有効で、ドメイン管理者不要。ゴールデンチケット = krbtgt hash で TGT を偽造。全域を制覇するが、DCSync/ドメイン管理者の前提が必要。判定基準:特定の SQL/CIFS へのアクセスだけでよければシルバーで十分。長期バックドアが要るならゴールデン。
**Q2**: シルバーチケットの隠密性と限界は?
**A2**: 隠密性は高い(KDC を経由せず TGS を出す)。しかしサービスアカウントのパスワードが変われば無効。また PAC 検証が有効なドメイン(2019+)では KDC の二次検証を受けるため、pac 内のユーザーが実在するか、S-1-5-21-...-500 のようなものを使う必要がある。
**Q3**: ゴールデンチケットの krbtgt hash を取得した。即座に打つべきか?
**A3**: 即座に打たない。ゴールデンチケットは「核弾頭級」IoC で、一度使えば検知リスクが急上昇する。まず考える:一度きりで使うか、バックドアを残すか。バックドアなら hash を保存して場面から退出し、活動を残さない。使うなら正常な業務時間・正常なホスト・正常なアカウント名で偽装する。
**Q4**: krbtgt hash が取れない。同様の効果が欲しい場合は?
**A4**: 「ダイヤモンドチケット」/「サファイアチケット」(diamond/sapphire ticket)へ転換する。krbtgt hash が不要で、KDC が返した正当な TGT 内の PAC を改変して権限を偽造する。検知面はゴールデンより小さい。前提は、KDC 通信を遮断または偽装できること + 有効アカウント。
**Q5**: サービスアカウント hash はあるが PAC 検証がシルバーチケットを遮断する?
**A5**: S4U2self + S4U2proxy(アカウントが制約付き委任できる場合)で任意ユーザーとしてそのサービスへアクセスする形へ転換する。またはそのサービスアカウント自身の権限でアクセスする(それで十分な場合もある)。
**Q6**: チケット系が全て検知/失敗した。根本に戻るには?
**A6**: 次元を ACL 悪用へ変える。高権限グループへの GenericAll/WriteDACL/AddMember、Shadow Credentials(msDS-KeyCredentialLink 書き込み)、RBCD(msDS-AllowedToActOnBehalfOfOtherIdentity)。チケットなしでもドメインを取れる。

### DCSync 前提権限の判定基準
**Q1**: DCSync で krbtgt を取りたい。現在のアカウントは十分か?
**A1**: DS-Replication-Get-Changes と DS-Replication-Get-Changes-All の二つの拡張権限が必要。デフォルトでは Domain Admins、Enterprise Admins、Administrators、Domain Controllers、Read-Only Domain Controllers の一部権限が持つ。BloodHound または `dsacls` で現在の身元がこの二権限を継承または明示的に持つかを調べる。
**Q2**: DCSync 権限がない。昇格できるか?
**A2**: 中間の踏み台を探す。①ドメインルートオブジェクトに WriteDACL を持つアカウント(自分に権限を追加できる)。②あるグループに AddMember を持つアカウント(高権限グループへ追加)。③忘れられたサービスアカウントが過去に認可されていたケース。BloodHound の shortest path to DA がこれを探す。
**Q3**: krbtgt hash を取得後、即座に DCSync でさらにアカウントを取るべきか?
**A3**: DC 上で動作を繰り返さない。一度の接続で関心対象をまとめて dump する。krbtgt、Administrator、DA グループメンバー、対象業務アカウント、マシンアカウント(高価値サーバー)。まとめて一度で行い、4662 ログ回数を減らす。
**Q4**: DCSync が SACL 監視されている、または 4662 でアラートされる?
**A4**: 「オフラインで取る」へ転換する。DC ディスクに触れられるなら(vSphere スナップショット、バックアップファイル、shadow copy)、`ntds.dit` + SYSTEM hive を直接 dump し、オフライン secretsdump する。ネットワークトラフィックなし。
**Q5**: DC 全てに EDR + LSA Protection + バックアップ暗号化がある?
**A5**: DC を dump せずにドメイン権を取る方向へ転換する。ADCS ESC 系で証明書を直接偽造して DA になる、Golden SAML(ドメインが ADFS に連携し token 署名鍵を取得した場合)、Azure AD Connect Sync アカウント(hybrid の場合)。認証情報収集にはドメイン外経路がある。
**Q6**: krbtgt を取得したが read-only DC(RODC)だと判明した?
**A6**: RODC の krbtgt は独立しており、RODC がキャッシュしたユーザー TGT しか発行できない。判定基準:RODC キャッシュポリシーがある高価値アカウントを許可していればそのユーザーを偽造する。そうでなければこの hash の価値は限定的で、書き込み可能 DC を探す方向へ転換する。

### ntds.dit オフライン抽出の三経路
**Q1**: 既に DC 上だ。ntds.dit を取りたい。どれが最も静かか?
**A1**: 三択。①`ntdsutil "ac i ntds" "ifm" "create full C:\temp"`(IFM バックアップ作成。公式ツールで IoC が低い)。②ボリュームシャドウコピー vssadmin/diskshadow でコピー。③esentutl でロード中の ntds.dit を直接コピー。いずれも復号には SYSTEM hive が一緒に必要。判定基準:ntdsutil が最もホワイトだがイベント ID が明確。shadow は中程度。esentutl の直接コピーは失敗することがある。
**Q2**: ntdsutil が 4104/イベントログをトリガーする?
**A2**: diskshadow スクリプトモードへ転換する。`create` → `add volume` → `expose` → `copy` を `-tempfolder` の隠しパスと組み合わせる。または WMI リモート shadow で DC 上にシェルを落とさない。
**Q3**: ntds.dit + SYSTEM を持ち帰った。どう復号する?
**A3**: `secretsdump.py -ntds ntds.dit -system SYSTEM LOCAL` で全ドメインアカウントの NTLM + Kerberos key を出力する。膨大なので全てブルートフォースせず、admincount、グループ、命名でフィルタする。
**Q4**: DC 上の管理者権限はないが、あるバックアップシステムへのアクセスだけある?
**A4**: 「バックアップから奪う」へ転換する。Veeam/Commvault/NetBackup のバックアップストレージにはほぼ確実に DC のシステム状態バックアップが直近にある。バックアップ取得 = ntds.dit 取得。これは DC 直結を迂回する古典経路である。
**Q5**: バックアップも取れず、DC にも入れない?
**A5**: 「ntds.dit なしでドメインを取る」経路へ転換する。ADCS 証明書で全域を打つ(ESC1/6/8 が特に致命的)、Coerce+relay で LDAP の ACL を変更、tier-0 サービス(SCCM、Exchange、Azure AD Connect)を探して間接的に DA を取る。
**Q6**: ntds.dit を取得後の長期利用の姿勢は?
**A6**: krbtgt hash を記録してゴールデンチケットを生成する(予備バックドアだが慎重に使用)、全高権限 hash を記録して将来のリプレイに使う、krbtgt パスワード輪換周期に注意する。対象が二度ローテートすると(互換性のため二度必要)、ゴールデンチケットは無効になる。

### マシンアカウント hash の価値掘り起こし
**Q1**: LSASS に本機 `<HOSTNAME>$` の hash を見つけた。何ができる?
**A1**: マシンアカウント hash は本来ブルートフォース不能(128 バイト乱数)だが、①そのマシンのシルバーチケットを作る(自分のサービスへのアクセスを偽造)、②S4U/RBCD 関連攻撃、③PKINIT UnPAC-the-hash 逆照会、④本機サービスへの直接 pass-the-hash 認証ができる。捨てない。
**Q2**: マシンアカウントを他マシンの RBCD に追加できるか?
**A2**: 重要:各ドメインユーザー/マシンはデフォルトで 10 個のマシンアカウントを作成できる(ms-DS-MachineAccountQuota)。DA でなくても、一般ドメイン認証情報を取得すれば新マシンアカウントを作成できる → 対象マシンへ RBCD を設定 → S4U で任意ユーザーとして対象マシンへ到達。古典的な権限昇格チェーン。
**Q3**: MachineAccountQuota=0(ロック)の場合は?
**A3**: 「既存マシンアカウントの乗っ取り」次元へ転換する。あるマシンオブジェクトに GenericWrite/GenericAll がある場合(例: ある保守グループ)、msDS-AllowedToActOnBehalfOfOtherIdentity または msDS-KeyCredentialLink を書き、同等の効果を得る。新アカウント作成は不要。
**Q4**: マシンアカウント hash で横展開できるか?
**A4**: できるが限定的。pass-the-hash で `<HOST>$` として他マシンへ認証しても、通常は Authenticated Users の権限。ただしそのマシンアカウント自身が他マシンのローカル管理者グループにいる場合は別(SCCM/クラスタ場面で稀に存在)。
**Q5**: DC のマシンアカウント hash を取得した場合は?
**A5**: DCSync 権限と同等。DC マシンアカウントは複製権限を持つからだ。したがって DC 上の LSASS または SAM を dump して自分のマシンアカウント hash を取得すれば DCSync できる。「ドメイン管理者」という身元ラベルは不要。
**Q6**: マシンアカウント hash が突然認証失敗する?
**A6**: マシンアカウントはパスワードを自動ローテーションする(デフォルト 30 日)。hash には鮮度期間がある。判定基準:pwdLastSet から 30 日近いなら即座に使う。溜め込まない。または Delegate2Thyself などのテクニックで新しい hash を取る。

### GPP cpassword:古い鉱脈の価値判定
**Q1**: SYSVOL を探して GPP cpassword を探す価値はあるか?
**A1**: ある。ゼロ権限でアクセスできる(ドメインユーザーの SYSVOL 読み取り権限だけでよい)。5 分のコスト:`findstr /S /I cpassword \\<domain>\sysvol\<domain>\policies\*.xml`。ヒットすれば AES キーは公開されているので直接復号できる。
**Q2**: ヒットしない?
**A2**: 古い鉱脈は通常 2014 年パッチ後には掃除されているが、遺留箇所がある。ログオンスクリプト(.bat/.vbs)内のハードコードパスワード、Group Policy Preferences の cpassword 以外の Drives.xml/ScheduledTasks.xml、GPP の Registry.xml、SYSVOL の展開スクリプト、DFS 共有の遺留展開パッケージ。
**Q3**: SYSVOL を掘り終えた。低権限でまだパスワードを探したい?
**A3**: LDAP の `description`/`info` フィールドへ転換する。古い AD では管理者がここにパスワードをメモとして書くことがある。`Get-ADUser -Filter * -Properties description | ? { $_.description -match 'pass|pwd|パスワード' }`。
**Q4**: LDAP 説明にもない?
**A4**: 「共有ファイル単位」へ転換する。ネットワーク共有で匿名/ドメインユーザーが読めるディレクトリから config/xml/ini/txt のパスワードフィールド、または `\\<fs>\backup\` の履歴設定を検索する。SMB 検索ツール:snaffler / manspider。
**Q5**: いつ「ファイル内のパスワード」探しをやめるか?
**A5**: 判定基準:ドメインユーザーがアクセスできる SYSVOL + 最低 3 つの大共有 + 重要サーバーの C$/Admin$(権限があれば)を grep で網羅したのに結果なし → 認証情報源(ブラウザ/端末/サービス)へ転換する。
**Q6**: 平文パスワードをひとつ見つけたが、アカウントは既に退職済み?
**A6**: 捨てない。①パスワード再利用をテストする(現職アカウントが同様の規則を使っていないか)。②このアカウントが無効化されておらず、パスワード期限切れだけかを見る(NoPassExp 属性の可能性)。③この企業のパスワード構成習慣(会社名+年+特殊記号)を記録し、パスワード生成器に与える。

### ブラウザ認証情報の収集
**Q1**: ユーザー機に Chrome がある。保存パスワードを取る前提は?
**A1**: そのユーザーとして実行する必要がある(または DPAPI master key を持つ)。Chrome v80+ の encrypted_key は DPAPI 暗号化のため。SYSTEM でもだめで、ユーザー token を impersonate するか domain backup key 経由へ進む。
**Q2**: ユーザーが未ログオンでディスクだけの場合は?
**A2**: オフライン DPAPI チェーンへ進む。master key + ユーザーパスワード/NTLM または domain backup key が必要。したがってまず「このユーザーの認証情報をどう取るか」を解決し、一周して戻る。
**Q3**: パスワードを取得したが、同期済み Google アカウントのパスワードだと判明した?
**A3**: 高価値。Google アカウントは通常、仕事用メール、企業 SSO(SAML)、クラウドリソースに関連する。企業 Google Workspace 内で GCP/Gmail/Drive へ横展開できるかを見る。認証情報収集の目的は内網だけではない。
**Q4**: Chrome cookie は?
**A4**: より価値が高い。cookie はパスワードなしで直接ログオンでき、MFA を迂回する。対象 cookie:企業 SSO(Okta/Auth0/AzureAD)、クラウドコンソール(AWS/Azure/GCP)、コードリポジトリ(GitHub/GitLab)、内部管理コンソール。同じく DPAPI で encrypted_value を復号する必要がある。
**Q5**: Edge/Firefox は?
**A5**: Edge は Chrome と同じ(DPAPI + AES-GCM)。Firefox は key4.db + logins.json を使い、マスターパスワードが未設定のことがある(デフォルトはマスターパスワードなし)。firefox_decrypt などのツールで直接復号する。
**Q6**: ブラウザに保存認証情報がない?
**A6**: 「他の保存認証情報源」へ転換する。WSL の .git-credentials、~/.aws/credentials、~/.ssh/、%APPDATA% 配下の各種 IM/開発ツール、レジストリ HKCU\Software を辿って password フィールド、Outlook キャッシュメールから "password/アカウント" キーワード検索。

### token impersonation の手法
**Q1**: SYSTEM がある。あるドメイン管理者の token を借りたい。彼がこのマシンにさっきログオンした。どうする?
**A1**: 現在のプロセスの token 所有者を列挙する。`token::list`(mimikatz)、`incognito`(Meterpreter)、`SharpToken`。SeImpersonatePrivilege を持ち、対象ユーザーに属するプロセスを探し、`token::impersonate` または `steal_token <pid>`。
**Q2**: 彼がログオンしたが既にログオフした。token は残っているか?
**A2**: process token 層にはないが、LSASS キャッシュに残っている可能性がある(logon session)。mimikatz の `sekurlsa::logonpasswords` または `sekurlsa::tickets` へ転換し、彼の残留 TGT/hash を取る。token ではなく。
**Q3**: token を取得したがローカルでしか使えない。リモートで使いたい?
**A3**: token はローカルプロセスの属性であり持ち出せない。リモートで使うには:①token に紐づく logon session から TGT を抜き(`sekurlsa::tickets /export`)、pass-the-ticket する。②または LSASS から NTLM/平文を掘り、pass-the-hash/平文で他マシンへ認証する。
**Q4**: SeImpersonatePrivilege がない場合は?
**A4**: 「impersonate を持てるように権限昇格」へ転換する。Potato 系(Rogue/Juicy/Print/Rotten)の本質はネットワークサービスの NTLM auth を反射して自分に戻すことで、前提は SeImpersonate(サービスアカウントによくある)または一部の COM/DCOM トリガー条件。impersonate がなければ先に SYSTEM へ。
**Q5**: token を使った後に revert を忘れると、どんな結果になる?
**A5**: 後続操作がそのユーザーとして監査され、「濡れ衣」になるか、異常挙動アラート(そのユーザーが本来ログオンしないマシンからの動作)を誘発する。使い終わったら即座に `rev2self`。
**Q6**: 対象マシンに高権限ユーザーが誰もログオンしたことがない。token に釣れる魚がいない?
**A6**: 「ログオンを作り出す」へ転換する。管理者ログオンを誘導する(偽チケット/システム障害)、RDP セッションをハイジャックする(query session + tscon。SYSTEM が必要。切断されたセッションを乗っ取り、そのユーザー token 一式を取得)、サービスを乗っ取り高権限アカウントに自分のプロセスを実行させる。

### RDP セッションハイジャック
**Q1**: `query user` に切断された管理者 RDP セッションがある。ハイジャックしたい。どうする?
**A1**: SYSTEM 権限で `tscon <sessionid> /dest:<mysession>`。直接乗っ取り、そのユーザーのパスワードは不要。これは Windows の仕様であり脆弱性ではないが、極めて致命的。判定基準:SYSTEM でなければならない(管理者ではない)。したがって先に SYSTEM へ権限昇格する。
**Q2**: 気付かれるか?
**A2**: 気付かれる。元セッションのユーザーが再接続すれば異常に気付き、ログに 4779/4778(session reconnected)が残る。使う前に自分の session を `logoff` し、その後ハイジャックする。できるだけ退勤後に行う。
**Q3**: アクティブ(active)セッションしかなく、切断されたものがない?
**A3**: 「切断を待つ」へ転換する。`query session` を監視するか、切断を作り出す(RDP disconnect の専用 API、またはそのユーザーのネットワークを強制切断)。アクティブセッションもハイジャックできるが、元ユーザーを即座に蹴り出すため即座に発覚する。
**Q4**: 対象マシンが複数セッションを許可しない、または tscon がグループポリシーで無効?
**A4**: tscon 不要の方式へ転換する。そのユーザーがログオン済みという事実を使い、直接 token を impersonate する(前述のチェーン)、mimikatz で LSASS から彼の認証情報を抜く、彼の explorer.exe へプロセス注入して身元を取る。
**Q5**: ハイジャック後、彼のデスクトップにいる状態で彼が再ログオンしてきたら?
**A5**: 事前に準備する。explorer へプロセス注入後、即座にハイジャックセッションを抜け、身元 impersonation または認証情報を background に保持する。または即座に隠密な永続化(スケジュールタスク/サービス)を仕込み、退出後も戻れるようにする。
**Q6**: RDP ユーザーがまったくいない?
**A6**: pipe hijack(名前付きパイプのなりすまし)、smb impersonation、またはフィッシング経路で高権限ユーザーに自発的に対象機へログオンさせる(ソーシャルエンジニアリング/偽の運用ニーズ)へ転換する。

### coerce 強制逆接続:PetitPotam ファミリー
**Q1**: 一般ドメイン認証情報を取得した。あるマシン/DC に自分へ能動認証させたい。coerce 手法をどう選ぶ?
**A1**: ファミリー:PetitPotam (MS-EFSR)、PrinterBug (MS-RPRN)、DFSCoerce (MS-DFSNM)、ShadowCoerce (MS-FSRVP)。判定基準:対象のパッチ状態が異なるため、順に試し、失敗したら次へ。DFSCoerce はパッチ面が小さく、優先する。
**Q2**: coerce 成功、NTLM 認証を受信した。何に使う?
**A2**: 三方向。①LDAP/LDAPS へ relay(ACL 変更、自分に DCSync 追加)。②ADCS Web Enrollment へ relay(マシン証明書を取得し、UnPAC hash)。③SMB へ relay(署名非強制の場合)。ADCS relay(ESC8)が主役である。
**Q3**: 対象が DC で、パッチ+署名強制がある?
**A3**: パッチは一穴を塞ぐが、他はまだ開いている可能性がある(パッチ体系は不完全)。API を個別にテストする。署名強制は SMB relay を塞ぐが LDAPS relay は塞がない(EPA が要る)。ADCS Web はデフォルトで EPA を有効化しないため、ESC8 の古典的シンクである。
**Q4**: ADCS 環境がない?
**A4**: LDAP/LDAPS relay + 対象マシンへの msDS-AllowedToActOnBehalfOfOtherIdentity 追加(RBCD 攻撃チェーン)へ転換し、その後 S4U で対象マシンの admin チケットを取る。
**Q5**: coerce API が全て塞がれ、MS-EFSRPC/MS-RPRN が access denied を返す?
**A5**: 「WebDAV coerce」へ転換する(WebClient サービスが開いていれば、マシンに HTTP 認証させられる。HTTP relay から LDAPS は署名制限を受けない)。または coerce を諦めて認証情報窃取経路へ転換する。
**Q6**: 判定基準:いつ relay を諦めて他へ転換するか?
**A6**: 3+ の coerce API × 3+ の relay 先を全て試して失敗 → 対象環境の防御が完全であることを示す。脆弱性次元(ADCS ESC1/ESC7/CVE)、またはソーシャルエンジニアリング/物理へ転換する。

### Kerberos 委任悪用の選択ツリー
**Q1**: BloodHound があるアカウントに unconstrained delegation があると表示した。どう使う?
**A1**: ドメイン管理者をそのマシンへ認証させ、LSASS に DA の TGT を持たせる。coerce で DC を認証させ(PrinterBug から DC へ)、制御下の unconstrained マシン上で sekurlsa::tickets から DC$ TGT を取得 → DCSync。
**Q2**: constrained delegation (with protocol transition) だけの場合は?
**A2**: S4U2self + S4U2proxy の組み合わせ。まず自分に「任意ユーザー」からそのアカウント自身へのチケットを署名し、それで S4U2proxy により委任先サービスへ行く。判定基準:許可サービスリストは msDS-AllowedToDelegateTo にある。
**Q3**: 対象アカウントが Protected Users グループメンバーで S4U が失敗する?
**A3**: Protected Users は委任、NTLM、DES/RC4 を禁止する。S4U の対象がこの種のアカウントならそのまま無効。転換:そのユーザーへ委任できるオブジェクトを逆に探すか、委任をやめて別(ADCS)へ進む。
**Q4**: RBCD の場面で前提は何か?
**A4**: ①対象マシンへの GenericAll/GenericWrite または msDS-AllowedToActOnBehalfOfOtherIdentity への WriteProperty が必要。②作成できる SPN アカウントがある(自分でマシンアカウントを作るか、制御済みアカウント)。前提が欠ければ前提を補う(ACL 経路を探す)。
**Q5**: 委任が全て無効、または Authentication Silo で隔離されている?
**A5**: Authentication Policy Silo は tier-0 アカウントを tier-0 マシンからのみログオン可能に制限し、委任も制限される。ADCS(証明書身元は Kerberos 委任制限を通らない)へ転換するか、tier-0 ホストを取ってから考える。
**Q6**: 委任で取ったチケットがあるサービスにしか有効でない。拡大したい?
**A6**: 委任で CIFS/HOST チケットを取ればそのマシンへの完全アクセスと同等 → そのマシンから LSASS/認証情報を取る → 横展開を続ける。チケットは入口であり終点ではない。

### Shadow Credentials (msDS-KeyCredentialLink)
**Q1**: ある対象アカウントに GenericWrite がある。パスワード変更以外に何ができる?
**A1**: パスワード変更はアカウント機能を壊す(ユーザーが気付く)。Shadow Credentials へ転換する。msDS-KeyCredentialLink に自分の公開鍵を書き → PKINIT で秘密鍵からそのアカウントの TGT + NT hash を交換する(UnPAC)。ゼロ破壊、極めて隠密。
**Q2**: 前提条件は?
**A2**: ドメインに KDC が PKINIT をサポートしている(ほぼ常にある)+ AD Schema が 2016 以上(WHfBTools 場面)。ツールは pyWhisker / Whisker / ShadowSpray。
**Q3**: 打ち終わって痕跡を消したい?
**A3**: msDS-KeyCredentialLink から自分の KeyCredential を削除し、属性を復元する。デフォルトのアカウントはひとつか複数の可能性があるため(TPM/生体認証)、元のものを誤って消さない。
**Q4**: 対象アカウントの msDS-KeyCredentialLink 属性書き込みが拒否される?
**A4**: schema 非対応または KRBTGT 関連保護の可能性。古典的 RBCD、targeted Kerberoast(パスワード変更/SPN 追加)、または直接パスワード変更(破壊的だが有効)へ転換する。
**Q5**: 全域ユーザーが自分自身に msDS-KeyCredentialLink を書ける?
**A5**: その通り。ShadowSpray の場面:ある misconfig で "authenticated users" や "self" がその属性に書き込み権限を持つなら、任意のドメインユーザーが自分にバックドアを仕込める。環境が弱設定かどうかを一括スプレーで確認する。
**Q6**: 打ち終わって UnPAC で NT hash を取得した後は?
**A6**: 標準の hash 後続 → PtH またはもう一周。高権限アカウントなら DCSync に持っていく。サービスアカウントならそのサービスへアクセス。一般ユーザーなら BloodHound の outbound path。

### パスワードスプレーのテンポと判定基準
**Q1**: ユーザー名リストを手にスプレーしたい。まず何を見る?
**A1**: 三つ。①ドメインのロックアウトポリシー(`net accounts` または rpcclient/samr で lockout threshold/duration/observation window を調べる)。②既にロックされているアカウント(突き続けるのを避ける)。③ドメインの業務時間(勤務時間にスプレーし、ログオン失敗を正常ノイズに混ぜる)。
**Q2**: パスワードをどう選ぶ?
**A2**: 三階層。①会社/ブランド関連(組織略称+年/数字サフィックス、略称+特殊記号)。②季節/四半期関連(季節/四半期+当年+記号)。③業界共通の弱いパスワードパターン(一般語+数字/記号。公開辞書由来)。毎ラウンドひとつ変え、周期を observation window より長くする。具体的な値は対象の OSINT 結果で生成し、事前に固定しない。
**Q3**: 一ラウンドスプレーして全滅。続ける?
**A3**: 判定基準:パスワードポリシー lockout=5、observation=30min なら、一日最大 4〜5 ラウンド × ユーザー数。3 ラウンド無効 → パスワードリスト戦略が誤り(企業文化が常識からずれている)、またはユーザー名リストが汚染されている → 止めてインテリジェンスを集め直す。
**Q4**: あるアカウントだけ突然全試行がロックされ、他は正常?
**A4**: ハニーポットアカウントのシグナル(honey user)。このアカウントはブルーチームが意図的に残した囮で、どんな認証もアラートされる。即座にリストから外し、追跡されていないか振り返る。
**Q5**: 認証情報をひとつスプレーできたが低権限ユーザーだ。スプレーを続けるか深掘りするか?
**A5**: 深掘りを優先する。この認証情報で LDAP 完全列挙、BloodHound 収集、shares 探索、Kerberoast/AS-REP roast(今は認証情報がある)を行う。スプレーは入口であり終点ではない。
**Q6**: パスワードポリシー強 + lockout 厳格 + ハニーポット、全くスプレーできない?
**A6**: 「無認証攻撃」へ転換する。coerce、責任者プロトコル詐称、未認証 CVE、ADCS ESC8(HTTP relay)。または少量の既知情報で targeted phishing を仕掛けて本物の認証情報と交換する。

### LSA secrets の宝
**Q1**: SECURITY hive の LSA secrets から具体的に何を掘れる?
**A1**: サービスアカウント平文(サービスがドメインアカウント run-as の場合)、DPAPI SYSTEM key、DefaultPassword(自動ログオン時にここに書かれる)、マシンアカウントキャッシュパスワード(NL$KM などの接頭辞)。impacket secretsdump が自動分類出力する。
**Q2**: `_SC_サービス名` で始まるものを見た。価値は?
**A2**: 極めて価値が高い。「サービス実行身元」のパスワード平文である。命名に svc/backup/sql を含むものは全て注目。ドメイン内サービスアカウントである可能性が高く、多くのマシンでローカル管理者権限を持つことが多い。
**Q3**: `NL$KM` がバイナリだけ?
**A3**: それは domain cached credentials の暗号化キーで、`$MACHINE.ACC` と併用する。secretsdump が cached DCC2 hash を自動整理出力してオフラインブルートフォースに使う。
**Q4**: LSA secret がひとつもない?
**A4**: このマシンに「ドメイン身元で実行する」サービスが何も設定されていないことを示す。純粋なクライアント機である。ユーザープロファイル層の認証情報(ブラウザ/Credential Manager/RDP 保存)、またはそのマシンのファイルシステム内の設定ファイルへ転換する。
**Q5**: サービスアカウント平文を取得した。直接横展開?
**A5**: まず列挙する。そのアカウントのドメイン内 memberOf、servicePrincipalName、AdminCount。重要権限保持者かを見る。その後、ローカル管理者の再利用面をテストする(重要サーバーへスプレー)。
**Q6**: サービスアカウントが無効化されている。平文にまだ価値は?
**A6**: ある。「無効だが使える」かをテスト(古い設定)、履歴パスワードが他アカウントに踏襲されていないかをテスト、パスワードパターンサンプルとして辞書生成器に与える。

### Windows Credential Manager の低い垂れ下がった果実
**Q1**: 一般ユーザーセッションで、保存認証情報が素早く見える方法は?
**A1**: `cmdkey /list` で Windows Vault + Credential Manager 項目を一覧し、`vaultcmd /list` で web/generic 類を見る。target が内部サーバー名/メールドメインのものは全て注目に値する。
**Q2**: 項目はあるが平文が取れない。どうする?
**A2**: mimikatz の `vault::cred /patch` + `dpapi::cred` へ転換する。そのユーザーの DPAPI master key が必要(ログオンセッション内で直接復号、オフラインなら前述の DPAPI チェーン)。
**Q3**: RDP 保存認証情報(TERMSRV/...)がある?
**A3**: 直接 mimikatz の `dpapi::rdg` を使うか、そのユーザーセッション下で mstsc から保存認証情報でログオンする。復号不要 → 直接対象機へ横展開(その対象機は通常、彼がよく使うサーバーで、高確率で高権限)。
**Q4**: cmdkey list が完全に空?
**A4**: ブラウザ/他アプリのパスワードストレージへ転換する。VS Code(Git credentials)、SSMS(SQL server 接続)、Outlook(mapi profile)、企業 VPN クライアント(通常 %APPDATA% に保存)。
**Q5**: 項目はあるが全て web 認証情報でネットワーク認証情報ではない。使えるか?
**A5**: 有用。これらは通常、内部 SharePoint、Confluence、Jira、GitLab のアカウントパスワード。取得後、内部コード/ドキュメントプラットフォームへ横展開し、さらに認証情報や機微なデータ(ソースコード内のハードコードキー)を掘れる可能性がある。
**Q6**: ログオン済み全ユーザーの Vault を一括取得したい?
**A6**: SYSTEM + 各 logon session の token を逐次 impersonate して Vault を復号する(ユーザー紐付け)必要がある。または夜まで待ち、どれだけの異なるユーザーがログオンしたかを確認して一括取得する。

### SCCM/MECM ネットワークアクセスアカウント
**Q1**: 対象に SCCM(MECM)クライアント(`ccmexec.exe`)があるのを発見した。何を意味する?
**A1**: 巨大な宝。SCCM には高価値認証情報が複数ある。Network Access Account(NAA)、Task Sequence 認証情報、Client Push Installation Account、Site System Installation Account。NAA は特に一般的で、WMI に存在し抽出できる。
**Q2**: NAA をどう取る?
**A2**: CCM WMI クラスから暗号化 NAA blob を読み、本機 DPAPI で復号する。ツール:SharpSCCM、cmloot。SCCM クライアントが入ったマシン上で実行する必要があり、DPAPI は本機 SYSTEM に依存する。
**Q3**: NAA を取得した後は?
**A3**: NAA は通常、クライアントが DP(配布ポイント)から展開パッケージを取得するためのドメインアカウントで、権限は限定的。しかし多くのマシンに存在する可能性が高く、横展開面が広いアカウント。また権限設定を誤っていればローカル管理者/ドメイン管理者の可能性もある。
**Q4**: SCCM サーバー自体にもっと遊べるものがある?
**A4**: ある。SCCM Site Server データベース(MSSQL)に暗号化 client push アカウント、policy 内の機微データがある。SQL 認証またはドメイン権限でアクセスし、SCCMHunter/misconfig-manager と併用する。
**Q5**: 対象環境に SCCM がない。似たものは?
**A5**: MDT(展開共有の CustomSettings.ini/Bootstrap.ini に認証情報)、Intune、Ansible Tower / AWX、Chef/Puppet、Jenkins 認証情報マネージャ、Vault(misconfig の場合)、GPP。企業の運用インフラはすべて認証情報の集積地である。
**Q6**: SCCM クライアントはあるが、WMI/DPAPI で NAA が取れない?
**A6**: NAA が設定されていない可能性(現代の SCCM は Enhanced HTTP + クライアント証明書で NAA を代替することを推奨)。SCCM クライアント証明書自体を取るか、SCCM Web エンドポイントへ relay する方向へ転換する。

### AdminSDHolder と sIDHistory
**Q1**: ドメインを長期バックドアしたい。ゴールデンチケット以外の方式は?
**A1**: いくつか。①AdminSDHolder に ACE を追加(SDProp が 60 分ごとに全 protected group へ逆伝播し、削除が難しい)。②sIDHistory にドメイン管理者 SID を埋める(DA が一度必要。その後そのアカウントは恒久的に elevated)。③KRBTGT パスワードを変更(ゴールデンチケットの範囲を制限)しつつ backup アカウントを植える。
**Q2**: sIDHistory は具体的にどうやる?
**A2**: 既に DA のとき、ある低権限アカウントの sIDHistory 属性に DA グループ SID を書く(DSInternals/mimikatz の `misc::addsid`)。その後そのアカウントは DA グループにいなくても、Kerberos ticket に DA SID を持つ。
**Q3**: 検知されるか?
**A3**: 現代の EDR/AD audit は sIDHistory 変更イベント(4738)を持つ。判定基準:極めて隠密な長期バックドアが要るならリスクが高い。短期利用ならよく、残さない。または正当な信頼ドメイン場面の SIDHistory を利用し、正常に紛れる。
**Q4**: sIDHistory を権限昇格(永続化ではなく)に使えるか?
**A4**: 自分や他者の sIDHistory を書くには DA 権限が要る → 既に DA ならそれで権限昇格する必要はない。sIDHistory は通常永続化用である。
**Q5**: 対象ドメインが SID Filtering(信頼ドメイン間)を有効化している?
**A5**: クロスドメイン sIDHistory 攻撃(古典的 golden ticket + external trust)は SID filter quarantine に遮られる。forest trust attack へ転換する(同一 forest の別ドメインなら SID filter は弱い)。
**Q6**: 長期バックドアはセキュリティ評価で掘り出されるか?
**A6**: 高確率で掘り出される。現代の AD ヘルスチェックツール(PingCastle/BloodHound Enterprise)は AdminSDHolder ACL 異常、sIDHistory 異常をスキャンする。判定基準:バックドアを残すなら清算リスクを耐えられるか、「合法な遺留設定」に十分見せかけられるか。

### ボリュームシャドウコピー(VSS)の多用途
**Q1**: ロックされた SAM/ntds.dit を取ろうとしたが直接コピーが失敗。VSS をどう使う?
**A1**: `vssadmin create shadow /for=C:` で作成 → shadow copy パス(GLOBALROOT\Device\HarddiskVolumeShadowCopy1)を記録 → この読み取り専用スナップショットからファイルをコピー。排他ロックを迂回する。
**Q2**: vssadmin は管理者が要る。SYSTEM でできるか?
**A2**: SYSTEM は shadow を作成できるが、8193/8194 イベントログが残る。判定基準:対象に既に別の shadow があるなら(バックアップタスクの遺留)、既存 shadow を再利用して新イベント作成を避ける。
**Q3**: 隠密にしたい。代替は?
**A3**: `diskshadow` スクリプトモード(対話ツールだが自動化可能)、wbadmin(一部の場面で ntds をバックアップファイルへ dump できる)、PowerShell WMI Win32_ShadowCopy.Create。ノイズ最小のものを選ぶ。
**Q4**: 打ち終わって shadow を消したい?
**A4**: `vssadmin delete shadows /shadow=<id>`。注意:shadow 削除もランサムウェアの常套動作であり、EDR のランサムウェア保護ルールを誘発しうる。判定基準:shadow 削除がアラートを誘発するなら、消さずにシステムの正常スナップショットとして放置する方がよい。
**Q5**: VSS サービスが無効?
**A5**: ハードディスクセクタを直接読む(ドライバか特殊ツール:libguestfs/RawCopy)へ転換するか、マシンを既存バックアップシステムに加入させてバックアップ産物を見る。
**Q6**: shadow 内のファイルタイムスタンプで露見しないか?
**A6**: shadow スナップショットは独立した時点を持つ。shadow 内ファイルへのアクセスは主ボリュームのタイムスタンプを変更しない。ただし shadow 作成自体の時刻は見える。判定基準:業務スナップショット時刻付近に作成すれば混淆できる。

### プリンタ認証情報の宝庫
**Q1**: ネットワークに複数のプリンタを発見(ネットワークスキャンで 631/9100/515)。見る価値は?
**A1**: 極めて価値が高い。複合機(MFP)はしばしば LDAP 統合(スキャンからメール/フォルダ)を設定し、ドメインアカウントの平文パスワードを保存する。管理画面は通常 admin/admin またはデフォルト。
**Q2**: 具体的にどう取る?
**A2**: Web 管理画面にログイン → "Scan to Email" / "LDAP settings" / "Address Book" を探す。パスワードフィールドがある(平文表示のもの、アスタリスクだがフォーム内で見えるもの、firmware 固有の脆弱性で設定ファイルを dump する必要があるもの)。
**Q3**: プリンタ保存アカウントの権限は?
**A3**: 通常「スキャンから共有」アカウントで権限は限定的。しかしドメインアカウントであることが多く、あまりローテーションされず、複数のプリンタに設定されている(パスワード再利用面が広い)。初期入口として極めて良い。
**Q4**: プリンタ管理画面が認証必須で入れない?
**A4**: 三経路。①デフォルト認証情報の大辞書(各ベンダー公開の工場出荷時アカウント/パスワード)。②ベンダー脆弱性(SNMP OID 設定読み取り、未認証インターフェース dump)。③物理接触(プリンタ背面のシールにデフォルトアカウントがよくある)。
**Q5**: SNMP プリンタ community=public も宝か?
**A5**: 宝。多くのプリンタ SNMP が LDAP 設定とパスワードを直接露出する。`snmpwalk -c public <ip>` で OID を網羅する。
**Q6**: 全プリンタが独立管理 VLAN にあり、入れない?
**A6**: 他の IoT 系デバイスへ転換する。VoIP 電話(LDAP 設定が多い)、ビデオ会議(Polycom/Cisco)、入退室システム、UPS 管理カード、iLO/iDRAC/IPMI(いずれもドメイン統合認証情報の可能性)。

### Azure/Entra ハイブリッド環境の認証情報面
**Q1**: 対象ドメインに Azure AD Connect(hybrid identity)がある。追加の認証情報面はどこ?
**A1**: Azure AD Connect サーバー自体が tier-0 対象である。その上で動く MSOL_ アカウントは AD に DCSync 権限を持つ。またサーバーに ADSync データベース(LocalDB)があり、hybrid パスワードを復号する鍵が入っている。
**Q2**: 具体的にどう収集する?
**A2**: AAD Connect サーバー上で(SYSTEM が必要)、AADInternals または手動で LocalDB の keyset を dump し、DPAPI と併用して MSOL_ アカウント平文 + Azure Service Account (Sync_) 認証情報を復号する。
**Q3**: MSOL_ の使い道は?
**A3**: ドメイン内 DCSync 権限。DA 取得と同等。また多くの環境が AAD Connect サーバーを tier-0 保護に含め忘れており、大きな穴である。
**Q4**: Sync_@<tenant>.onmicrosoft.com は?
**A4**: クラウド側権限。通常 Directory Synchronization Accounts ロールを持ち、ユーザーパスワード変更や admin 追加ができる。on-prem からクラウドへ攻める古典的踏み台。
**Q5**: 対象が pass-through auth または federated で、password hash sync を使っていない?
**A5**: PTA agent サーバー(credential harvesting 攻撃で認証を遮断できる)へ転換する。または ADFS サーバーの token signing certificate(Golden SAML)。
**Q6**: on-prem ユーザー hash から直接クラウドを攻めたい?
**A6**: PHS 場面では on-prem hash で直接クラウドへログオンできない(hash of hash)。ただし hash を取得後オフラインブルートフォース → 平文でクラウドログオン。または Seamless SSO の AZUREADSSOACC$ マシンアカウント hash でシルバーチケット → クラウド SSO。

### MSSQL リンクサーバーと xp_cmdshell で認証情報を取る
**Q1**: 内網に MSSQL を発見し、あるドメインアカウントで接続できる。認証情報面をどう掘る?
**A1**: 三手。①`SELECT SYSTEM_USER, IS_SRVROLEMEMBER('sysadmin')` で権限を見る。②`EXEC sp_linkedservers` で他の SQL への linked server があるかを見る(一般的な misconfig:linked が sa または高権限アカウントを使用しており、飛び移れる)。③sysadmin なら `xp_cmdshell` で直接コマンド実行して LSASS を取る。
**Q2**: sysadmin でなく public だけ。まだ掘れる?
**A2**: 転換する。①UNC パス逆接続 — `xp_dirtree \\<my>\a` で MSSQL サービスアカウントの NetNTLMv2 逆接続を強制し、hash を取る。②TRUSTWORTHY + db_owner で sysadmin へ権限昇格(古典的 misconfig)。③他ログインを impersonate(`EXECUTE AS LOGIN='sa'` が認可されている場合)。
**Q3**: MSSQL が動くアカウントは何か?
**A3**: デフォルトは NT Service\MSSQLSERVER(ホスト上では SYSTEM 相当)またはドメインアカウント(例: svc_sql)。ドメインアカウント + 複数 SQL で再利用 = 複数マシンへ横展開。SYSTEM かつサーバーに unconstrained delegation があれば = 天与の踏み台。
**Q4**: linked server が別 SQL へリンクする際の身元は?
**A4**: sysservers の self-mapping を見る。"current context" なら現在のユーザー。固定ログイン(一般的な sa/リモート管理者)が設定されていれば遠隔権限取得と同等。`SELECT * FROM OPENQUERY(<linked>, 'SELECT SYSTEM_USER, IS_SRVROLEMEMBER(''sysadmin'')')` を試す。
**Q5**: xp_cmdshell が無効。他にコマンド実行経路は?
**A5**: `sp_configure` で開く(sysadmin が必要)、CLR ストアドプロシージャで .NET assembly を読み込む、Ole Automation Procedures、代理ジョブ(cmdexec 型の SQL Agent job)。複数経路がある。
**Q6**: MSSQL が完全に打てない?
**A6**: 他のデータベースサービス面へ転換する。Oracle(デフォルト dbsnmp/system 弱いパスワード)、MySQL(root パスワードなし/低権限ファイル読み取り)、PostgreSQL(信頼認証 misconfig)。それぞれ認証情報/コマンド実行経路がある。またはアプリ設定ファイル(web.config の接続文字列)へ転換する。

### メールサーバーの認証情報の穴
**Q1**: 対象環境に Exchange がある。認証情報収集面はどこ?
**A1**: 多層。①Exchange サーバー自体は tier-0 前時代にはよく無視され、その上の LSASS に大量の対話ログオンがある。②Exchange サービスアカウント EWS/Autodiscover 認証を relay できる。③メールボックス内容自体(パスワード/アカウントなどのキーワード検索)。④OAB に古いパスワードファイルがある可能性。
**Q2**: あるユーザーのメールボックスアクセス権を取得した。素早く認証情報を掘るには?
**A2**: 検索ルール:`password / pwd / パスワード / パスワード / vpn / アカウント / credentials / .kdbx / .rdp`、および添付の zip/rar/xlsx 内のアカウント帳。IT 運用/HR メールボックスは特に豊富。
**Q3**: EWS/OWA/Graph で一般ユーザー権限しかない。拡大できるか?
**A3**: ApplicationImpersonation RBAC ロール(対象アカウントがあれば、ワンクリックで全社メールボックスを打てる)、mailbox delegate 権限(あるアシスタントが CEO へ delegate)、Autodiscover のユーザーリスト漏えいをスプレーに使う。
**Q4**: Exchange に利用可能な歴史 CVE がある(ProxyLogon/ProxyShell/ProxyNotShell)?
**A4**: 判定基準:まず外部アセットが到達可能か + Exchange バージョン build 番号 + パッチ時刻を見る。利用できなければ、Exchange サービスアカウント relay(HTTP 認証を LDAPS へ push)へ転換する。
**Q5**: Exchange はないが他メールシステム(Zimbra/Mailu/テンセント企業メール/飛書メール)がある?
**A5**: 対応する穴と認証情報ポイントへ転換する。Zimbra には大量の歴史 CVE。クラウドメールは API token(内部管理コンソールが impersonation token を発行できる可能性)。認証情報収集の通則は「人の認証情報はメールボックスに一度は現れる」である。
**Q6**: メールボックスにアクセスできない。SMTP relay で釣れるか?
**A6**: 「内部フィッシングメールを送る」へ転換する。内部 SMTP はしばしば無認証送信を許可する。IT 部門を装ったパスワードリセットメールを送り、リンクを自分の fake OWA/SSO へ向けて平文を採る。認証情報収集にはソーシャルエンジニアリング側の経路がある。

### Kerberos AS-REQ ユーザー名列挙 + 暗号降格
**Q1**: 有効なユーザー名を列挙したい(後続のスプレー/AS-REP roast 用)。最速は?
**A1**: Kerberos AS-REQ ユーザー名列挙。事前認証なしの AS-REQ を送ると、KDC は存在/不存在/事前認証必要のユーザーごとに異なるエラーコードを返す(KDC_ERR_C_PRINCIPAL_UNKNOWN vs KDC_ERR_PREAUTH_REQUIRED)。ツールは kerbrute。速く、ロックを発生させない。
**Q2**: ユーザー名はどこから?
**A2**: OSINT の会社メール形式(LinkedIn/公式サイト)、内網 LDAP 匿名または既知認証情報での列挙、SMB null session RID cycling、GPO 関連ファイル内のユーザー名、メール署名。
**Q3**: ユーザー名を列挙した後、スプレー以外にできること?
**A3**: ①AS-REP roast で事前認証なしのもの。②timeroast(Windows 固有。古い KDC は一部 principal のチケットを unauth リクエストで存在性探索できる)。③精確な targeted phishing(実在する社員番号がある)。
**Q4**: 列挙後、全アカウントの msDS-SupportedEncryptionTypes が AES のみ許可で、ブルートフォースコストが高い。どうする?
**A4**: 判定基準:暗号ポリシーが統一的に締められている(2019+/セキュリティベースライン)なら、意図的な強化環境を示す → 「AS-REQ 側の最適化」という直交次元を諦め、ユーザー名インテリジェンスを「非ブルートフォース経路」(証明書系攻撃、委任悪用、ACL 権限昇格、精確なフィッシング)へ転換する。降格は別所で議論済みなので繰り返さない。
**Q5**: ユーザー名リストを取得したら、すぐ「低コストの試行錯誤の組み合わせ」をしたい?
**A5**: 組み合わせ行列:列挙したユーザー名 × (事前認証なし → AS-REP roast) × (極低ラウンドのスプレー → admin メールプレフィックス式の弱いパスワード) × (kerbrute password auth でロックしない境界で生存判断)。各手順を lockout ポリシーに合わせ、列挙の成果で逆にアカウントをロックしない。
**Q6**: ネットワーク上の Kerberos トラフィックを取得してオフライン分析したい?
**A6**: MITM(ARP spoof/DHCPv6 mitm6)で 88 番ポートのトラフィックを遮断するか、pcap から既に取得済み AS-REP 内の暗号化 timestamp をブルートフォースする(そのアカウントが事前認証なしの誤設定または弱いパスワードの場合)。この経路は ROI が低く、通常諦める。

### FIDO2/Passkey/WHfB 時代の対応
**Q1**: 対象ユーザーが Windows Hello for Business(PIN + TPM)でログオンする。LSASS に NTLM がない。どうする?
**A1**: WHfB は証明書+TPM で、ユーザー NTLM はメモリにない(SSO が必要な場合を除く)。ただし TGT はある。sekurlsa::tickets へ転換して TGT を取り、pass-the-ticket する。またはユーザー msDS-KeyCredentialLink の証明書は自分の公開鍵であり、鍵を直接逆に取ることはできない(TPM 内)。
**Q2**: そのユーザーの NT hash を取りたい(Kerberos 非対応サービスへの PtH 用)?
**A2**: UnPAC-the-hash。WHfB 証明書で PKINIT して TGT を取ると、TGT の PAC 内 PAC_CREDENTIAL_INFO に NTLM が含まれる。前提:その証明書を自分が持つ必要がある(通常ない。CA を制御していない限り)。または kekeo の関連モジュールを使う。
**Q3**: ユーザーが純粋な FIDO2 ハードウェアキーでログオンし、TPM すらない。付け入る隙は?
**A3**: 「ユーザーログオン後の downstream」へ転換する。TGT がある、session cookie がある、DPAPI master key がメモリで解錠済み → メモリ/プロセスの角度から、やはりユーザー身元の操作能力を収集できる。「パスワード自体」は取れないだけ。
**Q4**: アプリが Kerberos + PKINIT のみ認識し、NTLM を認識しない?
**A4**: 現代の趨勢(Disable NTLM の展開開始)。判定基準:この現実を受け入れ、Kerberos 手法(PtT、S4U、certificate abuse)を主軸にする。NTLM 関連技術(relay/PtH)の将来性は減退する。
**Q5**: passkey/SSO 環境で、他に低い垂れ下がった果実は?
**A5**: session token / refresh token(ブラウザ cookie、Windows PRT/Primary Refresh Token)、Azure AD Broker Plugin cookies(Windows 上で PRT を取れば、クラウドの全 SSO アプリへパスワードなしでログオンできる)へ転換する。
**Q6**: PRT をどう取る?
**A6**: `dsregcmd /status` で AzureAD 加入かを見る。ROADtools/AADInternals/dirkjanm の pipe 関連テクニックで、SYSTEM 権限で LSASS から PRT + session key を抜き、暗号化して持ち出してクラウドへログオンする。これが「クラウド認証情報収集」の核心である。

### バックアップシステム:忘れられた認証情報の金鉱
**Q1**: 対象環境に Veeam/CommVault/NetBackup/Rubrik のいずれかがある。優先して打つ価値は?
**A1**: 極めて価値が高い。バックアップシステムは本質的に「全アセットへアクセスできる」権限を必要とし、通常 DA またはドメインバックアップアカウント hash を保持する。また EDR から除外されていることが多い(バックアップ性能への影響を避けるため)。横展開と認証情報収集の両面の鉱脈である。
**Q2**: Veeam は具体的にどう収集する?
**A2**: Veeam Backup サーバーには SQL/PostgreSQL バックエンドが認証情報を保存し、暗号化キーはレジストリにある(Veeam には未認証で認証情報を読める歴史 CVE がある)。また Veeam は backup account を使い、それはしばしば DA またはローカル管理者である。
**Q3**: バックアップアカウント hash を取得した後、ドメイン認証情報として使う以外に何ができる?
**A3**: バックアップアカウントで任意マシンの最新バックアップの「復元」を要求 → バックアップサーバーがマウント可能な位置へ復元 → 復元イメージからオフラインで SAM/ntds.dit/LSASS ダンプを取る。対象機に一切触れない。
**Q4**: バックアップがテープ/オフライン媒体にあり、直接復元できない?
**A4**: 直近のオンライン増分バックアップ/スナップショットへ転換する。多くの環境は直近 7〜30 日のオンラインスナップショットを保持する(RPO 要件)。
**Q5**: バックアップシステムが独立管理ネットワークにあり、入れない?
**A5**: 「バックアップエージェント」(agent)へ転換する。バックアップ対象マシンには agent が入り、通常バックアップサーバーと双方向認証する。agent サービスアカウントは横展開能力を持つことが多い。または「バックアップ設定ストレージ」(ファイル共有の可能性)を攻める。
**Q6**: バックアップシステムへの access が完全にない?
**A6**: 「横展開面が広い」似たロールへ転換する。監視システム(Zabbix/Nagios/PRTG は管理者 agent をよく設定)、デプロイシステム(Ansible/SCCM)、AV/EDR 管理コンソール(SentinelOne/CrowdStrike の管理者アカウントは任意 payload を配布できる)。

### GMSA/sMSA と KDS root key
**Q1**: ドメインに GMSA (Group Managed Service Account) がある。どう使う?
**A1**: GMSA パスワードは KDC が 30 日ごとに自動ローテーションし、msDS-ManagedPassword blob に保存される。msDS-GroupMSAMembership で許可された主体だけが読める。判定基準:制御下アカウントが許可リストにあれば、GMSA パスワードをオンラインで読める。
**Q2**: 具体的にどう読む?
**A2**: `Get-ADServiceAccount -Identity <gmsa> -Properties msDS-ManagedPassword` または GMSApasswordReader / gMSADumper。バイナリが返り、current + previous NTLM を解く。
**Q3**: 読み取り許可リストにいない。昇格できるか?
**A3**: GMSA オブジェクトに msDS-GroupMSAMembership への WriteProperty を持つアカウントを探す(自分を追加する)、またはその属性の ACE に WriteDACL を持つ。
**Q4**: さらに強いのは、KDS root key を取得した後?
**A4**: KDS root key は DC 上にあり、DA が読める。それがあれば、任意の時刻の任意の GMSA パスワードをオフラインで計算できる(常時読み取り権限は不要)。ツールは GoldenGMSA。
**Q5**: GMSA は何のサービスを動かすのに使われる?
**A5**: 通常はクラスタ/ロードバランサの重要サービス。IIS アプリケーションプール、SQL Server、SharePoint、Exchange DAG。GMSA hash を取ればそのサービスの最高身元に相当する。
**Q6**: GMSA がなく sMSA(単機 MSA)だけの場合?
**A6**: sMSA パスワードは本機 LSA secrets にあり、暗号化方式は通常の MSA と同じ → 本機 SYSTEM で dump できる。前述の「LSA secrets」チェーンへ転換する。

### クラウド上の認証情報:クラウドコンソール metadata の類推
**Q1**: 内網マシンが「IaaS 仮想マシン」(例: 自前 vSphere だが Terraform 接続、またはプライベートクラウド上)の場合、クラウド metadata に似た認証情報入口はあるか?
**A1**: ある。`~/.vmware/`、cloud-init logs、起動スクリプトにブートストラップ認証情報(bootstrap key、agent token)がありうる。プライベートクラウドも config drive/nocloud で設定をマウントする。
**Q2**: 対象マシンが Azure/AWS/GCP VM(「ハイブリッド」AD 場面でも)の場合?
**A2**: クラウド metadata を打つ。AWS IMDSv1(v2 は token が必要)`169.254.169.254`、Azure IMDS `169.254.169.254/metadata/`、GCP `metadata.google.internal`。一時 STS credential を返し、SSRF/コマンド実行と併用してリモートでも打てる。
**Q3**: クラウド credential を取得後、AD 認証情報とどう連結する?
**A3**: AD に「クラウド管理者アカウント」(クラウドへ SSO するアカウント)があるかを見る。または逆に、クラウド上の SSM/SecretsManager に AD サービスアカウントパスワードが保存されている(domain join などを実行するために使われる)。
**Q4**: クラウド VM の managed identity で KeyVault を読める?
**A4**: 対象 VM の managed identity が KeyVault 権限を持つなら、`az keyvault secret list` で一括取得できる。中に domain admin/service accounts の平文がある可能性。
**Q5**: 完全に隔離され、内網マシンが metadata endpoint へアクセスできない?
**A5**: マシン上の CLI 設定へ転換する。`~/.aws/credentials`、`~/.azure/`、`~/.config/gcloud/` に長期 access key/refresh token がよくある。
**Q6**: on-prem からクラウド認証情報面を攻めたい。起点は?
**A6**: 前述の Azure AD Connect チェーン + Golden SAML + PRT 取得。認証情報収集がクラウドまで届けば、攻撃面は「内網」から「テナント」へ拡大する。

### 記録と撤退:認証情報収集後の自律
**Q1**: 一度の行動で数十の hash + 一部平文を収集した。どう整理する?
**A1**: 構造化する。アカウント名 / 種別(ユーザー/マシン/サービス)/ hash 種別(NTLM/DCC2/AES)/ 出所(どのマシン)/ そのアカウントの既知権限 / タイムスタンプ。混乱して重複利用するのを避け、高価値アカウントの見落としも避ける。
**Q2**: いつ横展開をやめて撤退すべきか?
**A2**: 判定基準:①目標達成(指定アセット/DA を取得)。②検知リスク急上昇(EDR アラートが既に発せられた可能性)。③認証情報面が飽和(掘り続けても収益逓減)。いずれか → 記録+撤退+永続化。
**Q3**: 撤退前に何をする?
**A3**: 清掃する。dump ファイル削除、event ログ消去(ログ消去自体がアラートになる点に注意)、レジストリ変更の復元、永続化以外の埋め込み削除。欲張らない。
**Q4**: 永続化にはどのアカウントを使う?
**A4**: 最高権限アカウントを永続化に使わない。目立たないサービスアカウントまたは次高権限アカウントで定期 beacon して戻る。DA は「重要なときに一度使う」核弾頭として残す。階層化する。
**Q5**: 万一認証情報がローテーションされたら、素早くアクセスを回復するには?
**A5**: 最低二つの独立入口:①認証情報(失効しうる)。②Shadow Credentials / KeyCredentialLink(公開鍵ベースで、パスワードローテーションの影響を受けず、一度設定すれば長期有効)。③スケジュールタスク/サービス永続化(認証情報に依存しない)。
**Q6**: 判定基準:いつ「今回の認証情報収集は失敗」と認めるか?
**A6**: LSASS + SAM + LSA secrets + DPAPI + ブラウザ + サービスアカウント + ファイルシステム検索 + coerce/relay + spray の全次元を試した → すべて進展なし → 「認証情報収集」を「脆弱性経路」(未認証 CVE、Web アプリ)、またはソーシャルエンジニアリング経路(フィッシング、水飲み場)へ転換する。ひとつの次元に固執しない。
