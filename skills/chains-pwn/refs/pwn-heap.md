# リバースエンジニアリング PWN · ヒープ脆弱性(UAF/オーバーフロー)の判断

### バイナリを入手したらまず「この問題はヒープ問題か」を判断する
**Q1**: 手元に ELF があり、メニュー型の対話。どの利用ルートかまだわからない。第一歩でどう素早く定性する?
**A1**: まず `checksec` + `file` + `strings | grep -i "gnu libc"` を実行する。次に `nm/objdump` で `malloc/free/calloc/realloc` の参照と、インポート表の `__libc_start_main` を見る。メニュー項目に "add/create/new" と "delete/free/remove" が含まれていれば、ヒープ問題テンプレートの可能性が高い。
**Q2**: 明確な add/delete の文字が無く、大きな buffer のループ読み書きが 1 つあるだけなら、ヒープ問題ではない?
**A2**: 必ずしもそうではない。`malloc(user_controlled_size)` があるかを見る。メニューが switch に隠れ、項目番号が数字のこともある。IDA で main を逆コンパイルし、ループ+switch+`malloc/free` の組み合わせがあるかを見る。これこそが判定基準。
**Q3**: ヒープ問題と判断した後、次に何をすべきか?
**A3**: 直ちに libc バージョンを特定する——`ldd ./bin` か、問題添付の `libc.so.6` で `strings | grep "GNU C Library"` を使う。libc バージョンが以降のすべての利用手法(tcache の safe-linking 有無、hook 削除有無)を決める。
**Q4**: この方針が通らない——バイナリが libc と全くリンクされておらず、静的コンパイルの musl/uclibc の場合、どうする?
**A4**: 直交次元へ切り替える:静的コンパイルはヒープマネージャが musl の mallocng/oldmalloc、あるいは自前実装の allocator である可能性を意味する。glibc の手法を当てはめず、まず IDA で malloc の実装を見て「未知の allocator のリバースエンジニアリング」として扱う。musl mallocng には専用の攻撃面(meta 構造)があるので、別に進める。
**Q5**: 逆アセンブル時にメニュー関数の構造が奇妙で、暗号化/難読化があるように見える。深掘りする価値は?
**A5**: 最初に難読化で消耗する価値はない。まず「脆弱性点」と「利用プリミティブ」の有無を確認する——難読化がどれほど重くても、脆弱性ロジックは最終的に malloc/free/memcpy の近くに現れる。動的追跡(gdb で malloc/free にブレークポイントを張る)の方がむしろ速い。

### glibc バージョン識別が一連の手法を決める
**Q1**: ヒープ問題と確認し、libc を入手したが、どの馴染みのブランチにもバージョンが一致しない。まず何を見る?
**A1**: 4 つの分水嶺を見る:①tcache(2.26+ で導入);②tcache safe-linking(2.32+);③__free_hook/__malloc_hook の存在(2.34 で削除);④`_IO_wfile_jumps` 検査(2.24+ の vtable check)。この 4 点で libc を「古・中・新・超新」の 4 段階に分類する。
**Q2**: 2.34+ と判定した後、通常の hook 手法は使えなくなる。主方針をどう調整する?
**A2**: FSOP(File Stream Oriented Programming)、`_IO_list_all` 攻撃、`__exit_funcs` の改ざん、または(スタックをリークできれば)スタック上の戻りアドレス直接書き換えへ方向転換する。核心思想:hook が無ければ「プログラム終了前に必ず通る間接呼び出し点」を探す。
**Q3**: libc バージョンを誤判定するとどうなる?
**A3**: 結果は深刻——safe-linking の XOR アルゴリズムは 2.32+ でのみ有効で、バージョン誤判定により tcache poisoning の計算アドレスが誤り、偽チャンクが通らず abort を起こす。判定基準:先にローカルで同バージョンの docker を起動するか patchelf で libc を置き換えて通してからリモートを叩く。
**Q4**: リモート問題が libc ファイルを渡さず、ip:port しか渡さない。どうする?
**A4**: 「リークでバージョン判定」へ方向転換する:まず libc 関数アドレスを 1 つリークし、低 12 ビットオフセットを取得して libc.rip / libc-database で具体的なバージョンを逆引きするか、puts/printf 実装末尾の命令パターンを見る。バージョン確定前に二段目を性急に攻撃しない。
**Q5**: 入手した libc がパッチ済みのカスタム版の場合?
**A5**: 次元を変える——CTF の手法を無理に当てはめない。IDA で `__malloc_hook`、`_IO_2_1_stdout_` などのキーシンボル周辺に検証が追加されていないか見る。必要なら malloc/free/exit の重要パスを逆コンパイルし、カスタム版を「半未知の allocator」として扱う。

### メニューの四プリミティブ(add/delete/edit/show)の素早い特定
**Q1**: ヒープ問題を IDA で開くと sub_XXX だらけ。追加・削除・変更・照会を最速で特定するには?
**A1**: 特徴を掴む:add には必ず `malloc/calloc`、delete には必ず `free`、edit には `read/fgets` かつ配列インデックスで始まる、show には `puts/printf/write` でヒープ内容を出力する。相互参照でメニューの dispatcher までさかのぼり、4 つの関数を一度に特定する。
**Q2**: show 関数が存在しない、または削除されている場合、何を意味する?
**A2**: 直接の leak プリミティブが無いことを意味する——これが問題の難度に関する核心の示唆。方向転換:①UAF 後に同サイズを malloc し、unsorted bin の残留 fd/bk を何らかの tcache エントリに落として add 時に読み出す。②オーバーフローでまだ解放されていないチャンクの size を上書きし、間接的に leak を構成する。
**Q3**: 逆アセンブルで add の size がハードコード定数でユーザー入力でないのを発見した。どうする?
**A3**: チャンクサイズが固定されていることを意味する。この size がどの bin に落ちるか見る(<0x90 は tcache/fastbin;0x90-0x400 は small;>0x400 は large)。サイズ固定はむしろ手法空間を狭める——その bin の定番利用手法に直接進む。
**Q4**: edit に長さパラメータがあるが、厳密な境界チェックがあるように見える。オーバーフローは無い?
**A4**: 結論を急がない。確認する:①境界比較が符号付きか符号なしか(符号付きなら負数を渡せる);②長さ vs 実際のチャンクサイズが off-by-one か;③strcpy/strcat など長さを見ない関数を使うか。多くの問題のオーバーフローは「チェックがあるように見える」中に隠れている。
**Q5**: メニューに見慣れない第 5、第 6 の項目(gift/backdoor/debug など)が現れた。どうする?
**A5**: 優先して見る!出題者が加えた非常規項目の 90% はキープリミティブ——任意読み取り、任意書き込み、または直接 libc リークを渡してくる可能性がある。まずその入力制約と副作用を確認してから利用方針を決める。
**Q6**: バイナリ全体にメニューが無く、http サーバー/複雑な業務ロジックの場合?
**A6**: 次元を変える:「メニュー問題」の発想から「ロジック脆弱性発掘」の発想へ切り替える。業務内の alloc/free のペアパスを探し、状態機械図を描き、「ある状態で free されたが参照が残る」パスを探す。このとき逆アセンブル作業量は大幅に増えるので、AFL/libfuzzer で特定を補助することを検討する。

