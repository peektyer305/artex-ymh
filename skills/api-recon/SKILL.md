---
name: api-recon
description: WebサイトのAPIを収集する際にこのskillを呼び出します。
---

# API Recon（フロントエンドAPI偵察）

**認可済み**の前提で、可能な限り網羅的に発見する：**バックエンド API**（パス、メソッド、パラメータ、レスポンスボディ）、**フロントエンドルート**、**UI 機能トリガー**（タブ、モーダル、テーブル操作など）。

---

## 境界と禁止（Agent 必読 · 違反は範囲外）

本 skill は **API / パラメータ面の偵察のみ** を行い、脆弱性の探索やペネトレーション・エクスプロイトのフェーズではありません。

### タスク境界

| 範囲 | 許可 | 禁止 |
|---|---|---|
| **対象** | path、method、パラメータ、ルート、UI トリガーの列挙 | SQLi/XSS/権限外アクセス/ブルートフォース/fuzz の脆弱性、パケット改ざん攻撃、破壊的操作 |
| **認証** | Hook + stub/mock で**クライアント**側のログインゲートを迂回 | ユーザーにアカウント/パスワードを要求・推測する、実際のログインフォーム送信を試みる |
| **ランタイム** | 認証情報なしで API をフックし、mock レスポンスで SPA をログイン後のシェル層へ導く | 実際のバックエンドセッションに依存しないと続行できないフロー |

### 認証情報なしの動的分析（Phase 3 デフォルト）

1. `preload.js` / `runtime_harvest.js` でログイン・権限・メニューなどの bootstrap API を**インターセプトして stub** する；
2. ビジネス照会 API に対して**構造が正しく、ビジネスコードが成功で、データが空でもよい** mock body を返す；
3. フロントエンドをバックエンドなしまたは 401 の環境でもログイン後のページを描画できるようにし、より多くの XHR/fetch/WebSocket をトリガーする；
4. **空データ、空白テーブル、プレースホルダ UI はいずれも想定どおり**。そのために実際のログインや脆弱性テストへ逸れないこと。

**一言でいうと**：mock でフロントエンドのルートとコンポーネントのマウントを広げ、**outbound リクエストのみ記録する**。バックエンドが何を返すかは重要ではなく、重要なのはフロントエンドが**さらにどの API を送信するか**である。

### フローのハード禁止

| 禁止 | 代替手段 |
|---|---|
| Phase 1 完了前に主 entry `index-*.js` を grep/curl/Read して API path を抽出する | `OUTDIR/harvest_static.py` を実行する |
| harvest の代替として `extract_apis.py` などを手書きする | `OUTDIR/harvest_static.py` を変更して再実行する |
| 同じ grep/コマンドが 2 回以上失敗しても繰り返す | 戦略を変える：tool_logs を読む、harvest を変更する、reference を確認する |
| ゲート A/B をスキップして `scripts/` のオリジナルを直接実行する | OUTDIR へコピーして対象に合わせて変更する |
| 実際のユーザー名/パスワード、OTP、OAuth などの認証 | stub/mock（前述を参照） |
| 「実際のデータを取得する」ことを理由に stub をスキップして権限外アクセス/インジェクションのテストを行う | outbound のみ記録する。これは recon の境界 |
| 削除、機密データのエクスポート、一括書き込みなどの不可逆な操作 | coverage のクリックも同様 |
| runtime + 動的列挙を完了せずに、全ページと API を取得済みと主張する | 「完了定義」を参照、または限界を明記する |
| パラメータトリガーマトリクス + diff を完了せずに、全パラメータを把握済みと主張する | Phase 3b マトリクス + Phase 5 diff |
| 単一の runtime サンプルで必須/任意を推定する | 複数サンプルの diff、または検証ルール/エラーからの逆推定 |

---

## 2層モデル + 実行モード

| 層 | 成果物 | 限界 |
|---|---|---|
| **静的**（JS bundle） | 全量の endpoint パス、ルート草案、組み立てポイントのフィールド候補 | HTTP メソッドなし；パラメータは Phase 1b が必要；ランタイムで組み立てられる URL を見落とす |
| **ランタイム**（ライブセッション） | メソッド + body + レスポンス + 動的 URL + WS/SSE；複数サンプルの diff でパラメータを補完 | ページが実際に描画されないとリクエストが送信されない；単一サンプルでは必須/任意を確定できない |

