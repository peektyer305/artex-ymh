# ADドメイン内 · DCSync と flag 取得による仕上げ

### ドメインアカウントをひとつ持っている。まず直接 DCSync できるかを判断する
**Q1**: 何らかの手段でドメインユーザーの認証情報(平文/hash/チケット)を取得した。今、DCSync へ直行したい。最初に何を確認すべきか?
**A1**: 盲目的に DCSync を発起しない。DCSync の本質は DC になりすまして DRSUAPI の GetNCChanges を呼ぶことであり、成功の前提はその主体がドメインルートオブジェクト上で `DS-Replication-Get-Changes`+`DS-Replication-Get-Changes-All` 拡張権限を持つこと。まず PowerView の `Get-DomainObjectAcl -Identity "DC=..." -ResolveGUIDs` でこの二つの GUID をフィルタし、現在の主体またはその所属グループが ACL にあるかを見る。判定基準:ヒットすれば直接同期できる。ヒットしなければ先に権限昇格するか主体を替える必要がある。
**Q2**: このアカウントがどのグループに属するか確信がない。複製権限の出所を素早く判断するには?
**A2**: `whoami /groups` でトークン内のグループ SID を見て、ドメイン内の高権限グループ(Domain Admins/Enterprise Admins/Administrators/DC グループおよびカスタム委任グループ)と照合する。グループの入れ子に注意する。多くの環境が複製権限を目立たないカスタムグループに委任している。BloodHound の DCSync エッジまたは memberof の再帰展開で調べる。
**Q3**: 権限不足のまま直接実行するとどうなる?
**A3**: mimikatz の `lsadump::dcsync` は `0x00002105 (ERROR_DS_DRA_ACCESS_DENIED)` を返す。このエラーを見たら即座に止め、再試行を繰り返して 4662 アラートを誘発しない。
**Q4**: この経路(現在のアカウントで直接同期)が権限の壁で遮られた。どの直交次元へ転換する?
**A4**: 「複製権限をどう取得するか」へ転換し、このアカウントを磨き続けない。独立した三つの支線:(1)アカウントを DA へ権限昇格する。(2)複製権限を既に持つアカウントを乗っ取る(委任グループメンバー、バックアップアカウント、Azure AD Connect の MSOL アカウントは自前の複製権を持ちやすい)。(3)ACL 攻撃で自分に権限を追加する(ドメインオブジェクト上の WriteDacl/GenericAll → Replication 権限を自己付与)。
**Q5**: ACL 攻撃で自己認可する。判定基準は何か?
**A5**: 現在の主体が `DC=domain,DC=com` に WriteDacl または GenericAll を持つなら、`Add-DomainObjectAcl -TargetIdentity "DC=..." -PrincipalIdentity <me> -Rights DCSync` で一時付与し、同期完了後すぐ Remove で復元して痕跡を減らす。判定基準:ドメインルートオブジェクト上の WriteDacl = DCSync まであと一歩。

### DCSync は誰を同期するか。いきなり全量 dump しない
**Q1**: 複製権限があることを確認した。実行するとき、ドメイン全体か単一アカウントか?
**A1**: まず定点同期し、全量はしない。全量 `/all` は何千ものオブジェクトを走査し、大量の DRSUAPI トラフィックと 4662 イベントを生み、SOC を誘発しやすい。まず `krbtgt`(ゴールデンチケット用)と `Administrator`(ドメインコントローラ着地用)だけを同期する。この二つが仕上げの中核。構文:`lsadump::dcsync /domain:X /user:krbtgt`。
**Q2**: なぜ krbtgt を優先し、直接 DA を取らないのか?
**A2**: krbtgt の NTLM hash はゴールデンチケット(Golden Ticket)の発行鍵。取得すれば、任意ユーザー・任意時間長の TGT をオフラインで偽造でき、特定の DA アカウントのパスワード変更に依存せず、ドメイン全体への永続化制御に相当する。これが「仕上げ」の戦略アセットである。
**Q3**: 単一ユーザーの同期は成功したが、横展開用に全ユーザー hash を一括取得したい。どう天秤にかける?
**A3**: `/all /csv` で一度にエクスポートするが、より高いアラートリスクを受け入れる。または高価値アカウント(サービスアカウント、他の管理者)だけを選んで個別同期する。トレードオフの判定基準:競技/実戦で flag 取得に近いなら、単点同期で足りれば全量はしない。大規模横展開が要るなら一括を検討する。
**Q4**: 同期時にユーザーが見つからない、またはドメイン名解決失敗と出た。どうする?
**A4**: DCSync は DC を解決して接続できる必要がある。`/domain` が FQDN であること、DNS が DC を指すこと、RPC(135+高位ポート)が到達可能であることを確認する。クロスドメイン同期なら `/user` は `DOMAIN\user` と書く必要がある場合がある。
**Q5**: DRSUAPI トラフィックがネットワーク層で封じられた、またはポート不通。この経路をどう転換する?
**A5**: 「DC 上でローカルに採取」という直交次元へ転換する。DC へ着地してから直接 `lsadump::lsa /inject` や `sekurlsa`、`NTDS.dit`+SYSTEM を取得してオフライン解析、またはボリュームシャドウコピーを行う。DCSync はリモート手法であり、ネットワークで封じられたら「既に DC 上にいる」ローカル経路へ切り替える。

### NTLM hash のみで平文がない。DCSync へどう進むか
**Q1**: 高権限アカウントの NTLM hash を取得したが平文ではない。DCSync を発起できるか?
**A1**: できる。DCSync は Kerberos/RPC 認証を使い、平文は不要。`sekurlsa::pth` でその hash を持つプロセスを起動するか、impacket の `secretsdump.py -hashes LM:NT domain/user@DC` で直接リモート DCSync する。判定基準:hash があれば DCSync へ PtH でき、平文を解く必要はない。
**Q2**: impacket の secretsdump と mimikatz の dcsync のどちらを選ぶ?
**A2**: Linux/クロスプラットフォームの踏み台から、または Windows に着地していない場合は impacket の secretsdump(純粋なリモートで、バイナリアップロード不要)。既に Windows 上で PtH でプロセスを起動して後続操作するなら mimikatz。現在の足場がどの OS かで判断する。
**Q3**: PtH でプロセスを起動しても dcsync が拒否される。どこが誤りか?
**A3**: (1)hash に対応するアカウントが本当に複製権限を持つか。(2)NTLM 制限/`Protected Users` グループで hash が使えないか。(3)時間ずれで Kerberos が失敗していないか。逐項排除する。
**Q4**: 対象アカウントが Protected Users グループ、または AES 強制で PtH が遮られた。どこへ転換する?
**A4**: 「NTLM ではなく Kerberos 鍵を使う」直交次元へ転換する。そのアカウントの AES256 鍵を取って `ptt`(overpass-the-hash)するか、TGT チケットを直接取って注入する。NTLM が禁止されても Kerberos 経路は通ることが多い。
**Q5**: Kerberos も通らない(強力な二要素/スマートカード)場合、まだ出口はあるか?
**A5**: 「マシンアカウントまたは委任を使う」直交次元へ転換する。マシンアカウント `マシン名$` は Protected Users の制限を受けないことが多い。対応する RBCD/委任があれば s4u チェーンで TGS を取れる。またはこのアカウントを諦め、複製権限を持つ別アカウントでやり直す。

### krbtgt を取得した後、ゴールデンチケットを「flag 取得」へ落とし込む
**Q1**: DCSync で krbtgt の hash を取得した。flag 取得のため次にどう使う?
**A1**: ゴールデンチケットのためのゴールデンチケットにしない。まず flag がどこにあるかを問う。flag が特定サーバー/DC のファイルにあるなら、ゴールデンチケットは「任意身元で任意サービスへアクセスする」手段に過ぎない。`kerberos::golden` で Administrator の TGT を偽造し、注入後に直接 `dir \\対象\c$` や PsExec で flag を読む。ゴールデンチケットは通行証であり終点ではない。
**Q2**: ゴールデンチケット作成にどのパラメータが要り、欠けるとどうなる?
**A2**: krbtgt の NTLM(または AES)、ドメイン SID、ドメイン名、偽造ユーザー名/RID が必要。ドメイン SID は `whoami /user` で現在のドメイン SID から末尾 RID を外すか、`Get-DomainSID` で取る。SID 欠落や誤りはチケットが無効になるがエラーは出ず、アクセス時に初めて失敗し、原因特定が難しい。必ず照合する。
**Q3**: ゴールデンチケットを作って注入したが、対象アクセスがやはり拒否される?
**A3**: (1)対象が同一ドメインか(クロスドメインはクロスドメイン信頼チケットまたは Enterprise Admin の SID History が要る)。(2)異常チケットを検知する防御がないか(超長有効期間は特徴。デフォルト 10 年ではなく 10 時間など合理的な長さに設定することを推奨)。(3)正しいセッションへ注入したか(`kerberos::ptt` 後に `klist` で確認)。
**Q4**: このマシンに EDR があり、ゴールデンチケット(異常 TGT 特徴)を専門に検知する。この経路が遮られたらどう転換する?
**A4**: 「偽造チケットではなく本物の認証情報を使う」直交次元へ転換する。krbtgt で DCSync できるなら、対象マシンのローカル管理者やドメイン管理者の本物の hash も DCSync で出せる。本物の hash で PtH 横展開し、ゴールデンチケット検知を誘発しない。または一歩引いてダイヤモンドチケット(Diamond Ticket。本物の TGT を改変し、無から作らない)で特徴を回避する。