### UAF なのかオーバーフローなのかを判断する
**Q1**: ヒープ脆弱性があると確定したが、具体的に UAF かオーバーフローか不明。どう定性する?
**A1**: delete 関数を見る——解放後にポインタを NULL にしているか/インデックス配列をクリアしているか。していない=UAF/double-free の可能性。している=オーバーフローか off-by-one の可能性が高い。同時に edit が許す書き込み長と割り当て size が厳密に等しいかを見る。
**Q2**: 両方の脆弱性がある場合、どちらを先に攻める?
**A2**: UAF を優先する——通常 UAF は fd ポインタの書き換え能力を直接得られるので、プリミティブがより強い。オーバーフローは何バイト溢れるかを見る必要があり、off-by-one や off-by-null は精密なヒープレイアウトを要する。UAF が tcache double-free 検査に阻まれた場合のみ、オーバーフローに戻る。
**Q3**: UAF だが free 後に leak show プリミティブが無く、判定難度が急上昇する。どうする?
**A3**: 「UAF でオーバーフローを作る」へ方向転換する:たとえば UAF でチャンクの size を上書きし、次回 free 時に誤って大きなチャンクへ結合させ、overlapping chunks を構成して任意読み書きを得る。UAF は「メタプリミティブ」であり、定番の tcache poisoning に限定しない。
**Q4**: オーバーフローが 1 バイトだけ(off-by-one)の場合、判定基準をどう定める?
**A4**: 溢れるバイトがちょうど次チャンクの size に落ちるか、また null-byte オーバーフロー(off-by-null)か任意値オーバーフローかを見る。null-byte オーバーフローは poison null byte / house of einherjar に進む。任意値オーバーフローは unlink / chunk overlap に進む。
**Q5**: 標準的な UAF でもオーバーフローでもなく「double free」の場合?
**A5**: 2.29+ には tcache double-free 検査(key フィールド)がある。判定基準:1 回 free すると chunk[8] の位置に tcache_perthread ポインタが書かれる。2 回目の free の前にこの key を上書きする方法を考える。これは「double-free 手法から tcache dup 手法へ転換」の合図。
**Q6**: 一般的な脆弱性をすべて確認しても見つからない場合、カスタム構造上のロジック穴の可能性?
**A6**: 次元を変える:問題自身が定義する chunk header を見る——多くの問題は malloc した buffer 内に size/next/isused などのフィールドを置き、業務コードの「同一 index の再利用」が論理 UAF になる。この種の問題は libc の常識に頼れず、データ構造を逆アセンブルで明確にする必要がある。

### show プリミティブが無い場合の libc リーク
**Q1**: UAF/オーバーフローのプリミティブがあるが show が無く、ヒープ内容を直接 print できない。どうリークする?
**A1**: 定番の手順——unsorted bin の fd/bk が main_arena+0x60 付近を指す特性を利用し、unsorted bin チャンクの一部内容を「まだ print できる場所」へ滲み出させる。方法:業務フィールド(たとえば name)とチャンク内容を overlap させる。
**Q2**: チャンクサイズがすべて <0x90 で tcache/fastbin にしか入らず、unsorted bin に入れない場合は?
**A2**: 「人為的に大きなチャンクを作る」へ方向転換する:①連続して 7 個 malloc して tcache を埋めてから 8 個目を free すると unsorted に入る。②オーバーフローで size フィールドを書き換えて小チャンクを大チャンクに偽装してから free する。これが show 無し問題の基本。
**Q3**: 上記の手が全て使えず、malloc 回数が非常に少ない制限がある場合?
**A3**: 次元を変える:libc をリークせず直接攻める——たとえば partial overwrite で低 2 バイトだけ書き換え、高位をブルートフォース(2^4=16 回以内、1/16 の確率で one_gadget に命中)、または既知位置へのスタックピボットで shellcode を使う。「必ず leak が必要」という固定観念を捨てる。
**Q4**: 「partial overwrite できるか」の判断のキー条件は?
**A4**: ①ターゲットアドレスと既知アドレスの高位が揃っている(ともに libc 内、またはともに heap 内)。②制御可能な書き込み長がバイト単位で正確。③ブルートフォース回数が許容できる(リモート 1/16 確率、数分で通る)。3 つ揃って初めて進む価値がある。
**Q5**: stdout があるが puts がヒープ内容を出力しない。_IO_2_1_stdout_ の flags を書き換えてリークできる?
**A5**: house of orange の FSOP leak を使える——`_IO_2_1_stdout_` の `_IO_write_base` を書き換えて、次回の puts がより早い位置から出力を始めるようにし、後ろの libc ポインタを追加でリークする。前提:2.28-2.33 の間のバージョンで、vtable check は追加済みだが stdout 構造は未変更。
**Q6**: サンドボックスがすべての出力すら無効化し、alloc/free しか見えない場合?
**A6**: 次元を変える:サイドチャネルに進む——alloc の成否(SIGSEGV/タイムアウト)、特定の abort メッセージのトリガー有無でメモリ内容を oracle する。これは極端な難問の手法で、通常の問題では考慮不要。

### edit プリミティブが無い困難
**Q1**: add/delete/show はあるが edit が無い(全体を 1 回だけ書き込める)。どう攻める?
**A1**: 核心思想:「free 自体の書き込み能力」を利用する。tcache/fastbin の free は fd ポインタをチャンクヘッダに書く。これが暗黙の edit。UAF 後に同サイズのチャンクを add し、add 時の初期化書き込みで fd の制御を完了する。
**Q2**: add が malloc でなく calloc の場合、追加の落とし穴がある?
**A2**: ある!calloc は tcache を通らず、fastbin/smallbin から直接取り、しかも memset でゼロクリアする。つまり tcache poisoning を calloc で取り出すことはできない。判定基準:add が calloc なら、ターゲットチャンクを unsorted か smallbin のパスに通す必要がある。
**Q3**: 上記が失敗——add が calloc かつ size が tcache しか許されない場合?
**A3**: 方向転換する:①先に tcache を埋め(同 size を 7 回 free)、8 回目を fastbin/unsorted に入れてから calloc で取り出す。②house of botcake など bin を跨ぐ手法に変える。「直接の tcache poison」を諦める。
**Q4**: add の size が毎回異なり、tcache を埋められない場合?
**A4**: できない——tcache は size でバケット分けされる。このとき問題が許す size 範囲を見て、1 つの size に集中して操作するか、オーバーフローでチャンク size を書き換えて異なるサイズを合流させる。
**Q5**: add の size に上限があるが負数にできる?
**A5**: size 変数の型を見る。符号付き int で負数を渡すと、malloc 内部で巨大な unsigned size_t として扱われて失敗し NULL を返す。ただし add 後に戻り値を検査しない場合、「構造内のポインタが NULL なのに使い続ける」空ポインタ書き込みプリミティブが生まれる。これは直交パス。
**Q6**: edit も UAF も無く、オーバーフローだけの場合、まだ攻められる?
**A6**: 攻められるが難しい。house of force(旧 libc)、house of spirit、またはオーバーフローで隣接チャンクの size を書き換えて free 時に誤った bin に入れる、へ方向転換する。overflow-only 問題の核心は「オーバーフローで free 時の挙動の歪みを作る」こと。