| 実行モード | エンジン | 適用 |
|---|---|---|
| **depth** | `runtime_harvest.js`（Puppeteer） | API リスト、METHOD/params/レスポンスボディ、WS/SSE、再現可能な一括実行 |
| **coverage** | browser + `preload.js` | タブ/モーダル/テーブルをクリックし、機能ポイントをより深くカバーする |
| **both** | 先に depth、次に coverage | 最も完全で、所要時間が最長 |

**パラメータ方法論**（汎用スクリプトなし）：path は harvest/正規表現を使用；パラメータは **アンカー拡窓 + UI バインディングチェーン + 複数サンプル diff + エラー逆推定**（grep レシピは [reference.md](reference.md) の J 節を参照）。

---

## 完了定義

すべて満たした場合のみ recon 完了を主張できる：

- [ ] **静的**：Phase 1 harvest が `api_static.txt`、`routes.txt`、`js/` を生成する
- [ ] **ランタイム**：depth または coverage の少なくとも一方；coverage/both は **Hook 有効 + 動的列挙ループ** が必要
- [ ] **シェル進入**：ビジネス path へアクセスした際に `/login` でない（hash ルートに注意）
- [ ] **パラメータ**：coverage/both でパラメータトリガーマトリクス + `param_samples.json` を完了；Phase 5 で `params_merged.json` を統合
- [ ] **深度**（モジュールページが空白の場合）：Phase 4 で権限ツリーを復元して再実行し、**module レベルの API** が現れるまで続ける（locale/bootstrap のみでない）
- [ ] **成果物**：Phase 5 の成果物が揃っている（Phase 5 成果物表を参照）；`insert_assets` がサービスとエンドポイントのアセットを書き込む

---

## スクリプトとゲート

`scripts/` は参考テンプレートにすぎず、オリジナルを直接実行して最終結果とみなすことは**禁止**。

**ルール**：先に読む → 対象に合わせて変更 → `OUTDIR`（例 `recon/`）へ書き込む → `CHANGES.md` に記録；一致しない場合は方法論に従って書き直し、構造のみ借用する。

| ゲート | タイミング | 参考スクリプト → OUTDIR コピー | よくある必須変更点 |
|---|---|---|---|
| **A（静的）** | Phase 0 後、harvest/spider を**初めて**実行する前 | `harvest_static.py` / `spider_mpa.py` | **多くのサイトはデフォルト regex で直接実行できる**；manifest/書き方が一致しない場合のみ endpoint 正規表現、webpack/Vite `publicPath`、MPA の exclude/cookie を変更 |
| **B（ランタイム）** | Phase 2 後、depth/coverage を実行する前 | `runtime_harvest.js` / `preload.js` + `config.json` | Cookie/localStorage のキー、neutralize の成功値、stubs、login の正規表現、api プレフィックス、hash/history |

**SPA の強制順序**（交換不可；Phase 番号は「先に探索してからスクリプト」より優先）：

| ステップ | 必須 | 禁止 |
|---|---|---|
| Phase 0 完了後 | 次の Bash = `python3 OUTDIR/harvest_static.py <URL> OUTDIR` | 主 entry `index-*.js`（通常 >500KB）を curl/grep/Read する |
| ゲート A | スクリプトをコピー → 必要に応じて小変更 → **すぐに実行** | 先に手動で API を抽出してから harvest するか判断する |
| Phase 1 完了前 | `wc -l` で成果物を検証；404 は harvest を変更して再試行 | extract スクリプトを手書きする；未ダウンロードの URL を繰り返し grep する |
| Phase 1b 以降 | grep は `OUTDIR/js/*.js` のみ | 主 bundle で harvest を代替する |

- ✅ `harvest_static.py` をコピー → （任意）regex を変更 → **すぐに実行**
- ❌ 主 bundle を curl → grep を複数回 → 一時的な extract を書く → 最後にやっと harvest
- **MPA**：Phase 0 後の次の Bash = `python3 OUTDIR/spider_mpa.py ...`

---

## ツールと出力の制約