### flag は DC にないかもしれない。flag を特定してから DCSync の要否を決める
**Q1**: DA に近づいたが、立ち止まって考える。flag 取得に本当に DCSync が必要か?
**A1**: 逆に考える。DCSync は「ドメイン制御」級の手段で、コストもアラートも高い。まず flag の位置と読み取りに必要な権限を確認する。flag は一般業務機の web ディレクトリ、あるユーザーのデスクトップ、データベースにあるかもしれない。ローカル管理者か業務アカウントで読めるなら、DC を騒がせる必要はない。判定基準:必要な最小権限 < ドメイン管理者なら、DCSync という牛刀を使わない。
**Q2**: flag がどのマシンにあるかを素早く判断するには?
**A2**: 題意/業務ロジックから逆算する(flag は「王冠アセット」によくある。コア DB、ファイルサーバー、DC、特定のターゲット機)。技術的には `Find-InterestingDomainShareFile` で共有内の flag.txt/*.flag をスキャンし、web ルートを探り、データベースを調べる。先にスキャンしてから打つ。
**Q3**: 一通りスキャンしたが目立つ flag ファイルがない。DC に上がるしかないか?
**A3**: 必ずしもそうではない。flag は (1)より高い権限でないと列挙できないディレクトリ、(2)レジストリ/環境変数/スケジュールタスク、(3)「ドメイン制御の証明」そのもの(問題が krbtgt hash や DA 権限のスクリーンショット提出を要求)の可能性がある。三番目なら DCSync/ドメイン制御こそが真の対象。先に題意を読み、「flag=ファイル」か「flag=制御権の証明」かを判断する。
**Q4**: 対象機が DC 上にあるが DC に入れない。この正面経路が塞がったらどう転換する?
**A4**: 「ドメイン権限で DC 上のファイルを間接的に読む」直交次元へ転換する。DA/複製権限を持てば、対話ログオンせずとも、WMI/WinRM/SMB でリモートからファイルを読める。`reg` でレジストリをリモート読み、または DCSync で DC ローカル管理者 hash を出してから SMB で C$ へ接続する。「DC へログオンして flag」を「DC 上のそのファイルをリモートで読む」へ格下げする。

### secretsdump を回して大量の hash が出た。flag を取れる鍵をどう見つけるか
**Q1**: DCSync/secretsdump で全域数百アカウントの hash をエクスポートした。大量の hash を前に、まず誰を見る?
**A1**: 個別にブルートフォースしない。優先順位:(1)`Administrator` と組み込みドメイン管理者。(2)`krbtgt`。(3)admin/svc/backup/sql/exchange を含む名前のサービスアカウント。(4)対象業務機のマシンアカウント(`マシン名$`)。「flag に最も近いアセットの管理者」順に並べ、上から下へ走査しない。
**Q2**: flag があるマシンにログオンできるアカウントをどう知る?
**A2**: BloodHound にインポートして「対象機への最短経路」を調べるか、`Find-LocalAdminAccess` でどのアカウントが対象機にローカル管理者権限を持つかを確認する。権限を持つその hash が鍵であり、他は当面無関係。
**Q3**: hash を取得したが対象機が PtH を受け付けない(NTLM 無効またはローカルアカウントフィルタ)?
**A3**: ローカルアカウントのリモート PtH は `LocalAccountTokenFilterPolicy` や UAC リモート制限に遮られることがある。ドメインアカウント hash に切り替えるか、Kerberos(overpass-the-hash で AES 鍵)を使うか、マシンアカウント hash で `s4u` を使う。
**Q4**: 全 hash が対象機にローカル管理者権限を持たず、横展開が遮られた。どう転換する?
**A4**: 「対象機のアクセス制御を変える」直交次元へ転換する。既にドメインを制御しているなら、GPO で自分をローカル管理者に追加するか、RBCD(リソースベース制約付き委任)を対象機へ設定して s4u でチケットを取るか、対象機が属する OU の GPO を変更する。ドメイン制御後の横展開は単機 ACL に阻まれるべきではない。「ドメイン層から権限を押し下げる」方式に切り替える。

### DC へ着地後、mimikatz を使わない hash 取得経路
**Q1**: 既に DC 上の管理者セッションだが、EDR が mimikatz を監視している。ドメイン hash をどう取る?
**A1**: 必ずしも mimikatz を使わない。四つの常用迂回:(1)`ntdsutil "ac i ntds" "ifm" "create full C:\Temp\a" q q` のワンコマンドで NTDS.dit+SYSTEM+SECURITY を生成。(2)`vssadmin create shadow` でボリュームシャドウコピー後、シャドウデバイスから `NTDS.dit` をコピー。(3)`reg save HKLM\SYSTEM`+`reg save HKLM\SECURITY`+`reg save HKLM\SAM` でローカル取得。(4)diskshadow スクリプトでボリュームシャドウを自動化。踏み台へ持ち帰った後、impacket の `secretsdump.py -ntds NTDS.dit -system SYSTEM LOCAL` でオフライン解析する。
**Q2**: ntdsutil も vssadmin も Windows 標準なのに、なぜ EDR がまだ遮断しうる?
**A2**: 現代の EDR は `ntdsutil ac i ntds` のコマンドライン文字列を直接マッチする。diskshadow スクリプト(コマンドをファイルに書いて `diskshadow /s script.txt`)、または PowerShell 反射で `WMI Win32_ShadowCopy.Create` を呼んで文字列マッチを迂回する。判定基準:「コマンドライン特徴が最も敏感でない」経路を選ぶ。
**Q3**: DC ローカルにファイルを落として痕跡を残したくない。純粋にリモートで取れるか?
**A3**: できる。impacket の `secretsdump.py -just-dc-ntlm DOMAIN/user@DC` で純粋リモート DCSync。または `wmiexec`+`vssadmin` でボリュームシャドウをトリガーしてから SMB で持ち出す(それでも着地はある)。完全に非着地なら DCSync を使う。
**Q4**: NTDS.dit を取得したが解析失敗/hash が出ない。どうする?
**A4**: よくある落とし穴は、SYSTEM hive と NTDS.dit が同一時刻の整合スナップショットでなければならないこと。異なる時点を混ぜると復号できない。整合スナップショットを取り直す。または `impacket-secretsdump -ntds ... -system ... -history` で鍵履歴問題かを見る。
**Q5**: DC のディスクが読み取り専用/`C:\Temp` に書けない。ntdsutil が失敗したらどう転換する?
**A5**: 「メモリ内の生きた LSASS を読む」または「リモート DCSync」という二つの直交経路へ転換する。メモリ経路は `Rubeus`/`SafetyKatz` などの LSASS ダンプツールだが EDR を通過する必要がある。リモート経路は DC ディスクへ落とさない。または別の書き込み可能ディレクトリ(DC 上の共有など)へ書いてシステムドライブ容量制限を回避する。

### DCSync が EDR に遮断された場合の迂回決定ツリー
**Q1**: dcsync を発起した瞬間に EDR に kill された/プロセスが終了した。どうする?
**A1**: まず「コマンドライン/プロセス名/署名で殺された」のか「DRSUAPI 呼び出し挙動で遮断された」のかを区別する。前者は実装を替えればよい(impacket、自作 dcsync コード、C# ツール)。後者はより隠密な呼び出し方、または別の hash 取得経路へ切り替える必要がある。判定基準:別ツールで再試行し、同じタイミングで殺される → 挙動検知。動くが結果異常 → ネットワーク/権限問題。
**Q2**: ツールを替える具体的な優先順位は?
**A2**: (1)impacket の `secretsdump.py -just-dc` は純 Python で静的シグネチャに命中しにくい。(2)C# 実装の DCSync ツール(SharpKatz、DSInternals)はコンパイル後の AMSI/AV 署名が新しい。(3)DRSUAPI を自前で呼ぶ Rust/C バイナリ。判定基準:「EDR のブラックリストにないもの」を先に試す。
**Q3**: 全ツールの DRSUAPI 呼び出しが挙動検知で遮断される(異常 IP からの複製発起)?
**A3**: 「正当な発信元から発起する」方針へ転換する。既存の DC または AzureADConnect サーバーを侵入し、その IP から複製を発起すれば正常な同期に見える。または DCSync を使わず、ntdsutil/ボリュームシャドウでローカル取得する。
**Q4**: 時間がなく、迂回する余裕がない。この遮断された経路を完全に諦めてどこへ転換する?
**A4**: 「DCSync を使わない仕上げ経路」へ転換する。(1)既に DA なら DC へ直接着地してローカル取得。(2)ドメインコントローラ級の認証情報ではなく flag の具体的な位置を探す。(3)Kerberoasting/AS-REP でサービスアカウントを取り、そのサービスアカウントが対象機に権限を持てば十分。DCSync は経路のひとつに過ぎず、唯一ではない。

### DC が出網しない/接続不能な場合の DCSync
**Q1**: 踏み台上で secretsdump を DC へ接続しようとしたが、接続失敗またはタイムアウト。どう診断する?
**A1**: DCSync は RPC(TCP 135 + 動的高位ポート)+ SMB(445)を使う。まず `nc -zv DC 135 445` でポートを確認し、踏み台 SOCKS プロキシ経由が必要かを見る。よくある問題:(1)ネットワークセグメント分離で、DC は内網のある VLAN にのみ開放。(2)ファイアウォールが DC 間の複製トラフィックのみ放行。
**Q2**: ポートは通るが DCSync がやはり失敗する?
**A2**: プロキシを確認する。impacket は `proxychains` や socks の正しい設定が必要で、RPC の動的ポートは多くのプロキシが全ポートを通さない。`ntlmrelayx`+`--socks` で長接続を保持するか、DMZ の制御済みホスト上で直接 secretsdump を実行し、一段隔てない。
**Q3**: 外網踏み台からしか打てず、DC が厳格に内網の複製ポートのみ開放している?
**A3**: 「内網のマシンを制御してそこから発起する」方式へ転換する。Cobalt Strike/sliver で内網マシンに beacon を建て、beacon 内から DCSync を実行する(ツールは既に内網にあり、外網から RPC 全ポートを貫通する必要がない)。
**Q4**: 内網にも使える踏み台がなく、DMZ の webshell だけ?
**A4**: 「tunnel+proxy」直交次元へ転換する。reGeorg/frp/ligolo で SOCKS トンネルを張り、その中で secretsdump を実行する。このチェーンの鍵は、ツールの全 RPC 動的ポートを透過させることで、ligolo/wg トンネルは HTTP 転送より優れる。
**Q5**: トンネルも張れない(DMZ の出網が厳格)場合は?
**A5**: 「DCSync をしない」へ完全転換する。DMZ webshell 場面では DCSync のコストが大きすぎる。先に内網で DC と通じるマシンへ横展開してから決める。またはドメイン制御を諦め、DMZ 内にありうる flag の業務システムを直接探す。

### Kerberos クロックずれによる DCSync 失敗
**Q1**: DCSync が `KRB_AP_ERR_SKEW` または "clock skew too great" を報告。原因は?
**A1**: Kerberos はクライアントと KDC の時刻差を 5 分未満(デフォルト)に要求する。踏み台マシンの時刻と DC が同期していないと失敗する。クロスタイムゾーン/Linux 踏み台コンテナが NTP 同期していない場合に多い。判定基準:skew/クロック関連エラーを見たら即座に時刻を疑う。
**Q2**: 素早く時刻を合わせるには?
**A2**: Linux で `sudo ntpdate DC_IP` または `sudo rdate -n DC_IP`。impacket スクリプトは `-dc-ip` を設定し本機時刻を DC と一致させる。root がなければ `faketime` でコマンドラインをラップして時刻を偽造する。
**Q3**: NTP トラフィックが出られない/DC が NTP 照会を受け付けない。どうする?
**A3**: Kerberos 失敗情報から DC 時刻を逆算する。`KRB_AP_ERR_SKEW` エラーに DC 時刻が含まれることがある。または先に AS-REQ をリクエストしてレスポンスパケットのタイムスタンプを見る。その後 `faketime` でローカル時刻を微調整する。
**Q4**: 時刻を合わせたのに別ツールでまた skew。どう根本解決する?
**A4**: コンテナ/WSL 時刻のドリフトを確認する(よくある問題:コンテナがホスト NTP と同期しない)。シェルで `export FAKETIME=...` を統一するか、`chrony` で長期同期を保つ。判定基準:複数回試して全て skew = クロックサービス自体が動いていない。
**Q5**: 時刻を合わせられない(厳格なネットワーク)。この経路が完全に通らない場合、どう転換する?
**A5**: 「Kerberos ではなく NTLM を使う」直交次元へ転換する。DCSync は NTLM 認証を使える(secretsdump はデフォルトで fallback 可)ため、クロック依存を避けられる。または時刻同期不要の hash 取得手段(リモートレジストリ、ボリュームシャドウ)に切り替える。

### BloodHound で図を作った後の DCSync 最短経路判断
**Q1**: BloodHound データを採取した。現在のアカウントから DCSync を見つけるにはどのクエリを使う?
**A1**: 組み込みの `Shortest Paths to Domain Admins` と `Find Principals with DCSync Rights` を使う。さらにカスタム Cypher クエリ:`MATCH p=shortestPath((n {name:"現在のアカウント"})-[*1..]->(m:Domain)) WHERE ANY(r IN relationships(p) WHERE type(r) IN ["DCSync","GetChanges","GetChangesAll","GenericAll","WriteDacl","Owns"]) RETURN p`。これで DCSync 権限まで最低何手かを示す。
**Q2**: 図に大量の経路がある。最適なものをどう選ぶ?
**A2**: 「エッジ数が少ない+エッジ種別が安定している」経路を優先する。安定エッジ:グループへの AddMember、ユーザーへの GenericAll、ForceChangePassword。これらは一回の操作で成立する。不安定エッジ:HasSession(ユーザーのオンラインが必要)、CanRDP(認証情報が必要)、ExecuteDCOM(ネットワーク+防御の弱さが必要)。判定基準:「オフライン操作で即完了」するエッジ > 「条件発生を待つ」エッジ。
**Q3**: 図で、あるグループが重要な中継だが、どう加入すればよいか分からない?
**A3**: エッジ種別を確認する。AddMember/GenericAll on group は自分を追加できることを意味する。エッジが "AddSelf" なら直接一手で追加。入れ子の委任グループなら先にグループ owner を制御する必要がある場合がある。
**Q4**: BloodHound データが古く、現場の権限が変わっている可能性がある。データが信頼できない場合は?
**A4**: 再採取するか小規模検証する。SharpHound の `--CollectionMethods DCOnly` でオブジェクトと ACL データを素早く更新する(セッション走査は不要)。判定基準:図は方向を示すもので、細部はその場で実地確認する。
**Q5**: 図に現在の主体から DCSync への経路がまったくない。行き止まりか?
**A5**: 違う。「図の外の攻撃面」へ転換する。BloodHound には含まれないもの:CVE(Zerologon/noPac/PrintNightmare)、ADCS ESC(専用採取が必要)、Coerce+Relay チェーン、パスワード再利用横展開。bloodhound-ce または ADCS collector でデータを補うか、直接 CVE 経路を検討する。図が空 = 次元を変える。

### Kerberoasting で SPN サービスアカウントを取って DCSync へ行けるか
**Q1**: 一般ドメインユーザーだ。Kerberoasting で高権限アカウントを取れるか?
**A1**: 可能性はある。任意のドメインユーザーはドメイン内 SPN アカウントの TGS をリクエストでき、オフラインで hashcat モード 13100 で解読できる。判定基準:`Get-DomainUser -SPN | ?{$_.MemberOf -match "Admin"}` を探す。あるサービスアカウントが高権限グループに属し、パスワードが弱ければ、一発で DA に届く。
**Q2**: 解読したパスワードで何ができる?
**A2**: 平文/hash を取得後:(1)DA または複製権限主体なら直接 DCSync。(2)特定マシンにローカル管理者があれば、先に横展開してさらに高い認証情報を探す。(3)サービスアカウントは超長期間パスワード未変更が多く、再利用価値が高い。
**Q3**: SPN アカウントのパスワードが強く解読できない?
**A3**: 磨かない。転換する:(1)AS-REP roasting で事前認証を無効化したアカウントを探す。(2)他の攻撃面を探す。判定基準:強パスワードの Kerberoasting は解なし。時間コストを考えると次元を変える方が価値がある。
**Q4**: SPN リストに高権限アカウントがなく、低権限サービスアカウントだけ?
**A4**: 「低権限サービスアカウントで何ができるか」へ転換する。特定サーバーにローカル管理者を持つ、またはオブジェクト ACL(例: some group への WriteDacl)を持ち、間接的に DA へ通じる可能性がある。BloodHound または手動で outbound 権限を調べる。低権限アカウント+ACL チェーンは意外な通路になりがち。
**Q5**: ドメイン内に使える SPN がまったくない(または全て MSA/gMSA)。この経路が完全に塞がれたらどう転換する?
**A5**: AS-REP、NTLM Relay、CVE 経路(Zerologon/noPac)、または ADCS ESC へ転換する。Kerberoasting は「サービスアカウントのパスワードが弱い」という賭けに過ぎない。賭けられないなら別の賭け台へ移る。

### AS-REP Roasting で低権限アカウントを取り、DCSync へ昇格する
**Q1**: あるアカウントが `DONT_REQ_PREAUTH` で、AS-REQ を一発送ると暗号化 TGT が返る。何ができる?
**A1**: この暗号化データを取得してオフライン解読する(hashcat モード 18200)。平文を解けばそのアカウントの認証情報を得る。判定基準:事前認証を無効化したアカウントを見つける = オフライン解読可能な hash がひとつ手に入り、後続認証に影響しない。
**Q2**: 解読したアカウントは通常権限が低い。DCSync へ近づくには?
**A2**: 三手。(1)それでログオンして列挙面を拡大する(一部の ACL/グループ情報は普通の匿名では読めない)。(2)それで Kerberoasting/BloodHound を実行する。(3)ACL 図での位置を見る。あるグループの member かもしれず、チェーン上に委任があるかもしれない。判定基準:AS-REP アカウントの価値 = 列挙入口と ACL 起点がひとつ増えることで、終点ではない。
**Q3**: 事前認証を無効化したアカウントがない?
**A3**: `GetNPUsers.py -no-pass -usersfile <name-list>` で盲探する。ドメイン内ユーザーの命名規則(名前の頭文字.姓、社員番号)でアカウントリストを列挙できることが多い。見つからなければ経路を変える。
**Q4**: 見つかったがパスワードが解読できない。どう転換する?
**A4**: 「username enumeration」の副産物として使う方向へ転換する。TGT が返ることはアカウントの存在証明であり、確認済みユーザーリストを構成して後続のパスワードスプレーに使う。または完全に CVE 経路へ転換する。

### NTLM Relay で LDAP へ DCSync 権限を付与する
**Q1**: 内網で高権限アカウントまたは DC の NTLM 認証を取得した。直接 LDAP へ relay して DCSync できるか?
**A1**: できるが条件がある。対象 LDAP が署名強制でないこと(デフォルトの DC は LDAP 署名を強制するが、389 ポートの非署名 LDAP や一部の古い設定は許可しうる)。ntlmrelayx の `-t ldaps://DC --escalate-user 現在のアカウント` で、relay された高権限認証を「現在のアカウントへ DCSync ACL を追加」に変換する。判定基準:成功すれば現在のアカウントで DCSync できる。
**Q2**: LDAP に署名がある場合は?
**A2**: LDAPS(636/3269)へ転換する。LDAPS はデフォルトで SMB Signing/LDAP Signing ポリシーの制約を受けないが、channel binding が無効である必要がある。`-t ldaps://` を使う。判定基準:LDAPS 636 が使える = relay がまだ通る可能性。先に試す。
**Q3**: 認証源をどう得る?受動で待つか能動で強制するか?
**A3**: 能動強制:PetitPotam/PrinterBug/DFSCoerce で DC または対象ホストに自分の SMB へ NTLM 認証を発起させる。受動:mitm6 で WPAD/IPv6 応答を乗っ取り、マシンに能動認証させる。判定基準:実戦では能動強制を優先(PetitPotam は EFSRPC を使い、まだ開いていることが多い)。
**Q4**: relay は成功したが、権限付与後に DCSync がやはり失敗する?
**A4**: escalate が実際に何の権限を付与したかを確認する。ntlmrelayx はデフォルトで DCSync 権限を付与するか、組み込み admin グループへ追加する。`Get-DomainObjectAcl "DC=..."` で自分が追加した ACE が有効かを確認する。LDAP 複製伝播を待つ必要があるか、別 DC で試す。
**Q5**: 全域で LDAP 署名強制 + channel binding。relay 経路が完全に塞がれたらどう転換する?
**A5**: 「ADCS Web Enrollment へ relay」(ESC8)へ転換する。coerce で得たマシンアカウント認証を `/certsrv` へ relay してマシンアカウント証明書を発行し、証明書 PKINIT で TGT を取り S4U2Self で DA を取る。または coerce + 他設定脆弱性の経路へ転換する。

### PetitPotam/DFSCoerce で DC 認証を強制する際の選型
**Q1**: マシンに認証発起を coerce したい。いくつかの手法(SpoolSample/PetitPotam/DFSCoerce/ShadowCoerce)をどう選ぶ?
**A1**: 「どのサービスがデフォルトで開いており、パッチが当てにくいか」で並べる。(1)DFSCoerce(MS-DFSNM)——デフォルトで開き、塞ぎにくい。(2)PetitPotam(EFSRPC)——パッチ適用後も他の RPC インターフェースが使える。(3)SpoolSample(Print Spooler)——PrintNightmare 後に多くの環境が印刷スプーラーを無効化。(4)ShadowCoerce(FSRVP)——File Server VSS サービスに依存。判定基準:まず DFSCoerce、失敗したら PetitPotam に降格、次に SpoolSample。
**Q2**: coerce が強制するのは誰のマシンアカウントか?
**A2**: 誰を強制するかで、その誰が SYSTEM として自分の SMB/NTLM 認証を発起し、得られるのは「対象マシン$」の hash か、その認証の relay である。coerce で DC = DC$ のマシンアカウント認証を取得 = ADCS/LDAP へ relay して DA を取れる。
**Q3**: coerce を送ったが自分の側に認証が届かない?
**A3**: (1)ファイアウォール/ネットワークセグメントが自分の IP の 445 へ到達可能か。(2)UNC パスが WebDAV を経由していないか(WebClient サービスの有効が必要)。(3)一部の coerce はトリガー側が自分の hostname を解決できることを要求する。IP で直接トリガーする。判定基準:coerce ツールの出力 "success" は認証到達を意味しない。tcpdump で入站 445 を確認する。
**Q4**: coerce がパッチで塞がれた(全インターフェースがパッチ済み)。どう転換する?
**A4**: 「coerce 不要の relay」へ転換する。mitm6 の IPv6 乗っ取りでマシンに能動認証させる(RPC インターフェース非依存)。または relay を諦め、Kerberos delegation/ADCS ESC1 テンプレート悪用など他の DA 経路へ転換する。

### ADCS ESC1:弱テンプレートで一段階 DA
**Q1**: ドメイン内に CA がある。証明書経路で DCSync へ行けるか?
**A1**: できる。Certify/certipy でテンプレートを列挙し、`ESC1` 特徴を探す。テンプレートが「クライアント認証」+「Enrollee が SAN 提供可能」+「低権限ユーザーが申請可能」を許すもの。ヒットすれば現在のアカウントで SAN=Administrator の証明書を申請し、PKINIT ログオンすれば即 DA。
**Q2**: certipy find/Certify.exe find が大量のテンプレートを出力する。ESC1 を素早く識別するには?
**A2**: 四条件が並ぶかを見る。(1)`Client Authentication` または `Any Purpose` EKU。(2)`ENROLLEE_SUPPLIES_SUBJECT` フラグ。(3)Enroll rights に現在のユーザーまたはそのグループを含む。(4)管理者承認が未有効。四条件が全て揃えば ESC1。
**Q3**: 証明書を取得後、どう DCSync に変える?
**A3**: PKINIT 認証:`certipy auth -pfx admin.pfx` で TGT+NT hash を取得する(UnPAC-the-Hash)。その後 hash で DCSync するか、TGT で直接 DC リソースへアクセスする。
**Q4**: ESC1 テンプレートはあるが現在のアカウントに enroll 権限がない?
**A4**: 他の ESC へ転換する。ESC2(Any Purpose EKU)、ESC3(登録代理)、ESC4(テンプレート ACL 書き込み可)、ESC6(EDITF_ATTRIBUTESUBJECTALTNAME2)、ESC7(CA 管理者)、ESC8(HTTP エンドポイント relay)、ESC9/ESC10/ESC11。判定基準:ADCS には十数個の ESC があり、ひとつが通らなければ次へ。固執しない。
**Q5**: ドメインに ADCS がそもそもない。どう転換する?
**A5**: 非 ADCS 経路へ転換する。ADCS は「あれば非常に美味しい」ボーナスであり、必須経路ではない。Kerberoast/AS-REP/ACL/Coerce+Relay/CVE の標準メニューへ戻る。

### ADCS ESC8:coerce+relay で web enrollment へ
**Q1**: LDAP relay が強署名で遮られた。ADCS Web Enrollment へ relay できるか?
**A1**: できる。CA の `/certsrv` はデフォルト NTLM 認証で、署名/channel binding を長年打っていないことが多い。`ntlmrelayx -t http://CA/certsrv/certfnsh.asp --template DomainController` で coerce で得たマシン認証を relay し、対象マシンアカウント証明書を発行する。判定基準:ADCS Web Enrollment HTTP エンドポイントが開いている + coerce が認証をトリガーできる = ESC8 利用可能。
**Q2**: どのテンプレートを発行する?
**A2**: `DomainController` または `Machine` テンプレート(デフォルトで全ドメインマシンが申請可能)を発行し、DC$ に一枚発行する。証明書取得後 PKINIT で DC$ の TGT+hash を取得。DC$ は自身に複製権を持つため、直接 DCSync できる。
**Q3**: `/certsrv` へ relay して失敗、401 が出る?
**A3**: (1)certsrv が NTLM を無効化していないか。(2)HTTPS が要るか(`--adcs` オプションを試す)。(3)coerce の対象マシンアカウントが除外されていないか。またはエンドポイントを変える:`/certsrv/certfnsh.asp` vs `/certsrv/certnew.asp`。
**Q4**: ADCS Web エンドポイントが開いていない(純 DCOM enrollment)?
**A4**: ESC11(DCOM enrollment 上の relay が有効な場合)または純 ESC1/ESC4 など web 不要の経路へ転換する。または他の発行入口(NDES/SCEP)を探す。

### シャドウ認証情報 (Shadow Credentials / msDS-KeyCredentialLink)
**Q1**: ある高権限オブジェクトに GenericWrite がある。ForceChangePassword を使わずに、より隠密な経路は?
**A1**: ある。Shadow Credentials。対象の `msDS-KeyCredentialLink` 属性に、自分が制御する Key Trust 認証情報(証明書公開鍵)を書き込み、その後秘密鍵で PKINIT ログオンしてそのアカウントの TGT+hash を取得する。ツール:pywhisker/Whisker+Rubeus。パスワード変更より業務に影響せず、対象ユーザーが全く気付かない。
**Q2**: シャドウ認証情報の前提条件は?
**A2**: (1)ドメイン機能レベル 2016 以上。(2)ドメイン内に KDC が PKINIT をサポート(ほぼ全ての現代ドメイン)。(3)対象オブジェクトへの `GenericAll`/`GenericWrite`/`WriteProperty on msDS-KeyCredentialLink`。判定基準:三条件全て満たせば、強制パスワード変更より隠密な第一選択。
**Q3**: 書き込み後に PKINIT が失敗する?
**A3**: (1)証明書が KeyCredentialLink 項目へ正しく紐づいているか。(2)クロック同期。(3)対象が Protected Users か(Kerberos を制限される)。
**Q4**: 対象が Protected Users、またはドメイン機能レベルを満たさない。シャドウ経路が通らない?
**A4**: 「強制パスワード変更+ TGT 取得 + 戻し」の古い経路(computer オブジェクトへの RBCD 攻撃でも可)、または RBCD 経路へ転換する。判定基準:シャドウ認証情報が通らない = 他の ACL 利用手法を使う。行き止まりではない。

### RBCD (リソースベース制約付き委任) でコンピュータオブジェクトを乗っ取る
**Q1**: 対象コンピュータオブジェクトに GenericWrite または `WriteProperty on msDS-AllowedToActOnBehalfOfOtherIdentity` がある。何ができる?
**A1**: RBCD。自分が制御するマシンアカウント(自作または制御済み)を対象の `msDS-AllowedToActOnBehalfOfOtherIdentity` に追加し、その後制御下マシンアカウントから対象へ S4U2Self+S4U2Proxy で DA になりすましたサービスチケットを取る。ツール:impacket rbcd.py + getST.py。対象へのローカル管理者ログオンと同等。
**Q2**: 「制御下マシンアカウント」をどう作る?
**A2**: ドメイン内の一般ユーザーはデフォルトで 10 台のマシンをドメインに追加できる(MachineAccountQuota=10)。impacket の `addcomputer.py` で新しいマシンを追加し、そのパスワード/hash を S4U 発起者として使う。判定基準:MAQ>0 = 一手でマシンを作れる。
**Q3**: MAQ が 0 に変更された場合は?
**A3**: 「パスワード/hash が既知のマシンアカウント」または「ServicePrincipalName を持つ一般アカウント」を発起者として使う。または制御済みの他コンピュータオブジェクトを利用する。
**Q4**: DA の S4U チケットを取得した。DCSync できるか?
**A4**: そのチケットは特定の service 専用で、通常 CIFS/HTTP など。直接 SMB/WinRM で対象機へログオンしてローカルを取るか、対象が DC ならそこから DCSync する。または直接 DC へ RBCD して DC の DA チケットを取る → 通吃。
**Q5**: GenericWrite もなくマシン追加もできない。RBCD 経路が完全に塞がれたらどう転換する?
**A5**: 「既存委任設定の変更」または「シャドウ認証情報」または「伝統的な ACL チェーン」へ転換する。RBCD は ACL 利用のひとつに過ぎず、通らなければ換える。

### ACL 自己付与 DCSync 権限後のタイミング、痕跡、落とし穴
**Q1**: ACL 攻撃で自分に DCSync 権限を付与した。痕跡を最も残さない操作順序は?
**A1**: 順序は「付与 → 即 dcsync → 即 Remove 復元」。三手の間には何の余計な動作もしない。ACL 変更はイベント 5136 を書く。単一の「付与+復元」ペアは必ずしも疑われないが、間に大量の他動作を挟んだり、タイムラインが長すぎたりすると、SOC が遡る際に摘出されやすい。判定基準:付与と取り消しの間の時間窓が狭いほど隠密。
**Q2**: 付与は成功したが DCSync が即拒否される?
**A2**: ACL が DCSync を発起したその DC へまだ複製されていない可能性がある。30 秒から数分待つか、「付与した同じ DC」を同期源に明示指定する。または別の同期済み DC を選ぶ。判定基準:マルチ DC 環境では ACL 伝播に遅延がある。同期直後の失敗で権限自体を疑わない。
**Q3**: ドメインルート ACL の変更が直接拒否された?
**A3**: (1)セッション認証切れ(再 pth/kinit)。(2)RODC 上では書き込み権限がない(主 DC へ切り替え)。(3)AdminSDHolder 保護周期内で SDPROP が直前に走り、対象が保護グループに属していて権限がロールバックされた。判定基準:同じコマンドを主 DC/別認証セッションで再試行し、「自分」か「この DC」の問題かを特定する。
**Q4**: DCSync を付与したら全 hash を読めるか、それとも特定アカウントのみか?
**A4**: `GetChanges`+`GetChangesAll` の二つを合わせれば、パスワード hash を含む全属性を同期できる。`GetChanges` だけを追加して `GetChangesAll` を欠くと、メタデータは同期できるが hash フィールドがフィルタされ、dcsync はエラーになるが権限不足に見える。判定基準:二つの ACE が欠かせない。半分だけ付与しない。
**Q5**: この ACL 経路が完全に通らない(WriteDACL/GenericAll がなく、または全て拒否)。どう転換する?
**A5**: 「変更するのでなく授ける」の直交次元へ転換する。(1)対象身元オブジェクト(高権限ユーザー/グループ)に WriteProperty があるなら、その属性を変更する(member に自分を DA グループへ追加、または msDS-KeyCredentialLink を変更してシャドウ認証情報)。(2)ある OU に GenericAll があるなら、OU 配下の GPO を変更して権限を逆に押し出す。(3)ACL チェーンを完全に諦め、委任/証明書/CVE へ進む。判定基準:ドメインルート ACL は唯一の入口ではなく、身元オブジェクト/グループ/OU/GPO はすべて並行する認可面である。

### Backup Operators グループの隠れた特権
**Q1**: 現在のアカウントが Backup Operators グループにいる。DA までどのくらいか?
**A1**: 非常に近い。Backup Operators は DC 上で `SeBackupPrivilege`+`SeRestorePrivilege` を持ち、ACL を迂回して任意ファイルを読み取れる。`NTDS.dit`+SYSTEM も含む。DC へ着地後、`robocopy /B` または wbadmin でバックアップし、NTDS.dit を持ち出してオフラインで hash を復号する。判定基準:Backup Operators = 事実上の DCSync 相当物であり、経路が「オフライン NTDS」なだけ。
**Q2**: Backup Operators は DC へリモートログオンできない。どう着地する?
**A2**: いくつか。(1)`wbadmin start backup` でリモートからバックアップタスクをトリガーし、ファイルを共有へ落とせる。(2)Task Scheduler でリモートから Backup Operators 身元でタスクを作成。(3)`reg save` をリモート呼び出し(SeBackupPrivilege+リモートレジストリサービスが必要)。判定基準:対象機上で「このグループのリモート操作を受け付ける」インターフェースを選ぶ。
**Q3**: DC 上のリモートレジストリとスケジュールタスクが両方無効?
**A3**: 「Backup Operators で他の重要アセットをバックアップする」へ転換する。Backup Operators はファイルサーバーにも権限を持つことがあり、他の機微ファイル(GPP パスワードファイル、バックアップパッケージ内の認証情報)を読める。判定基準:Backup Operators 権限 = 任意の ACL 遮蔽ファイルを読める。それで flag を読む方がドメイン制御より速い場合がある。
**Q4**: バックアップ権限が厳格に監査されている場合は?
**A4**: 「バックアップ権限で他の認証情報を取り、その後通常ログオンする」へ転換する。C:\Windows\System32\config\SAM+SYSTEM をバックアップしてローカルアカウントを取得、LSA secrets をバックアップ、スケジュールタスクが保存する認証情報ファイルをバックアップ。またはレジストリに保存された自動ログオン認証情報を読む。

### DA を取得した後、対象機へ隠密にログオンして flag を取る
**Q1**: DA を取得した。対象マシンに flag がある。読み取りをどう実行する?WMI/WinRM/SMB/RDP のどれ?
**A1**: アラート等級で並べる。(1)WMI(wmiexec/`Invoke-WmiMethod`)は挙動が一般的で、ログは 4688+ネットワークログ。(2)WinRM(evil-winrm/PSSession)——最も「公式」のリモート管理で、環境が既に WinRM を使っていれば日常に紛れる。(3)SMB でリモート C$ からファイルを読む(exec せず、読み取りのみ)——最も静か。(4)PsExec——ドライバ+サービスを落とし、最も音が大きい。(5)RDP 対話——マウスキーボードの痕跡が重い。判定基準:flag ファイルを読むだけなら SMB の `\\対象\C$\...` を優先し、exec しなければアラート連鎖が起きない。
**Q2**: flag ファイルのあるディレクトリ ACL が自分(DA でも)を拒否する?
**A2**: DA はデフォルトで NTFS の大部分の ACL を迂回できるが、「拒否 ACE」で明示的に遮られることがある。`SeTakeOwnershipPrivilege` を加えて所有権を奪うか、`SeBackupPrivilege` で ACL を迂回する(icacls+PS、または `Set-Acl` の前に takeown)。判定基準:DA が拒否されても終点ではない。特権昇格後は読める。
**Q3**: 対象機に EFS 暗号化または BitLocker があり、読んだファイルが暗号文?
**A3**: EFS 暗号化なら、そのユーザー/DA 身元で対象へログオンして(純リモート SMB ではなく)EFS 復号させる。BitLocker なら、通常マシン実行中は透過復号されるため読みに影響しない。オフライン場面は別。
**Q4**: 対象機への DA リモート接続がネットワーク ACL で遮られた(例: ある VLAN)。この経路をどう転換する?
**A4**: 「GPO で対象機から逆に接続してくるタスクを配る」直交次元へ転換する。DA は GPO を変更できる。対象機へスケジュールタスク/起動スクリプトを配り、自分の方へ逆接続させる。対象機への入站接続が不要。または WMI イベントサブスクリプションで対象機に展開する。
**Q5**: 対象機がドメインにない(独立機)?
**A5**: DA 権限は無効。ローカル認証情報へ切り替える。ドメインアカウントからそのローカル管理者 hash やパスワードを取得済みなら(共通パスワード/LAPS 漏えい/RDP 認証情報取得)、直接 PtH。そうでなければ他の攻撃面(web/サービス/物理)を探す。

### 対象機へ着地後、素早く flag を探す
**Q1**: 対象機へログオン/リモートした。flag パスが不明。まずどこを探す?
**A1**: 業務種別で走査する。(1)`C:\Users\<全ユーザー>\Desktop|Documents|Downloads`——CTF/HW でよくある。(2)`C:\` ルート+`C:\Flag*`。(3)Web ルート `C:\inetpub\wwwroot`、`C:\phpstudy` など。(4)データベースフォルダ。(5)`%USERPROFILE%\AppData` のブラウザブックマーク/メールキャッシュ。コマンド:`dir C:\ /s /b | findstr /i "flag"`。
**Q2**: 全ディスクで flag を探すのが遅すぎる。もっと賢いフィルタは?
**A2**: `dir /a-d /o-d /t:w /s C:\Users\*.txt` で最終書き込み時刻順に並べるか、最近開かれたファイルを探す `Get-ChildItem -Recurse | Sort-Object LastAccessTime`。flag は配置者が最近作成したと仮定し、タイムスタンプが最良のフィルタになる。
**Q3**: flag がファイルではなく環境変数/レジストリかもしれない?
**A3**: `set` で環境変数、`reg query HKLM /f flag /s /d` で全レジストリ検索、`schtasks /query /fo LIST /v | findstr /i flag` でスケジュールタスク、`Get-ItemProperty HKLM:\SOFTWARE\...` でソフトウェア設定を走査する。
**Q4**: 全部探してもない。位置を間違えたか?
**A4**: 「題意/業務ロジックを読む」へ転換する。flag は (1)別マシンのサービス上にあり、現在のマシンは踏み台に過ぎない。(2)「合成された認証情報」で、レジストリ+ブラウザ認証情報から組み立てる必要がある。(3)Docker コンテナ/WSL サブシステム内(`docker.exe`、`wsl` を探す)。判定基準:全ディスク走査で無 = 位置が誤り。題意を見直す。

### ドメインファイル共有と SYSVOL で認証情報/flag を探す
**Q1**: すぐ DA を打ちたくない。まずドメイン共有内で価値あるものを走査したい。何を走査する?
**A1**: 三大対象。(1)SYSVOL 共有(全ドメインユーザーが読める)内の Group Policy Preferences XML に暗号化 cpassword がありうる。(2)全域共有の `Find-InterestingDomainShareFile`(PowerView)。キーワード flag/password/pass/secret/backup/*.kdbx。(3)よくある管理者遺留:`\\server\share\backup\*`。
**Q2**: GPP cpassword をどう復号する?
**A2**: `Get-GPPPassword`(PowerView)または `gpp-decrypt`(Kali)。AES 鍵はマイクロソフトが公開しており、cpassword を取得すれば即平文。判定基準:2014 年以前の旧ドメインは cpassword を残しやすい。新ドメインは修正済みだが、履歴バックアップに残る場合がある。
**Q3**: 共有が多すぎて走査しきれない?
**A3**: `ShareFinder` で全共有を列挙し、`Invoke-ShareFinder`+「非デフォルト+読み取り可」フィルタで絞る。デフォルト C$/ADMIN$/IPC$ はスキップし、命名の怪しい共有("Backup"、"IT"、"Public" など)を重点的に見る。
**Q4**: 共有読み取り権限が過剰に締められ、一般ユーザーが何も読めない。どう転換する?
**A4**: 「SPN/ADCS/CVE 経路で直接権限昇格する」へ転換し、共有走査をスキップする。共有が空 = SysAdmin が締めたが、他の攻撃面(例: ADCS 設定)は逆に忘れている可能性がある。

### LAPS パスワード読み取り経路
**Q1**: ドメインで LAPS が有効。対象機のローカル管理者パスワードをどう読む?
**A1**: LAPS パスワードはコンピュータオブジェクトの `ms-Mcs-AdmPwd` 属性にあり、読み取り権限は通常 IT グループに付与される。ACL を調べる:`Get-DomainObjectAcl -SearchBase "OU=..." -ResolveGUIDs | ?{$_.ObjectAceType -like "*ms-Mcs-AdmPwd*"}`。判定基準:読み取り権限主体を見つけて乗っ取る = 全域のローカル管理者パスワードを取得。
**Q2**: 現在のアカウントに ms-Mcs-AdmPwd 読み取り権限がない?
**A2**: 三つの転換。(1)読み取り権限を持つアカウントを ACL チェーンで乗っ取る。(2)あるマシンのマシンアカウントを使う(それは自分の AdmPwd に読み取り権限を持つ)——そのマシンを制御している場合。(3)Windows LAPS(新版)は別属性 `msLAPS-Password` に保存する。個別に確認する。
**Q3**: ローカル管理者パスワードを取得後、どう DCSync に使う?
**A3**: ローカル管理者自体は DCSync できない。しかしそれで対象機へログオン後、LSASS 内のドメイン認証情報、DPAPI などを取得できる。判定基準:LAPS パスワードは横展開ツールであり、直接の DCSync の鍵ではない。チェーンの一環である。
**Q4**: LAPS パスワードフィールドが空?
**A4**: そのマシンが LAPS 未有効/パスワード未ローテーション/マシンがドメイン離脱の可能性。非 LAPS マシンのローカル認証情報(SAM)を読むか、他の情報源を探す方向へ転換する。

### DPAPI マスターキーでブラウザ/RDP 認証情報を復号して flag の手がかりを探す
**Q1**: ユーザー機でセッションを取得した。DPAPI に何か有用なものがあるか?
**A1**: DPAPI が保護するもの:(1)ブラウザ保存のパスワード/cookie(Chrome/Edge)。(2)RDP 接続ファイル内の暗号化パスワード(`*.rdg`、`Credential Manager`)。(3)Wi-Fi パスワード。(4)VPN パスワード。(5)一部アプリのカスタム認証情報。対象アカウントがどこかの認証情報で flag を隠したアセットへログオンしている可能性がある。
**Q2**: DPAPI をどう復号する?
**A2**: 現在のユーザーセッション下で `sekurlsa::dpapi`+`dpapi::masterkey` を使い、現在のユーザーコンテキストで自動復号する。オフライン:`%APPDATA%\Microsoft\Protect\<SID>\` の masterkey ファイル+ユーザーパスワード/hash を持ち帰り、mimikatz の `dpapi::masterkey /in:... /sid:... /password:...` でオフライン復号する。
**Q3**: ユーザーパスワードが不明?
**A3**: ドメインバックアップ鍵を使う(ドメインは全ユーザーの DPAPI マスターキーを DC へ自動バックアップする)。DA は `lsadump::backupkeys` でドメイン DPAPI バックアップ RSA 秘密鍵をエクスポートでき、一つの鍵で全ユーザーの全マスターキーを復号する。判定基準:ドメイン制御 = 全域 DPAPI 制御。
**Q4**: 対象ユーザーが本機に一度もログオンしていない(純サービスアカウント機)。DPAPI に何もない?
**A4**: 「サーバー」ではなく「ユーザー機」へ転換して DPAPI を探す。開発者/管理者の日常マシンこそ DPAPI の宝庫。またはデータベース/メールなどの業務システムへ転換する。

### 非制約委任ホストの乗っ取り
**Q1**: BloodHound が、ある一般サーバーに非制約委任(TrustedForDelegation)が有効と表示した。何ができる?
**A1**: 非制約委任マシン:任意のユーザーがそれへ認証すると、そのユーザーの転送可能 TGT を取得してメモリに保存する。DA/DC$ を認証させられれば、DA の TGT を取得して DCSync/横展開できる。判定基準:非制約委任機 + DC 認証を強制できる(coerce)= 黄金チェーン。
**Q2**: DC に認証させるには?
**A2**: 制御済み委任機上で SpoolSample/PetitPotam を DC$ に向けて実行し、DC に自分の SMB へ認証させる。この認証が Kerberos 形式で委任機へ戻り、委任機メモリに DC$ の TGT が入る。mimikatz の `sekurlsa::tickets /export` でエクスポートする。
**Q3**: DC$ の TGT を取得後?
**A3**: DC$ は自分に複製権を持つ → DC$ TGT で直接 dcsync。または TGT を renew して後続操作に使う。
**Q4**: 非制約委任機に admin 権限がなく入れない?
**A4**: 「先にその委任機へ横展開する」をサブ目標として転換する。または RBCD/制約付き委任経路へ切り替える。判定基準:非制約委任は宝だが、マシン上の SYSTEM/admin が要る。手に入らないからと言って諦めない。
**Q5**: ドメインに非制約委任機がない。どう転換する?
**A5**: 制約付き委任 S4U チェーン、RBCD、ADCS ESC などへ転換する。非制約委任は 2016+ で無効化が推奨される歴史的特徴であり、現代のドメインにはない場合がある。

### 制約付き委任 S4U チェーンで DA を取る
**Q1**: あるサービスアカウントに `msDS-AllowedToDelegateTo`(制約付き委任)が設定されており、自分がそれを制御している。どう使う?
**A1**: S4U2Self+S4U2Proxy。そのサービスアカウント身元で、「任意のユーザー」をクライアントとして指定のバックエンドサービス(委任対象)へアクセスする TGS をリクエストする。impacket getST.py または Rubeus s4u。委任対象に CIFS/DC が含まれれば、任意ユーザーとして DC ファイルを読める。
**Q2**: 委任対象があるマシンの CIFS だけ。他サービスへ拡大できるか?
**A2**: できる。制約付き委任設定が「Kerberos のみ」をチェックしていなければ、プロトコル変換を許可する(S4U2Self は対象ユーザーパスワード不要)。また委任 service 制限は「対象マシン+サービス種別」だが、多くの環境はマシン全体を書いて具体的 service を書かない → そのマシンの全 SPN を target にできる。
**Q3**: 制約付き委任対象が DC でなく、DC 上のものを取れない?
**A3**: 「委任対象機へ横展開し、そのマシンから二次委任や認証情報を探す」へ転換する。委任チェーンは区間ごとに飛ぶ。各区間で新たな DA 関連認証情報を取得できる可能性がある。
**Q4**: 対象アカウントが `Sensitive` または Protected Users で、委任が無効?
**A4**: その保護は S4U2Self を `Administrator` など特権アカウントに対して失敗させる。非機微な高権限アカウント(機微フラグを付けていないカスタム DA ユーザーなど)へ target を変えるか、シャドウ認証情報/RBCD など他経路へ転換する。

### Zerologon (CVE-2020-1472) の判断と利用
**Q1**: ドメインコントローラの OS バージョンが古い。Zerologon を試したい。まず判定基準は?
**A1**: `zerologon_tester.py` または Nmap スクリプトで探索する。原理:Netlogon の AES-CFB8 の IV が 0 であることを利用し、平均 256 回の試行で DC マシンアカウントパスワードを空にできる。判定基準:2020 年 8 月のパッチを適用した DC は免疫。古いパッチレベルの DC は命中しうる。
**Q2**: 利用が成功するとどうなる。直接 DCSync できる?
**A2**: DC$ のパスワードを AD 内で空にし、その後空パスワードで DC$ 身元として DCSync を発起する(DC$ は自身に複製権を持つ)。一手で krbtgt に到達。
**Q3**: 打った後、必ず DC$ パスワードを復元しないとどうなる?
**A3**: DC と他 DC の複製が失敗し、ドメインサービスが不安定になり、SOC が必ず調査する。復元:DCSync で出した DC$ の元 hash を `Reset-ComputerMachinePassword` または直接 RPC で元 hash を書き戻す。判定基準:Zerologon の利用+復元はセットの動作。復元しないとドメインが爆発する。
**Q4**: Zerologon にパッチ済みの場合は?
**A4**: noPac/PetitPotam+ADCS/ADCS ESC など他の一撃必殺 CVE へ転換する。または標準の ACL/委任経路へ進む。判定基準:CVE 経路は宝くじ。パッチ済みなら換える。磨かない。

### noPac (sAMAccountName spoofing CVE-2021-42287/42278)
**Q1**: パッチレベルが 2021 年秋前の DC に見える。noPac を試せるか?
**A1**: できる。前提:任意のドメインユーザー + MAQ>0 がマシン追加を許可。原理:マシンアカウントを追加後、sAMAccountName を DC 名($ を外す)に変更し、それで TGT をリクエストし、その後自分の名前に戻し、S4U2Self で DC 身元として DA 用 TGT を取る。判定基準:2021-11 以前のパッチ前なら一発で DA。
**Q2**: MAQ=0 またはマシン追加できない場合は?
**A2**: パスワード既知の既存マシンアカウント/computer object を使う。sAMAccountName を変更できればよい。または SPN アカウントの name を変更する(対応する書き込み権限が必要)。
**Q3**: 打ったが TGS_REQ が拒否された?
**A3**: パッチ適用済み、または KDC 署名保護が有効化済み。他 CVE または標準経路へ転換する。
**Q4**: DA チケットを取得後、どう DCSync に変える?
**A4**: 直接 DA 身元で dcsync して krbtgt を取り、長期永続化する。または他高権限アカウント hash を取る。

### flag を取った後の痕跡と仕上げ
**Q1**: flag を取った。これで終わりか?
**A1**: 競技/評価場面ではまだ必要かも:(1)証明の保持(スクリーンショット/ファイル hash)。(2)後続テストへの影響を避ける重要痕跡の清掃(例: 一時付与した DCSync ACL、加入した admin グループ、ゴールデンチケットファイル)。(3)レポート用に攻撃経路を記録。判定基準:flag 取得 = 技術目標達成だが、フローにはまだ仕上げがある。
**Q2**: 清掃はどこまで?
**A2**: 最低限復元する。(1)自分が追加したドメインオブジェクト ACL(Remove-DomainObjectAcl で)。(2)加入したグループ(Remove-DomainGroupMember)。(3)自作マシンアカウント/証明書(Remove-ADObject または証明書 revoke)。(4)mimikatz/ツールの着地ファイルを `del`。全量ログ抹消はしない(逆に強いアラートを誘発)。
**Q3**: 競技が「隠密性スコア」を要求する。ゴールデンチケットと骨チケットは残すべきか?
**A3**: ゴールデンチケットと DSRM パスワードなどの永続化バックドアは、評価終了まで残す価値がある(DA パスワード輪換への対処)。ただし生成過程の着地ファイル(krbtgt.kirbi など)は清掃する。判定基準:バックドアは残せるが、痕跡は消す。
**Q4**: 時間がなく清掃しきれない。優先順位は?
**A4**: 「高採点の痕跡」を優先して消す。(1)非デフォルト ACE(手動監査で見つかりやすい)。(2)非デフォルトグループメンバー。(3)大きな着地ファイル。(4)mimikatz プロセス。ログ類の 4624/4662/5136 は消さない。逆に対照アラートを誘発する。

### クロスフォレスト/クロスドメイン信頼で flag を取る
**Q1**: 子ドメイン DA を制御したが、flag は親ドメインまたは別フォレストにある。どうする?
**A1**: 信頼種別を判断する。(1)子親ドメイン:自動双方向・推移的。子ドメイン DA の信頼鍵 `sekurlsa::trust` でクロスドメインゴールデンチケットを作る(Enterprise Admins の SID History 身元で)ことで親ドメインへ行く。(2)フォレスト間信頼:デフォルトで SID history をフィルタし、跨ぎにくい。脆弱性を探す必要がある。判定基準:同一フォレスト子ドメイン = 一手で跨ぐ。クロスフォレスト = 複雑。
**Q2**: クロスドメインゴールデンチケットの重要パラメータは?
**A2**: 子親ドメイン間の信頼鍵(`sekurlsa::trust` で `Inter-Realm Key` をエクスポート)、`kerberos::golden` に `/sids:S-1-5-21-...-519`(親ドメイン Enterprise Admins の SID)を加える。判定基準:SID history はクロスドメイン権限伝播の鍵である。
**Q3**: クロスフォレスト信頼で SID filtering が有効で SID history が使えない。どう転換する?
**A3**: 「信頼自体の脆弱性を探す」へ転換する。クロスフォレストゴールデンチケット(CVE-2020-0665 類)、TrustAccount$ 認証情報悪用、または親フォレストユーザーが子フォレストマシンへ委任済みの権限から逆に打つ。または「信頼双方が共同管理するアセットを制御する」迂回。
**Q4**: 全く別ドメインで信頼がない。どう転換する?
**A4**: 純粋なネットワーク層の攻撃面へ転換する。別ドメイン = 別認証情報体系。サービス/CVE/認証情報再利用から打ち込むしかなく、AD とは無関係。判定基準:信頼なし = AD 問題ではなく、外網/横展開問題。

### DCSync 完了だが取得した hash が使えない(鍵が誤り)
**Q1**: DCSync で `krbtgt` を出した。ゴールデンチケットを作るとマシンが無効と言う。原因は?
**A1**: よくある原因:(1)krbtgt が最近ローテーションされ、取得したのは実は履歴 hash("krbtgt password history")。NTLM history の最初を見る必要がある。(2)ドメインが AES 強制で、NTLM ゴールデンチケットが拒否。AES256 鍵で作るべき。(3)ドメイン名または SID の誤り。判定基準:ゴールデンチケット発行はエラーを出さないが、注入後にアクセスが拒否される → hash 対応関係が誤り。
**Q2**: 現在有効な krbtgt hash を取得したことをどう確認する?
**A2**: secretsdump 出力の "pwdLastSet" を見るか、異なる時刻の二回同期が一致するかを比較する。二回が違えば、その間にローテーションが起きた。または DC 上で `Get-ADUser krbtgt -Properties PasswordLastSet` でパスワード変更時刻を見る。
**Q3**: 取得したのが AES key で NTLM でない。どうチケットを作る?
**A3**: mimikatz ゴールデンチケットは AES 対応:`kerberos::golden /aes256:<key>` で `/rc4:<ntlm>` を代替する。現代のドメインは RC4 を無効化すると AES 必須になる。
**Q4**: 対象環境が RC4 を完全に無効化し、AES も取れない(ツールが dump しなかった)。どう転換する?
**A4**: Kerberos keys を含めて再同期する(`secretsdump -just-dc-user krbtgt` はデフォルトで全 key を含む)。またはゴールデンチケットを諦め、「DA の本物 hash で PtT/PtH」する経路へ進む。

### 全工程を終えた後、次回の DCSync のために永続化する
**Q1**: flag を取ったが環境はまだ使う。繰り返し DCSync できる入口を残したい。最も省力な方法は?
**A1**: 三階層の永続化。(1)ゴールデンチケット(krbtgt hash 在手で任意身元・任意時間長。最も「高次元」だが検知手法も多い)。(2)自作ドメインアカウント+ DCSync ACL 追加(監査で見つかりやすい)。(3)AdminSDHolder ACL 変更(毎時全保護オブジェクトへ刷り戻し、影響面が広く、極めて隠密だが極めて高危険)。判定基準:競技場はゴールデンチケット、実戦は AdminSDHolder 寄り。
**Q2**: ゴールデンチケットの有効期間をどう合理的に設定する?
**A2**: デフォルト 10 年は検知特徴。10 時間、またはドメイン最長チケットポリシー(通常 8〜10 時間)と一致させる。判定基準:正常なチケットトラフィックに紛れる。
**Q3**: krbtgt が能動的にローテーションされた(二回)ためゴールデンチケットが無効。どう補救する?
**A3**: まだ元の DCSync 能力があるなら(あるアカウントが複製権を保持)、再同期して新しい hash を取る。複製権を失っていれば「現在の足場から再び DA へ権限昇格する」古い経路へ戻る。判定基準:ゴールデンチケット永続化は永久ではない。バックアップ経路が要る。
**Q4**: ブルーチームの応答能力が強く、どんな永続化も消される。完全に諦めるべきか?
**A4**: 「権限系」ではなく「情報系」の永続化へ転換する。全域 NTDS hash+GPP パスワード+DPAPI バックアップをローカルへ取得しておけば、追い出されても、これらの認証情報で将来のパスワード輪換周期外でも再侵入できる。判定基準:認証情報は最も失効させにくい永続化である。