### size が tcache 範囲に固定される
**Q1**: 問題が size <0x80 しか許さず、全部 tcache を通り fastbin/unsorted に触れない。どう攻める?
**A1**: 慌てない。tcache poisoning 自体が最短パス:UAF 後に fd を書き換え → tcache ヘッドが任意アドレスを指す → 次回 malloc が直接それを返す。2.32+ は safe-linking が必要なので、fd に `(target >> 12) ^ target` の変換を忘れない。
**Q2**: しかし tcache poisoning には libc/heap のリークが必要で、size が全部 tcache だとどうリークする?
**A2**: 制限を破る:まず tcache を埋め(7 回 free)て 8 回目の free を unsorted に通す → libc ポインタが現れる。または埋めた後に 8 回目の free の前にオーバーフローで size を書き換えて bin を跨がせる。これが tcache 問題の汎用の突破口。
**Q3**: safe-linking 下で heap のリークは必須?
**A3**: 必須。fd はチャンクアドレスの高位と XOR されている——ターゲットアドレス T を書き込むには、チャンクアドレス P を知って `P ^ T` を書く必要がある。heap のリーク優先度を libc リークより前に上げる。
**Q4**: tcache poisoning を仕掛けたが malloc 時に abort した(アライメントチェック)。どうする?
**A4**: 2.32+ はチャンクのアライメント(0x10)を検査する。書き込むターゲットアドレスは 16 バイトアラインでないと abort する。判定基準:拒否されたならターゲットを前後に微調整してアライメント点を探す。通常 hook の前 8 バイトの「ゴミバイト」は書き込みを許容できる。
**Q5**: 書き換えたいターゲットが __free_hook だが libc 2.34+ で削除されている。等価なターゲットは?
**A5**: 方向転換する:①`_IO_list_all` → FSOP;②`stdout->vtable` → IO_wfile_jumps;③何らかの `__exit_funcs` 要素;④スタック上の戻りアドレス直接書き換え(追加のスタックリークが必要)。tcache poisoning は汎用武器なので、ターゲットは libc バージョンに合わせる。
**Q6**: 全部 tcache を通るが double-free 検査を越えられない?
**A6**: 次元を変える:まず tcache key を消す方法を考える——UAF 後に add を 1 回行えば key 位置に業務データが書かれ、再度 free すると回避できる。または tcache stashing(smallbin の回避)で double-free 検査を完全に回避する。

### free が 1 回しかできない制限
**Q1**: 問題が各チャンクを 1 回しか free できない(delete 後に NULL を設定)。UAF 手法が使えなくなった。どうする?
**A1**: オーバーフロー、house of einherjar / poison null byte へ方向転換し、オーバーフローで free 時のチャンク結合をトリガーして overlapping chunks を作る——効果は UAF と等価で、同一チャンクの 2 回 free は不要。
**Q2**: オーバーフローも null byte だけの場合、何ができる?
**A2**: poison null byte:次チャンクの size 低バイトを 0 に書き換え、prev_size 検証を無効化して free 時に誤結合させる。これが off-by-null の定番手法。前提は size の高位が制御可能で、prev_size フィールドが書き込み可能なこと。
**Q3**: 2.29+ が chunk size vs prev_size の整合性検査を追加し、poison null byte が直接 abort する。どうする?
**A3**: house of einherjar へ方向転換する:prev_size と PREV_INUSE を入念に構築して検査を通過させる。核心は先に heap ベースをリークし、prev_size = ターゲットチャンクまでの距離を偽造して、free 時に「消極的結合」を自分の制御する位置へ誘導する。
**Q4**: 1 回の free でも結合をトリガーできない(size が全部 tcache 内)場合?
**A4**: 次元を変える:①size を大きく書き換えて tcache 境界を跨ぐ(0x420 以上は直接 unsorted/large に入る)。②「ロジック脆弱性」ルートに進む——問題内に idx 境界問題があり、負インデックスでアクセスすべきでないチャンクにアクセスできるかもしれない。
**Q5**: 「UAF/double-free 手法を諦めるべきか」の判断の合図は?
**A5**: 3 つの合図のうち 1 つでも出れば方向転換する:①delete 後にポインタがゼロクリアされ、以後使用されない。②free 時にチャンクが memset でクリアされる。③index エントリが free 後に完全に破棄される。このとき重心をオーバーフロー/ロジック穴へ移す。

### tcache ポイズニング失敗(safe-linking 遮断)
**Q1**: 2.32+ で tcache poisoning を仕掛け、malloc 戻り後に abort した。どこが間違っている可能性が高い?
**A1**: safe-linking 検証の失敗。malloc 時に fd が指すチャンクアドレスが現在のチャンクとアライン整合するか検査する。判定基準:①ターゲットアドレスが 0x10 アラインであること。②書き込む fd が `pos ^ target` であり、生の target でないこと。③ブルートフォース成功前に正確な heap base が必要。
**Q2**: heap base をリークできず、12 ビットのブルートフォースしかできない。価値はある?
**A2**: ある。heap ベースの中間 3 nibble はランダムで、mmap 層のランダム化。リモートは 1/4096 から 1/16 の間(ブルートフォース戦略次第)で、数百秒で取れる。ただし優先して考えるべきは:unsorted bin の部分リークで heap アドレスを漏らせないか。
**Q3**: heap をリークしたが tcache poisoning を仕掛けてもまだクラッシュする。何が考えられる?
**A3**: 確認する:①チャンクアドレスの計算ミス(ヘッダの 0x10 オフセットを計算していない)。②ターゲットが 8 バイトアラインでない。③tcache count のオーバーフロー。gdb + pwndbg の `tcache` コマンドでリンクリストの状態を直接見て、頭で補完しない。
**Q4**: poisoning が繰り返し失敗して諦めようとしている。何に方向転換する?
**A4**: fastbin attack(size 0x20-0x80)、unsorted bin attack(main_arena+固定オフセットをターゲットへ任意書き込み)、または直接 large bin attack へ方向転換する。tcache は手法の 1 つにすぎず、一本やりにしない。
**Q5**: __free_hook 付近を叩いたが free トリガー後にシェルへジャンプしない。何が考えられる?
**A5**: ①hook 位置の計算ミス(libc バージョンごとにオフセットが異なる)。②hook 値の書き込みミス(system を書くべき所に malloc を書いた)。③free の引数が "/bin/sh" 文字列でない。確認:free(x) は hook(x) を呼ぶので、x が "/bin/sh" を指す必要がある、または one_gadget で引数不要にする。

### チャンクがどの bin に落ちるか判断する
**Q1**: 逆アセンブルで size を出した後、このチャンクが free 後にどの bin に落ちるか素早く判断するには?
**A1**: 4 つの閾値を覚える:size < 0x420(tcache);tcache 無しなら <=0x80 は fastbin;<=0x400 は smallbin;>=0x400 は largebin;どの bin にも一致しないときはまず unsorted に入る。注意:size は malloc 要求サイズではなく「ヘッダ込み」のチャンクサイズ。
**Q2**: malloc(0x18) を要求したときの実際のチャンクサイズは?
**A2**: `chunk_size = (request + 0x10 + 0x0f) & ~0x0f`、最小 0x20。よって malloc(0x18) → チャンク 0x20。判定基準は request_size ではなく chunk_size を使う。
**Q3**: tcache が満杯の場合、どう落ちる?
**A3**: tcache はバケットごとに最大 7 個。満杯後は同 size の free が fastbin(size <=0x80 の場合)か unsorted に進む。これが「unsorted を作る」核心テクニック:tcache を埋める → 8 回目の free が unsorted に入る → libc leak を取得する。
**Q4**: チャンクを large bin に入れるにはどれだけ必要?
**A4**: チャンク >= 0x400。かつ large bin はソートされ、最初の free は unsorted に入り、次の malloc 時に初めて large bin へ整理される。判定基準:先に大きなチャンクを free し、より大きなものを malloc して初めて整理がトリガーされる。
**Q5**: size がどうしても狭い範囲に固定され、大 bin 手法をトリガーしたい場合は?
**A5**: 次元を変える:オーバーフローか UAF で既に unsorted にあるチャンクの size を改ざんし、次回整理時に大チャンクとして large bin に置かせて large bin attack を仕掛ける。add の size を変えずに大 bin をトリガーする手法。