| 制約 | 説明 |
|---|---|
| 大容量ファイル | >100KB の `index-*.js` を Read/grep でコンテキストへ入れることは**禁止**；OUTDIR のスクリプトで一括処理する |
| grep 出力 | `\| head -20` または `-m 5` が必須；会話には path の要約のみ残し、bundle の断片を貼らない |
| 検証 | `wc -l`、`ls \| wc -l` を使用；ディレクトリ全体を Read しない |
| regex の初探 | 任意、1 回以下、≤50KB の小さな chunk または HTML のみ；正式な静的解析は harvest を基準とする |
| reference | レシピ/テンプレート/トラブルシューティングは [reference.md](reference.md) を参照。全文を inline で繰り返さない |

---

## 実行ロードマップ

```
Phase 0 分類 + OUTDIR
  → ゲート A → Phase 1 harvest（★ すぐに実行 ★）
  → Phase 1b パラメータ逆解析
  → Phase 2 認証の3つのゲート → config.json
  → ゲート B → Phase 3 ランタイム + パラメータマトリクス
  → Phase 4 権限ツリー（必要な場合）→ Phase 3 を再実行
  → Phase 5 レポート統合 + insert_assets で発見したすべてのサービス、エンドポイント API のアセットを一括挿入。どのような場合も、挿入時に発見済みアセットを漏らしてはならない
```

順にチェックする。**前の項目が完了するまで次の Phase に入ってはならない**。