### __free_hook / __malloc_hook が削除された場合の対応
**Q1**: libc 2.34+ で hook が全削除された。tcache poisoning はどこを叩く?
**A1**: 優先順位リスト:①`_IO_list_all`(FSOP);②`stdout->vtable` + IO_wfile_jumps 手法;③`__exit_funcs`(exit か main からの return のトリガーが必要);④スタック上の戻りアドレス(スタックのリークが必要)。順に実現可能性を試す。
**Q2**: FSOP に必要な前提条件は?
**A2**: ①制御可能なデータを 1 区間任意書き込みできる(偽の IO_FILE を偽造)。②libc 内のある関数アドレス(たとえば system)を知っている。③プログラムが puts/exit を通って stream flush をトリガーする。3 つ揃って初めて攻められる。
**Q3**: プログラムが永遠に exit しない(死ループメニュー)場合、FSOP をどうトリガーする?
**A3**: 方向転換する:①例外を発生させて libc 内部に malloc_printerr を呼ばせる → stderr flush に進む → vtable 呼び出しをトリガーする。②`_dl_open_hook` などの runtime 構造を書き換える。③GOT の puts/printf を書き換える(Full RELRO でない場合)。
**Q4**: `_IO_list_all` を叩いた後 abort する。何を意味する?
**A4**: 2.24+ には vtable check がある——vtable は `__libc_IO_vtables` 区間内でなければならない。回避方法:①`_IO_wfile_jumps` や `_IO_str_jumps` などの正規 vtable + 入念なフィールド構築。②house of apple(2.35+ の主流手法)で `_wide_data->_wide_vtable` を乗っ取る。
**Q5**: すべての IO 手法が失敗したら、何に方向転換する?
**A5**: 次元を変える:largebin attack + tls_dtor_list、`stderr` の類似構造、または __environ を直接ブルートフォースしてスタックを探し ret を上書きする。IO はパスの 1 つにすぎず、粘り続けない。

### Full RELRO 下の対応
**Q1**: checksec が Full RELRO を示し、GOT が書き込み不可。手法に何の変化がある?
**A1**: GOT hijack を諦め、方向転換する:①libc 内 hook の書き換え(バージョンが許せば);②スタック上の戻りアドレス書き換え;③IO_FILE 攻撃。Full RELRO は 1 つの扉を閉じただけで、すべての扉を閉じたわけではない。
**Q2**: PIE + Full RELRO + リーク無し。まだ攻められる?
**A2**: 極めて難しいが攻められる。問題が stack cookie のリーク経路を渡しているか、partial overwrite で libc 内の低位だけを書き換えられるかを見る。3 つとも駄目ならロジック穴か他のサイドチャネルへ方向転換する。
**Q3**: PIE だが Full RELRO でない場合?
**A3**: GOT を攻められる。ただし GOT 攻撃の前に PIE ベースをリークする必要がある。判定基準:プログラムに puts(何らかの got 内アドレス)があるか、ヒープ内に PIE ポインタをリークするパスがあるか。
**Q4**: Full RELRO 下でスタックの戻りアドレスを書き換えたい。スタックをどう特定する?
**A4**: `environ` に進む——libc 内の `environ` はスタック上の環境変数配列を指すので、任意読み取りでそれを読めばスタックをリークできる。判定基準:tcache poisoning で environ の位置を叩き、add でスタックアドレスを読み出してから ret アドレスの位置を計算する。

### house of orange / large bin attack の選定
**Q1**: どのような場合に house of orange を検討する?
**A1**: 3 条件が同時に満たされる:①free プリミティブが無く malloc + オーバーフローのみ。②top chunk の size を書き換えられる。③libc バージョンが古い(2.24-2.28 の間が最適)。核心は top chunk の改ざんで「自動 free」をトリガーして unsorted に入れること。
**Q2**: 2.29+ が top chunk 検証を追加した。house of orange はまだ攻められる?
**A2**: 難しくなる。house of orange の変種へ方向転換する——偽の top を精密に構築して新 top も検証を通過させる。または house of orange を諦めて house of einherjar / botcake に進む。
**Q3**: large bin attack の核心プリミティブは?
**A3**: largebin チャンクの `bk_nextsize` を(ターゲットアドレス - 0x20)に書き換えると、次回 malloc が largebin 整理をトリガーした際に、ターゲット位置にチャンクアドレスが書き込まれる。「任意位置に heap ポインタを 1 つ書く」と等価。
**Q4**: この「heap ポインタの任意書き込み」で何ができる?
**A4**: `_IO_list_all` に書き込んでヒープ上の偽 IO_FILE を指させたり、`global_max_fast` に書き込んで全チャンクの free を fastbin に落として fastbin 攻撃面を最大化したりする。これはチェーンの一環であり終点ではない。
**Q5**: largebin をトリガーしたいが size が上がらない場合は?
**A5**: 次元を変える:先にオーバーフローで中チャンクの size を書き換えて largebin 範囲に入れ、整理をトリガーする。または tcache stashing へ転換する——こちらも「任意位置に heap アドレスを 1 つ書ける」が、要件がより低い。

### IO_FILE 攻撃チェーンの選定判断
**Q1**: FSOP に進むと決めたが、多種の IO 攻撃に直面してどう選ぶ?
**A1**: libc バージョンで選ぶ:①2.23:直接 vtable 書き換えで検査なし。②2.24-2.27:vtable check があるが hook が使えるので IO は代替。③2.28-2.33:hook + _IO_str_jumps / IO_wfile_jumps。④2.34+:house of apple 2 / house of some。
**Q2**: house of apple 2 に必要な条件は?
**A2**: ①大きな制御可能メモリ(少なくとも 0x100+ バイト)を任意書き込みできる。②libc と heap をリークする。③exit か puts をトリガーして stream flush に進む。核心思想:`_wide_data->_wide_vtable->__doallocate` を setcontext へ向けて乗っ取る。
**Q3**: setcontext 手法でシェルは取れるがサンドボックスが execve を禁じる場合は?
**A3**: ORW(open/read/write)チェーンへ方向転換する——setcontext の後にスタックをヒープへピボットし、ヒープ上に open/read/write flag の ROP チェーンを置く。サンドボックス検知は seccomp-tools で filter を事前に確認する。
**Q4**: IO 攻撃時に vtable check は通過したがジャンプしない。どういうこと?
**A4**: 偽 IO_FILE の flags、_lock、_mode などのフィールドを確認する。IO 内部には多数の暗黙検査がある(_lock が 0 でないことなど)。gdb でシングルステップしてどのフィールドが遮断したか見る。
**Q5**: すべての IO 手法を試したが駄目。他に何がある?
**A5**: 次元を変える:①`tls_dtor_list` 手法(tls のリークが必要);②`__exit_funcs` の unprotect(2.34+ はポインタ暗号化を追加したので追加リークが必要);③got の free/malloc 直接書き換え(Full RELRO でない場合)。

### libc ベースアドレスのリーク失敗時の自己救済
**Q1**: 手順通りに unsorted bin leak でアドレスを取ったが、libc ベースと合わない。どう切り分ける?
**A1**: まずこのアドレスが `main_arena+X` か確認する。main_arena は libc データセグメント末尾にあり、base から約 0x1e0000 のオフセット(バージョンで異なる)。`libc.symbols['main_arena']` か gdb 内の `p &main_arena` で正確なオフセットを取る。
**Q2**: リークした高位が常に 0x7f だが低位の揺らぎが大きい。どういうこと?
**A2**: 高位 0x7f は libc アドレスの特徴(64 ビット下で libc は通常 0x7f... 区間)。低位の揺らぎは ASLR。判定基準:複数回実行して高位が安定するか見る——安定していれば libc 内アドレス、不安定ならリークパスを再検討する。
**Q3**: リークした数字が libc でなく heap アドレスのように見える。どういう状況?
**A3**: チャンクがまだ unsorted を通っていない——tcache/fastbin にあり、fd が別のチャンクを指す。方向転換:tcache を埋めて次の free を unsorted に通すか、size を書き換えて強制的に bin を跨がせる。
**Q4**: リークパスが全く無い場合は?
**A4**: 次元を変える:①部分アドレスのブルートフォース(one_gadget の低 12 ビットはリーク不要)。②スタック上の残留 libc アドレス(`__libc_start_main+X` など)をリークできるか見る。③stdout の partial overwrite で puts により多くリークさせる。リークは unsorted の 1 ルートだけではない。
**Q5**: リークしたアドレスは合っているようだが one_gadget が機能しない。どういうこと?
**A5**: one_gadget は特定のレジスタ制約(`[rsp+0x30] == NULL` など)を満たす必要がある。トリガー点ごとに制約が異なる——free_hook トリガー時は rdi=チャンクで、条件を満たさないことがある。one_gadget の番号を変えるか、execve("/bin/sh", 0, 0) の ROP に変える。

### heap ベースアドレスのリーク必要性の判断
**Q1**: どのような場合に heap のリークが必須?
**A1**: ①safe-linking(2.32+)の tcache poisoning;②largebin attack の正確な bk_nextsize;③IO 攻撃の偽 IO_FILE のアドレス;④house of einherjar の prev_size。このいくつかの場合、heap をリークしないと攻められない。
**Q2**: heap leak は通常どう取得する?
**A2**: ①tcache の fd ポインタ(2.32+ は XOR 後なので解く必要がある);②smallbin/unsorted の fd/bk;③業務フィールドとチャンク内容の overlap 時の随伴リーク。優先度:unsorted > smallbin > tcache。
**Q3**: 2.32+ の tcache fd は XOR 後。heap アドレスを逆算できる?
**A3**: できる。`fd = (chunk_addr >> 12) ^ next` で、next が既知なら(NULL はリンクリスト末尾を表す)fd がそのまま (chunk_addr >> 12)。左に 12 ビットシフトすれば heap ページ先頭。これは逆算リークの手法。
**Q4**: リーク経路が全く無い場合は?
**A4**: 次元を変える:ブルートフォース——heap の中間 3 nibble はランダムで、単機なら 1/4096、数秒で割れる。ただしリモートは通常ブルートフォースコストが高いので、先に別の道を考える。

### off-by-one / off-by-null の特徴識別
**Q1**: edit 関数の逆アセンブルで `read(fd, buf, size)` を見たが size がチャンク割り当てサイズそのもの。穴は無い?
**A1**: off-by-one の可能性がある:`read(fd, buf, size+1)` や `buf[size]=0` のような null 終端操作。これは最も見落としやすい穴の点——null byte 1 つで poison null byte 手法をトリガーできる。
**Q2**: 溢れるのが null でなく任意の 1 バイトの場合、どう攻める?
**A2**: house of einherjar に転換するか、直接次チャンクの size を書き換えて chunk overlap を作る。任意バイトの off-by-one は null-byte より強く、size を大きく書き換えて free 時の結合を複数チャンクにまたがらせられる。
**Q3**: off-by-one が利用可能かの判定基準のキーは?
**A3**: ①チャンクが隣接しているか(malloc 順が連続)。②次チャンクが free 済みか(prev_size が初めて効く)。③size 検査が厳密か。3 つ揃えば標準的な poison null byte の場面。
**Q4**: off-by-one が 2.29+ の size vs prev_size 検査に阻まれた場合は?
**A4**: house of einherjar に転換する:入念に prev_size を構築して自分が偽造した fake chunk を指させ、fake chunk の size と prev_size を一致させて検査を通過させる。前提:heap をリークできること。
**Q5**: 調査しても本当に off-by-one が無く、他の穴も無い場合は?
**A5**: 次元を変える:struct レイアウトを再検討する——union フィールドの型混淆、signed/unsigned 変換、realloc の特殊挙動が無いか。ヒープ問題の穴の点はしばしば表面上の read/memcpy ではなく業務ロジックにある。

### chunk 結合が size mask に破壊される
**Q1**: オーバーフローで次チャンクの size を上書きし、free 時に結合をトリガーしたいが、直接 free すると abort する。どういうこと?
**A1**: 結合トリガーの検査:①`chunk_size` と次チャンクの `prev_size` の一致;②PREV_INUSE ビットが 0;③2.29+ はさらに後ろのチャンクの size も検査する。どれか 1 つでも不満なら abort。gdb でシングルステップしてどの assert で落ちたか見る。
**Q2**: 「上方」のチャンクへ結合したい場合は?
**A2**: トリガー方法:現在チャンクの prev_size が前のチャンクを指し、かつ PREV_INUSE が 0(前が free 済みを示す)。手法:先にチャンクを 1 つ free して「free 済み」状態を作り、中間チャンクの prev_size を書き換えて上方結合をトリガーする。
**Q3**: 結合に成功したが期待した overlapping chunks が得られない。どういうこと?
**A3**: 結合後の大チャンクは unsorted に入り、次回 malloc が小片を 1 つ切り出す。後半のアドレス範囲が元のある業務チャンクと重なる——これが overlapping。その size を連続 malloc して初めて取得できる。
**Q4**: 結合途中で tcache に対応 size の entry が残っており、malloc が tcache から優先取得して大チャンクを取れない場合は?
**A4**: 先に tcache 内の entry を消費する(同 size を 7 回 malloc)と、次回 malloc は unsorted の大チャンクを通らざるを得ない。判定基準:毎操作前に `tcache` コマンド(pwndbg/pwngdb)で tcache 状態を確認する。

### seccomp サンドボックスの識別とバイパス
**Q1**: 問題を入手してまず `seccomp-tools dump ./bin` を実行すると、execve は禁止だが open/read/write は許可。戦略をどう決める?
**A1**: ORW ROP チェーンに進む。前提はスタックを乗っ取れるか SROP に進めるか。ヒープ問題では通常 tcache poisoning で __free_hook = setcontext+61 を叩く → スタックピボット → ORW ROP。
**Q2**: open すら禁止された場合は?
**A2**: `openat` に転換する(新しい libc は open でなく openat を使う)。判定基準:seccomp filter の具体的ルールを見て、openat/open/execveat の 3 つのうち少なくとも 1 つが開いているか確認する。
**Q3**: システムコールを全部禁止して exit だけ残した場合、まだ flag を取れる?
**A3**: 次元を変える:サイドチャネルに進む——flag を読んだ後 print せず、alloc のサイズやタイムアウト長でビット単位にリークする(たとえば flag[i]=='A' なら 1 秒 sleep)。攻撃難度は非常に高いが可能。
**Q4**: サンドボックスが execve を許可し execveat を禁止する場合、何を意味する?
**A4**: 定番の system("/bin/sh") や one_gadget に直接進めることを意味する。判定基準:execve のホワイトリストを見たら最簡パスを優先する。