1. [ ] **Phase 0**：SPA/MPA を初探；`OUTDIR` を作成 → [Phase 0](#phase-0--分類)
2. [ ] **ゲート A + Phase 1**：スクリプトをコピー → **すぐに** harvest → `wc -l` で検証 → [Phase 1](#phase-1--静的)
3. [ ] **Phase 1b**：アンカー拡窓 + バインディング層 → `param_candidates.json` → [Phase 1b](#phase-1b--パラメータ逆解析)
4. [ ] **Phase 2**：認証の3つのゲート → `config.json` → [Phase 2](#phase-2--認証の3つのゲート)
5. [ ] **ゲート B**：runtime スクリプトを調整 → [Phase 3](#phase-3--ランタイム)
6. [ ] **Phase 3**：depth / coverage / both；シェル進入を確認；パラメータトリガーマトリクス → `param_samples.json`
7. [ ] **Phase 4**（必要な場合）：権限ツリー → stubs を patch → Phase 3 を再実行 → [Phase 4](#phase-4--権限ツリーの復元)
8. [ ] **Phase 5**：成果物の統合 + レポート + `insert_assets` → [Phase 5](#phase-5--統合とレポート)

---

## Phase 0 — 分類

エントリ HTML を取得し、**`OUTDIR` を作成**（skill 内の `scripts/` を変更しない）：

- **SPA**：空シェル + `<div id=app>` + chunk → Phase 1–5
- **MPA**：SSR + `<form>`、endpoint bundle なし → ゲート A 後：

```bash
python3 recon/spider_mpa.py <BASE_URL> <OUTDIR> [--cookie "session=..."] [--max 300] [--depth 5] [--exclude "logout|delete|destroy"]
```

`forms.txt`、`links.txt`、`api_inline.txt` を生成する。SPA で forms ≈ 0 の場合 → Phase 1 へ移行。

---

## Phase 1 — 静的

[スクリプトとゲート](#スクリプトとゲート) ・ [ツールと出力の制約](#ツールと出力の制約) に従う。

```bash
python3 recon/harvest_static.py <BASE_URL> <OUTDIR>
```

harvest：HTML script を解析 → webpack/Vite manifest → 全 lazy chunk をダウンロード → `js/`、`api_static.txt`、`routes.txt`、`chunkmap.txt` を生成する。

```bash
wc -l OUTDIR/api_static.txt OUTDIR/routes.txt
ls OUTDIR/js | wc -l
```

- chunk 数 vs manifest：404 は harvest を変更して再試行する。chunk を 1 つずつ手動 curl しない
- `api_static.txt` が少なすぎる → OUTDIR 内の endpoint 正規表現を緩めて再実行（reference を参照）

### Phase 1b — パラメータ逆解析

path は Phase 1 由来；パラメータフィールドは別途 recon する必要がある。grep のルールは [ツールと出力の制約](#ツールと出力の制約) を参照。

**完了基準**：重要な API について以下を回答できること。フィールド名、転送位置、型の推定、必須かどうか、サンプル値、信頼度。

#### 1b.0 — 転送形態

| 形態 | パラメータの場所 | 静的解析で優先的に見る |
|---|---|---|
| REST JSON | body + query | path アンカー付近の `(params\|data\|body)\s*:\s*\{` |
| GraphQL | `variables` | gql テンプレート、`$page: Int` |
| 従来の form | urlencoded | `<form>`、`FormData` |
| ファイルアップロード | multipart | `FormData.append` |
| パスパラメータ | `/user/:id` | ルートテーブル + `useParams` / `$route.params` |
| 暗号化/署名 | `sign`/`data` に包まれる | 暗号化関数の入力を Hook（reference D 節） |

成果物：各 API に `transport: query|json|form|graphql|encrypted` を付与する。

#### 1b.1 — アンカー拡窓

既知の path をアンカーに、ウィンドウを広げて組み立てオブジェクトを探す：

```bash
grep -n '"/api/user/list"' OUTDIR/js/*.js | head -20
grep -rhoaE '.{0,120}("/api[^"]+").{0,200}' OUTDIR/js/*.js | head -20
grep -rhoaE '(params|data|body|payload)\s*:\s*\{' OUTDIR/js/*.js | head -20
```

| ラッパー層 | パラメータの手がかり |
|---|---|
| axios インスタンス | `data` / `params` |
| 共通 request | インターセプタがグローバルフィールドを注入 |
| OpenAPI クライアント | 生成された method シグネチャ |
| React Query / SWR | hook の第2引数 |
| Vue composable | composable の入力 |

型の残骸：`yup`/`zod`/rules、`Form.Item name=`、埋め込み Swagger。

→ `param_candidates.json`：`{ path, fields[], source: "static-callsite", confidence }`

#### 1b.2 — バインディング層

```
Form field → onFinish/handleSubmit → transform → API payload
```

| バインディング元 | 手法 |
|---|---|
| フォーム submit | submit → transform → API を追う |
| テーブル検索 | `getFieldsValue()` → `params` |
| ルート | `:id` / `?tab=` |
| インターセプタ | グローバル `tenantId`、ページネーション、sign |
| 列挙 select | `options` → API の列挙値 |

DevTools の call stack で `fetch`/`XHR.send` から上へ遡って組み立て関数を追う。

#### 1b.3 — 組み立ての3つの問い（≠ Phase 2 の認証の3つのゲート）

| 問い | 何に答えるか |
|---|---|
| **組み立て** | payload がどこで build されるか、transform の痕跡 |
| **検証** | required、pattern、enum |
| **転送** | path / query / body / multipart / ヘッダ |

インターセプタゲート（Phase 2）でついでにグローバル注入フィールド（Authorization、`X-Tenant-Id`、sign）を読む。

#### 1b.4 — Phase 3 への接続

候補フィールドは静的解析/バインディング層由来；**必須/任意/条件依存**は Phase 3 のパラメータマトリクス + diff + Phase 5 のエラー逆推定が必要。

---

## Phase 2 — 認証の3つのゲート

`OUTDIR/js/` を grep（`head` 付き）し、`config.json` に書き込む（レシピは reference を参照）：

| ゲート | 問題 | キーワード |
|---|---|---|
| **レンダリングゲート** | ログイン済みかどうかをどう判断するか？ | `isLogin`、`getToken`、Cookie/localStorage |
| **インターセプタゲート** | 何が `/login` への遷移をトリガーするか？ | `response_code`、`errno`、axios interceptor |
| **コンテンツゲート** | メニュー/権限はどこから来るか？ | `menu`、`permission`、`role`、`acl`、`routes` |

localStorage のキー名を認証情報とみなすことは禁止。chunk/リクエストチェーンから確認する必要がある。

**出口 = ゲート B**：結論を `config.json` に落とし、`OUTDIR/runtime_harvest.js` / `preload.js` を変更する。

### Phase 2b — API 観察（任意）

OUTDIR 内の `preload.js` でセッションのキー名、Authorization、ネストされた API URL を確認する：

| 設定 | 成果物 |
|---|---|
| `recordDetail: true` | `__API_RECON_DETAIL__` |
| `observe.xhrHeaders: true` | ヘッダの観察 |
| `extractUrlsFromResponse: true` | レスポンス内の子 API |
| `observe.storageReads/cookieReads: true` | config への反映 |
| `neutralizeVueRouter: true` | `__API_RECON_ROUTES__` |

coverage の各ラウンドでエクスポート：`__API_RECON_LOG__`、`__API_RECON_DETAIL__`、`__API_RECON_ROUTES__`、`__API_RECON_OBSERVE__`。

---

## Phase 3 — ランタイム

ゲート B を通過済みであること；[境界と禁止](#境界と禁止agent-必読--違反は範囲外) ・ 認証情報なしの mock 戦略に従う。

`config.json` に `"runtimeMode": "depth" | "coverage" | "both"` を設定する（テンプレートは reference を参照）。

### Hook と stub（depth + coverage 共通）

| 層 | 範囲 | 目的 |
|---|---|---|
| L1 精密 | auth/権限/bootstrap の stub | 初回表示の認証を通過 |
| L2 負方向の修正 | すべての JSON レスポンス | 未ログインコード → 成功 |
| L3 フォールバック | L1 に一致しない `/api` など | 空の成功ボディで UI を広げる |

- **depth**：fake auth + `forward` でビジネスコードを変更 + `stubs`；`routes`（hash/history）を走査；`runtime_api.json` を生成
- **coverage**：**document-start** で `preload.js` を注入（CDP `addScriptToEvaluateOnNewDocument` または Userscript）

検証：`window.__API_RECON_PRELOAD__` が存在する；ビジネス path が `/login` に戻らない。

```bash
cd recon && npm install
node runtime_harvest.js config.json
```

### 3b — coverage の動的列挙（必須）

1. メインナビゲーション/サイドバー — 各項目をクリックし、ネットワークを 1–3 秒待つ
2. タブ — `role=tab`、`.ant-tabs-tab`
3. テーブル — 先頭行の表示/編集/詳細
4. ツールバー — エクスポート、フィルタ、新規作成（**不可逆な削除は避ける**）
5. 各モジュールに入るたび — API/ルートを統合
6. SPA — `routes.txt` でカバーされていない path を制御下で `pushState`（MPA は禁止）

**パラメータトリガーマトリクス**（必須）：各モジュールで操作タイプごとに 1 回ずつ記録し、**複数サンプルを diff** する：

| 操作 | 通常増えるパラメータ |
|---|---|
| リスト初回表示 | ページネーション + デフォルトフィルタ |
| 検索クリック | keyword、filter |
| 高度なフィルタ | より多くの optional |
| 新規作成/編集 | 完全な entity |
| 一括/エクスポート/並べ替え | `ids[]`、`exportType`、`sortField` |

**stub 下でも outbound の body/headers は依然として実物**。リクエストを基準とする。記録 → `scan_raw.json`、`param_samples.json`、`api_detail.json`。

- **Vue**：`neutralizeVueRouter: true` + document-start preload
- **React**：`routes.txt` + サイドバークリック + `pushState`
- **both**：先に 3a depth、次に 3b coverage

---

## Phase 4 — 権限ツリーの復元

**トリガー**：モジュールページが空白 / 各ルートが bootstrap（例 locale）のみ → コンテンツゲートを通過していない。

| 現象 | 意味 |
|---|---|
| シェル進入に成功 | レンダリングゲート + インターセプタゲートを通過済み |
| サイドバーの項目不足/クリックで空白 | stub の shape または権限コードが不十分 |
| 各ルートの API が同じで非常に少ない | `v-if permission` が通過していない |
| `routes.txt` が bundle よりはるかに少ない | auth モジュールから補完する必要がある |

```bash
grep -rhoaE '"/api[^"]*(permission|perm|role|menu|acl)[^"]*"' OUTDIR/js/*.js | sort -u | head -30
grep -rhoaE 'userRouteAuth|getResultTree|routeMap|routeLink|menuList|authList' OUTDIR/js/*.js | head -20
```

典型的なチェーン：`role_permissions`（flat codes）+ `permissions/all`（tree）→ `getResultTree` → `userRouteAuth[CODE].url`。

```bash
python3 recon/extract_route_map.py recon/js recon/
python3 recon/build_perm_tree.py recon/js recon/ --config recon/config.json
```

中間成果物：`route_map.json`、`userRouteAuth.json`、`permissions_tree.json`、`*_stub.json`、`perm_codes_all.txt`。

stub 検査：外側の `response_code` がインターセプタゲートと一致；flat codes と tree が揃っている；`routes` が `route_map` の全 link をカバーする。

`config.json` を更新後、**Phase 3 を再実行**。大規模 SPA では `waitUntil`、`routeTimeout`、`perRouteMs` を調整できる（reference の A3/I 節を参照）。

---

## Phase 5 — 統合とレポート

### 成果物表

| ファイル | フェーズ | 内容 |
|---|---|---|
| `js/`、`api_static.txt`、`routes.txt`、`chunkmap.txt` | 1 | 静的 bundle と path |
| `param_candidates.json` | 1b | 静的パラメータのフィールド候補 |
| `config.json` | 2 | 3つのゲート + runtime 設定 |
| `runtime_api.json` | 3a | depth の詳細記録（WS/SSE を含む） |
| `param_samples.json`、`scan_raw.json`、`api_detail.json` | 3b | 複数サンプル、クリックログ、detail |
| `route_map.json` など | 4 | 権限ツリーの中間ファイル（実行した場合） |
| `params_merged.json` | 5 | 統合したパラメータフィールド + 信頼度 |
| `api_merged.txt` | 5 | `METHOD /path [params] [static\|runtime\|both]` |
| `site_map.json` | 5 | ルート、API、params、機能ポイント、限界 |
| **insert_assets** | 5 | すべてのサービス、エンドポイントアセットをアセットライブラリへ書き込む |

### 5b — パラメータ統合

`param_samples.json` から diff する。**汎用統合スクリプトはない**。信頼度ルールは reference の J7（高/中/低/トリガー待ち）を参照。

### 5c — エラー逆推定

認可範囲内で不完全なリクエストを送信して 400 を読むことができる（**パラメータ recon であり、脆弱性テストではない**）：`field 'x' is required`、列挙エラーなど。`data` ラッパー、`variables`、暗号化前の `bizData` に注意。

レポートに明記する必要がある：runtimeMode、静的/ランタイムの API 数、パラメータ信頼度、未カバーのモジュール、参考スクリプトとの相対的な `CHANGES.md` の要約。

`site_map.json` の推奨構造：

```json
{
  "site": "https://example.com",
  "runtimeMode": "both",
  "appType": "vue-spa",
  "routeGuardStrategy": ["nav-neutralize", "L1-auth", "L2-patch", "forward"],
  "apisFromStatic": [],
  "apisFromRuntime": [],
  "apis": [],
  "params": [{ "method": "POST", "path": "/api/user/list", "transport": "json", "fields": [] }],
  "frontendRoutes": [],
  "routesVerifiedByClick": [],
  "featuresTriggered": [],
  "limitations": ""
}
```

その他のフィールドと grep レシピは [reference.md](reference.md) を参照。

---

## 一般的な説明

- **フレームワーク非依存**：webpack/Vite/Angular の lazy load 手法は同じ
- **転送**：REST/JSON、GraphQL、WebSocket、SSE；gRPC-web は範囲外
- **SSR**：クライアントの fetch は記録可能；RSC/Server Actions は完全には列挙できない
- **盲点**：JSVMP、WASM、HMAC/mTLS の強力な検証 → 静的解析 + 限界の明記
- **パラメータの盲点**：条件連動、hidden params、WASM 組み立て → 「トリガー待ち」/「到達不能」
- **静的解析はセーフティネット**：runtime がブロックされても静的解析で endpoint を列挙できる

---

## 追加リソース

- Grep レシピ、`config.json` テンプレート、トラブルシューティング、Hook、パラメータ逆解析の J 節、site_map テンプレート：**[reference.md](reference.md)**
- 参考スクリプトのパスは [スクリプトとゲート](#スクリプトとゲート) の表を参照