### orw チェーンの組み立て
**Q1**: orw に決め、setcontext は準備済み。rop チェーンをどう組む?
**A1**: 三段:`open("./flag", 0)` → `read(3, buf, 0x100)` → `write(1, buf, 0x100)`。引数は pop rdi/rsi/rdx gadget で装填する。libc の pop rdx は通常 `getcontext` 付近に `pop rdx; ret` がある。
**Q2**: pop rdx が見つからない場合は?
**A2**: `mov rdx, rXX; call rXX+X` 系の gadget(ropper から)を使うか、`mprotect` でどこかを rwx にして shellcode へジャンプする(shellcode 内で任意に syscall できる)。
**Q3**: flag ファイル名が不確定な場合は?
**A3**: `./flag`、`/flag`、`flag`、`flag.txt` を優先的に試す。複数回試す。または openat でディレクトリを走査する:先に open(".") で fd を取り → getdents でディレクトリを列挙 → 1 つずつ open する。
**Q4**: read 後に write で出すが stdout がリダイレクト/クローズされている場合は?
**A4**: fd 1 が通らないことがあるので、fd 2(stderr)を使うか、直接ソケットをリバース接続する。問題環境を見る:CTF は通常 stdout が通るので、通常は心配不要。
**Q5**: ORW チェーンが動くが read が -1 を返す。どういうこと?
**A5**: ①open が返す fd が間違っている(3 でなく、前の setcontext に占有されている可能性)。②パスが間違っている。③引数レジスタが setcontext に改ざんされている。gdb でブレークポイントを張り open の戻り値を見る。方向転換:libc の read でなく syscall を直接呼ぶ。

### environ によるスタックアドレスリークのタイミング
**Q1**: スタックの戻りアドレスを書き換えたい。スタックをどうリークする?
**A1**: libc の `environ` シンボルはスタック上の環境変数配列を指すポインタ。任意読み取りで environ を読めばスタックアドレスを取得できる。tcache poisoning で environ - 8 を叩く(malloc が environ 位置を返すため)と、add でスタック値を読み出せる。
**Q2**: スタックをリークした後、ret アドレスの位置をどう計算する?
**A2**: environ は通常スタックの高位。現在関数の ret は低位。gdb でローカル実行してオフセット(environ から return address までの差)を測る。リモートも通常安定(stack padding が異ならない限り)。
**Q3**: ret アドレスを取って ROP を仕掛けたがトリガーされない。どういう状況?
**A3**: ret 1 箇所の書き換えでは不十分で、対象関数がまだ return していないとトリガーされない。プログラムの自然な return を待つ必要がある。ヒープ問題のメニューループでは return は exit 時にしかトリガーされないので、複数回操作後に exit する必要があるかもしれない。
**Q4**: environ リークで有効値が取れない。何が考えられる?
**A4**: ①libc バージョン不一致で environ オフセットの計算ミス。②tcache poisoning がずれて environ に落ちていない。③一部の簡素環境で environ が NULL。方向転換:`__libc_argv` や `__environ` の他の別名をリークする。

### calloc の特殊性による戦略調整
**Q1**: add が malloc でなく calloc を使う。tcache poisoning をどうする?
**A1**: calloc は tcache から取らない(少なくとも定番 glibc ではそう)ので、fastbin/smallbin/unsorted から取る。戦略:先にターゲットチャンクを tcache から fastbin へ溢れさせ(7 個埋める)、次に calloc で汚染チャンクを取得する。
**Q2**: calloc で取り出したチャンクは memset でゼロクリアされる。汚染された fd も消える?
**A2**: クリアされるのは user data 領域だけで、チャンクヘッダ外の空間はクリアされない。よって tcache poisoning がチャンクをターゲットへ向けた後、calloc が出すのは「ターゲットアドレス+ゼロクリア user data」で、fd は既に poison の使命を完了している。
**Q3**: calloc 要求の size がちょうど tcache 範囲で fastbin が埋められない場合は?
**A3**: unsorted に転換する:tcache を埋めてから大きなチャンクを解放して unsorted に入れ、ターゲット size を切り出す(calloc はこのパスを通る)。またはオーバーフローで強制的に size を書き換えて unsorted に free する。
**Q4**: calloc のゼロクリアを完全に回避できない。tcache 手法を使わない手は?
**A4**: 次元を変える:largebin attack、house of orange など「tcache 戻り値に依存しない」手法に進む。calloc は tcache poisoning を難しくするだけで、全ヒープ手法を無効にするわけではない。

### tcache stashing 攻撃の適用判定基準
**Q1**: poisoning ではなく tcache stashing を検討するのはいつ?
**A1**: 3 つの特徴:①smallbin(size 0x90-0x400)を操作できる。②「任意 malloc」ではなく「任意位置に heap ポインタを 1 つ書く」ことを望む。③double-free 検査を越えられない、または safe-linking が poisoning を難しくする。
**Q2**: tcache stashing のプリミティブは何?
**A2**: smallbin のチャンクが tcache へ stash される際、libc はその bk 位置の値を有効なチャンクとして扱い、どこかに書き込む(具体的には tcache_perthread の対応バケットかリンクリスト)。「任意位置に heap ポインタを 1 つ書く」と等価。
**Q3**: このプリミティブで何をしたい?
**A3**: よくあるターゲット:①`__free_hook` への書き込み(旧 libc、事前に heap 上へ system ポインタを置く必要がある);②`_IO_list_all` の書き換えと組み合わせる;③`tcache_perthread_struct` の counts を書き換えて後続の tcache 手法を有効にする。
**Q4**: tcache stashing を仕掛けたが効果が無い。どう切り分ける?
**A4**: smallbin 内のチャンク順序を確認する——stashing は fifo で取るので、最初に stash されるのは head。自分が制御したいのが head か 2 番目かを明確にする。gdb で smallbin 状態を見る。

### house of botcake の適用条件
**Q1**: いつ house of botcake を使う?
**A1**: 2.29+ には tcache double-free 検査(key フィールド)があるが、tcache 内で double free したい場合。botcake は「tcache を埋める + unsorted 内で double-free」で検査を回避し、tcache 内の dup を実現する。
**Q2**: 具体的な手順は?
**A2**: ①同 size のチャンクを 7 個 malloc。②前 6 個を free して tcache に入れる。③A と B を隣接して malloc。④A B を free(この時 tcache 満杯なので unsorted に入る)。⑤tcache から 1 個 malloc して場所を空ける。⑥B を再度 free(合法に tcache に入る)。⑦さらに B を free(この時 tcache に空きがあり、再度 tcache に入って dup をトリガー)。
**Q3**: 手順が複雑でどこで間違いやすい?
**A3**: チャンク隣接性の判定で間違いやすい。途中に free の隙間があると、A、B の free 時に結合して size が変わる。gdb で各ステップの bins 状態を見る。
**Q4**: botcake が通らない。何に方向転換する?
**A4**: house of einherjar / house of apple / large bin attack へ方向転換する。botcake は double-free 回避の 1 つにすぎず唯一ではない。あるいは「double-free が本当に必要か」という前提から逆算する——そもそも double-free が不要で、UAF+上書きでも目的を達成できるかもしれない。

### 問題意図の判断:シェル / orw / 部分変更
**Q1**: 問題を入手してシェルか orw か判断できない。どう判断する?
**A1**: seccomp filter を見る:①サンドボックス無し → シェルを狙う(execve 許可);②execve 禁止で open/read/write 許可 → orw;③ほぼ全禁止 → サイドチャネル。この一歩で基調を決める。方向を間違えずに exp を書き始める。
**Q2**: 問題の意図が flag 取得でなく「グローバル変数の変更」の場合もある。どう判断する?
**A2**: 非常に稀で、通常は CTF の「check 通過」系問題。判定基準:main に `if (var == secret) puts(flag)` のようなロジックがある。このときは任意書き込みで var を secret に変えるだけでよく、完全なフロー乗っ取りは不要。
**Q3**: 方向を誤判定する代償は?
**A3**: シェルを狙ったがサンドボックスが execve を禁止し、exp が hook を通しても SIGSYS をトリガーして全損。判定基準:第一歩で seccomp-tools dump を行い、手を抜かない。
**Q4**: 途中で方向が間違っていたと判明したら、どう調整する?
**A4**: 前半(リーク + hook 制御)を残し、後半のペイロード(system → setcontext+ORW)を差し替える。最初から書き直す必要はない。前半のプリミティブは通常再利用できる。

### struct レイアウトの逆アセンブルで行き詰まる
**Q1**: add 関数の逆アセンブルで、malloc が返す buf に業務フィールドが入っているが、フィールドオフセットがはっきり見えない。どうする?
**A1**: 動的デバッグが静的より勝る。gdb を malloc 後にブレークし、continue を 1 回行い、返されたチャンク内容を dump して手動でフィールド(name/size/ptr/next)を埋める。IDA の構造体推測より 10 倍速い。
**Q2**: フィールドに union / ビットフィールドがあり、IDA 表示が乱れている。どうする?
**A2**: そのフィールドの全相互参照を探し、各読み書き点の意味を見る——union の実際の型は業務文脈で決まる。複数の読み取り点が異なる型に解釈される場合、それが穴の点(型混淆)かもしれない。
**Q3**: 構造内に関数ポインタフィールドがある。どう攻める?
**A3**: これは「内蔵 hook」であり、直接 UAF/オーバーフローで関数ポインタを上書きすれば、呼び出し時に rip を制御できる。libc hook を叩くより遥かに簡単なので、このルートを優先する。
**Q4**: 構造の逆アセンブルが長く行き詰まり、明確にできない。逆アセンブルを諦めて直接攻められる?
**A4**: 次元を変える:ファジング手法——大量のランダム操作で crash をトリガーし、キーフィールド位置を逆算する。または QEMU + libafl で半自動化する。すべての問題が精密な逆アセンブルに値するわけではない。

### ヒープレイアウト(feng shui)がうまく機能しない場合の対応
**Q1**: チャンク A B C を A→B→C の順でヒープ上に連続させたいが、実際には別のチャンクが挟まる。どうする?
**A1**: malloc 時に暗黙の割り当てがあるか確認する(たとえば最初の malloc 時に libc が tcache_perthread_struct 0x290 を割り当てる)。これらの「寄生チャンク」が順序を乱す。判定基準:最初の malloc の前に heap に既に何があるかを見る。
**Q2**: tcache を埋めて次の free を unsorted に通そうとしたが、結果はやはり tcache に入る。どういうこと?
**A2**: tcache count はチャンクサイズのインデックスを使い、要求サイズではない。free するのが異なるチャンクサイズかもしれない。確認:tcache に埋める全チャンクの size が完全に一致しているか。
**Q3**: レイアウトは合っているが free が期待した結合をトリガーしない。何が考えられる?
**A3**: PREV_INUSE ビットの設定ミス。free するチャンクが隣接していても、次チャンクの PREV_INUSE がまだ 1(前が未 free を示す)なら free は結合しない。fastbin の罠(fastbin 内チャンクの PREV_INUSE はクリアされない)が無いか見る。
**Q4**: レイアウトをどう調整しても合わない。何に方向転換する?
**A4**: 次元を変える:精密なレイアウトを追求せず、「散布」手法に変える——様々な size を大量に alloc し、確率的にどれが成功するか統計を取る。または現行の手法チェーンを諦め、レイアウト非依存の手法(largebin attack は largebin チャンク 1 つで足りるなど)に変える。

### 利用から逆にたどって穴の点を再検証する
**Q1**: 逆アセンブルで穴の点を出し、exp を書いたが通らない。繰り返し調整しても駄目。どうする?
**A1**: exp の調整をやめて源頭へ戻る:穴の点自体の理解が間違っていないか?gdb で「利用意図を持たずに手動で脆弱性をトリガー」し、メモリ変化だけを観察する。UAF だと思っていたものが実際は free 時に memset されていることがよくある。
**Q2**: 手動トリガーで脆弱性の実在を確認したが、exp がまだ通らない。原因は?
**A2**: 中間段階の副作用——malloc 時に tcache_perthread_struct が割り当てられ、free 時に arena が更新され、show がついでに malloc/free をトリガーする。これらの副作用が予期を乱す。全操作の副作用を列挙する。
**Q3**: 副作用を確認してもまだ駄目。何が考えられる?
**A3**: libc バージョンの細部——使っている libc と実際のリモートがマイナーバージョンで異なり、あるオフセットか検査が異なる。patchelf でローカル libc を厳密に一致させてから攻める。
**Q4**: 全確認しても問題が無いのに通らない。最後の手段は?
**A4**: 次元を変える:まったく別の利用パスに変える。ある手法チェーンのどこかの環がこの問題でちょうど機能しない(問題が stdout buffer を閉じて FSOP を無効にするなど)ことがあり、パスを変える方がむしろ速い。1 つの道に 2 時間超で粘り続けない。
**Q5**: 時間が尽きそうなのに exp がまだ通らない。どう損切りする?
**A5**: leak 部分(既に libc/heap をリークできているなら)を残し、最も単純な partial overwrite かブルートフォースを試す。複雑なチェーンを諦め、確率 1/16 のブルートフォースで通す方が、100% だがキーリークを欠くチェーンより価値がある。

### 「ロジック穴かメモリ脆弱性か」の判断
**Q1**: 長く探しても UAF やオーバーフローが見つからない。方向が間違っている?
**A1**: ロジック穴の視点へ方向転換する:①idx 境界(負数、超大)でアクセスすべきでないチャンクにアクセスできるか。②realloc(0) が free と等価な罠があるか。③duplicate index で 2 つの slot が同一チャンクを指すか。④整数オーバーフローで size 計算が誤るか。
**Q2**: idx の境界外読み取りを 1 つ見つけた。何ができる?
**A2**: 境界外読み取りは通常直接 leak プリミティブを渡す。判定基準:index 表が bss セグメントにあり、境界外で近傍の libc 関数ポインタかスタックアドレスを読み、直接ベースを計算する。
**Q3**: duplicate index(2 つの slot が同一チャンクを指す)を見つけた。どう利用する?
**A3**: UAF と等価——slot A を削除しても slot B はまだ use できる。全 UAF 手法が適用可能。判定基準:delete を 1 回行った後、もう 1 つの slot の show がまだ内容を出力できるか。
**Q4**: 整数オーバーフローで size が極大か極小になる。どう攻める?
**A4**: 極小(たとえば 0):malloc(0) は最小チャンクを返し、後続の 0 バイト超の書き込みがオーバーフローになる。極大(たとえば -1 → 0xffffffff):malloc が失敗して NULL を返し、後続の NULL 解釈は利用でなく crash——ただし NULL ポインタ+オフセットは(低アドレスへ mmap する問題で)攻められる。
**Q5**: これらを確認しても何も無い。他に何が考えられる?
**A5**: 次元を変える:問題自体が misc/rev 混在のヒープ問題かもしれない。main の隠しロジックを見る——隠しバックドア(未エクスポートシンボル)、時間/pid ベースのアルゴリズムの穴が無いか。ヒープ問題だからといって必ずヒープ脆弱性があるとは限らない。

### unsorted bin attack の判定基準と代替
**Q1**: unsorted bin attack を検討すべきなのはいつ?
**A1**: 「main_arena+固定値(0x60 付近)をターゲットアドレスへ任意書き込みする」需要があるとき。定番用途:`global_max_fast` を書き換えて全チャンクを fastbin に通す、または何らかのグローバルフラグを書き換える。プリミティブ:unsorted bin チャンクの bk を (target - 0x10) に書き換え、次回 malloc 時に unsorted 整理をトリガーすると、libc がチャンクアドレスを bk+0x10 に書く。
**Q2**: 2.28+ が unsorted bin の整合性検査(fd/bk 一致)を追加した。まだ攻められる?
**A2**: 直接は攻められない。largebin attack へ方向転換する——より厳格だがまだ攻められる。または tcache poisoning で直接 global_max_fast 付近に malloc して書き換える。unsorted bin attack は新バージョンでは主に組み合わせチェーンの一環で、単独の主手法ではない。
**Q3**: unsorted bin attack 後、ターゲットが main_arena+X という「ゴミ値」になった。この値自体に何の価値が?
**A3**: 判定基準:ターゲットが `_IO_list_all` なら、main_arena+X はちょうど偽チャンクの制御可能領域を指す(入念に X を選んで一致させる)ことで FSOP をトリガーできる。これが house of orange 旧手順のキーステップ。
**Q4**: 実行後、プログラムがクラッシュせず利用効果も無い。どういうこと?
**A4**: 次元を変える:ターゲット位置の値が本当に書き込まれたか確認する。unsorted bin attack は malloc 時にしかトリガーされないので、bk を書き換えた後に少なくとも 1 回 malloc を起こす必要がある。判定基準:書き換え後になんの alloc もしなければ無効。

### mmap チャンクの識別と利用
**Q1**: チャンクサイズが極めて大きい(128KB / M_MMAP_THRESHOLD 超)場合、どのパスを通る?
**A1**: glibc は mmap でカーネルから直接申請し、arena に入らない。特徴:チャンクヘッダの IS_MMAPPED (0x2) ビットが立つ。free 時は直接 munmap し、bin ロジックを通らない。この種のチャンクは従来のヒープ脆弱性では攻められない。
**Q2**: add が超大 size をサポートしユーザー制御可能な場合、何を利用できる?
**A2**: mmap チャンクの相対オフセットを攻撃する——mmap 割り当ては通常 libc か heap に隣接する(aslr ポリシー次第)ので、mmap アドレスのリークで libc ベースを逆算できる。これがリークの近道の 1 つ。
**Q3**: 強制的に mmap に通したいが M_MMAP_THRESHOLD が高すぎる。どうする?
**A3**: 方向転換する:mallopt(M_MMAP_THRESHOLD, low_value) はプログラムの呼び出し点が必要で、通常は無い。「複数回 malloc して arena 拡張をトリガー」し、mmap sbrk 以外の補助領域を出現させ、間接的にレイアウトへ影響を与える。
**Q4**: mmap チャンクの攻撃面が全く使えない。もう終わり?
**A4**: 次元を変える:mmap は補助パスの 1 つにすぎず、主手法は依然として arena 内のチャンクにある。UAF/オーバーフローの本線へ戻り、mmap チャンクは「環境ノイズ」として扱う。

### ローカルは通るがリモートが通らない場合の切り分け
**Q1**: ローカルの exp が安定して通るのに、リモートが爆発する。まず何を見る?
**A1**: 3 点を確認する:①libc バージョンが厳密に一致するか(patchelf で libc を置き換えて再現);②対話方式(recv/recvuntil のタイミング——リモートのネットワーク遅延が recv を早めに返すことがある);③EOF か追加出力を喰っていないか(たとえば banner が 1 行多い)。
**Q2**: バージョンは一致するがまだ不安定。どうする?
**A2**: tube 種別を変える:pwntools の remote/process は低層挙動が異なる。`ssh` でターゲットマシン上で process してみて、安定すればネットワーク層の問題、不安定なら環境差(リモートの stdout バッファリングの違いなど)。
**Q3**: recv が期待したデータを取れない。どう切り分ける?
**A3**: `context.log_level = 'debug'` で実際のバイトストリームを見る。recvuntil の照合文字列に tab が余分/スペースが不足していることがよくある。リモートの tty 挙動とローカルの pty 挙動は異なり、プロンプト形式に微妙な差が出ることがある。
**Q4**: リモートの exp が確率的に通る(たとえば 1/16)。どう最適化する?
**A4**: 次元を変える:外側のリトライスクリプトを書き、flag が出なければ kill してやり直す。pwntools の `try/except EOFError` ループで成功率を記録する。単発の安定性に粘り続けず、確率手法は 3-5% の成功率でも flag を取るには十分と受け入れる。
**Q5**: リモートセッションが張れない/頻繁に切断される。どうする?
**A5**: 確認する:①ターゲット flag のレート制限(rate limit);②ローカルネットワークのプロキシによるパケットロス;③pow(proof-of-work)が必要か——CTF でよくあるハッシュ衝突検証。banner 内容を見て確認する。

### exp 安定性の調整
**Q1**: exp がローカルで 10 回中 5 回成功する。安定性をどう上げる?
**A1**: 不安定な段階を特定する——各ステップに print を加え、どのステップの戻り値に差があるか記録する。よくある不安定:①bin 内チャンク順序依存(fifo/lifo 混在);②異常 free による tcache count の偏り;③ASLR 高位が偶発的にアライメント不一致を起こす。
**Q2**: bin 順序が不安定な根源は?
**A2**: unsorted bin の整理は「head から tail へ取る」が、整理過程でチャンクを smallbin/largebin へ移す。malloc のタイミングが異なると取れるチャンクが異なる。判定基準:毎操作前に gdb で bin 状態をスナップショットして差を比較する。
**Q3**: ASLR で一部の高位が不確定になる場合は?
**A3**: partial overwrite へ方向転換する——低 2-3 バイトだけ書き換え、高位は無視する。またはペイロードを複数の高位可能性と両立させる(rop チェーンに複数 candidate アドレスを置き、境界外実行で正しいものに当てる)。
**Q4**: 調整してもまだ確率手法の場合は?
**A4**: 次元を変える:確率を受け入れ、外側のバッチリトライを行う。またはより安定だが長い利用チェーン(partial overwrite のブルートフォースを、完全リーク + 精密な tcache poisoning で置き換える)に変える。安定性とチェーン長はしばしばトレードオフ。

### exp のどの段階で止めて pivot すべきか判断する
**Q1**: exp があるステップで繰り返し失敗する。「このチェーンはもう駄目だ」と判断する合図は?
**A1**: 3 つの合図:①同じ crash パターンが 3 回以上出る;②パラメータを変えても同じエラー;③gdb で見た実際の状態が予期と完全に食い違う(少しの差でなく、根本的に異なる)。このときチェーン自体に問題がある。
**Q2**: pivot 先の優先度が最も高いのは?
**A2**: 優先度:①同じ穴の点の他の手法(UAF → tcache が駄目 → fastbin);②同じリークで他の利用ターゲット(free_hook が駄目 → IO_FILE);③穴の点を変える(複数の脆弱性がある場合)。最後に初めて「穴の点自体の再検討」。
**Q3**: pivot 前に何を残すべきか?
**A3**: leak 段階の産物(libc/heap/stack ベース)は通常再利用できる。スクリプト前半を残し、後半だけ書き直す。判定基準:leak 段階が再現できる = 前半に問題なし。
**Q4**: 同じ段階に長く嵌まり、時間が流れる。最後の判断は?
**A4**: 次元を変える——ヒープ脆弱性の視点を完全に外れる。問題説明、添付の他のファイル、docker-compose の公開ポートを見る。ヒープ問題は 1 つの段階にすぎず、本当の突破口が web / rev / crypto 側にあることもある。問題種別の固定観念を飛び出す。
