# api-recon — リファレンスマニュアル

Grep レシピ、`config.json` テンプレート、トラブルシューティング。すべての grep は `js/` ディレクトリに対して実行する。bundle が単一行の場合は、先に `js-beautify` または `sed 's/}/}\n/g'` を実行する。通常はコンテキストウィンドウ付きの raw grep で十分。

## スクリプトの説明

`scripts/` 内の全ファイルは**参考テンプレート**であり、実行前に必ず対象サイトに合わせて調整する。典型的な変更点：

| スクリプト | よくある調整項目 |
|---|---|
| `harvest_static.py` | endpoint 正規表現、webpack/Vite manifest 解析、マイクロフロントエンドの publicPath、再試行/並行処理 |
| `runtime_harvest.js` | neutralize のフィールド名と成功値、stub のマッチルールと body 構造、routes の出所、WS 記録、`waitUntil`/`routeTimeout`/`proxy` |
| `preload.js` | `loginPathRe`、L1 stubs、`neutralize.fields`、`apiPattern`、L3 を有効にするか、`recordDetail`、`observe.*`、`neutralizeVueRouter` |
| `spider_mpa.py` | `--exclude` の破壊的リンク、cookie、depth/max、同一ドメインフィルタ |
| `extract_route_map.py` | `routeMap` / `routeLink` の正規表現、KEY の命名パターン |
| `build_perm_tree.py` | `userRouteAuth` の解析、`ROOTS`/`PREFIX_PARENT` の階層ヒューリスティクス、stub の外側フィールド名 |
| `config.json` | 上記すべてのサイト専用パラメータの統合エントリポイント |

調整後のファイルはタスク作業ディレクトリ（例 `recon/`）に置くことを推奨し、レポートに参考スクリプトに対する具体的な変更を明記する。

---

## A. 3つのゲートの逆解析

### A1. レンダリングゲート — 「ログイン済みかどうかをどう判断するか？」

```bash
grep -rhoaE '.{0,40}(isLogin|isAuthenticated|loggedIn|hasLogin|requireAuth)\b.{0,80}' js | head
grep -rhoaE 'function (getUser|getToken|getAuth)[0-9]?\([^)]*\)\{.{0,200}' js | head
grep -rhoaE '(localStorage|sessionStorage)\.getItem\("[^"]+"\)' js | sort -u
grep -rhoaE '(Cookies?|cookie)\.(get|load)\("[^"]+"\)' js | sort -u
grep -rhoaE '\batob\(|JSON\.parse\(|jwt|decode' js | head
```

`isLogin = f(getUser())` → `getUser = decode(storage.read(KEY))` という連鎖を探し、**ストレージキー**、**コンテナ**（Cookie vs localStorage）、**エンコーディング**を確定する：

| エンコーディング | config での偽造方法 |
|---|---|
| 平文文字列 / `"1"` / token | `"value": "anything-truthy"` |
| `JSON.parse(x)` | `"value": "json:{\"id\":1,\"username\":\"admin\"}"` |
| `JSON.parse(atob(x))` | `"value": "b64json:{\"id\":1,\"username\":\"admin\"}"` |
| JWT | 署名なし/`alg:none` の JWT、または bundle 内の鍵で署名 |
| 暗号化（SM2/AES/RSA） | ハードコードされた鍵を探す；レンダリングゲートがデコード可能な blob のみ必要な場合は forge できる；そうでなければ静的解析でフォールバック |

→ `cookies` / `localStorage` に書き込む。

### A2. インターセプタゲート — 「何が /login への遷移をトリガーするか？」

```bash
grep -rhoaE '.{0,60}(interceptors\.response|axios|request\.use).{0,120}' js | head
grep -rhoaE '.{0,40}(response_code|errcode|errno|\bcode\b|\bret\b|\bstatus\b)\s*[=!]==?\s*[\-0-9]{1,4}.{0,60}' js | head -20
grep -rhoaE '.{0,40}(未登录|请重新登录|登录已过期|unauthorized|登录失效|授权|token.{0,10}invalid).{0,40}' js | head
grep -rhoaE '.{0,30}(location\.href|router\.(push|replace)|navigate)\([^)]*login[^)]*\)' js | head
```

確定する：**フィールド名**、**成功値**（通常 `0` または `200`）、**遷移をトリガーする失敗値**。junk session で検証する：

```bash
curl -sk -X POST -H 'Cookie: <fakekey>=junk' https://target/api/<protected> -d '{}' -H 'Content-Type: application/json'
```

→ `neutralize.fields` + `neutralize.success` に書き込む。

### A3. コンテンツゲート — 「メニュー/権限はどこから来るか？」

```bash
grep -rhoaE '"/api[^"]*(permission|perm|role|menu|acl|resource|nav)[^"]*"' js | sort -u
grep -rhoaE '.{0,30}(menus|permissions|menuList|routeList|authList|role_permissions)\b.{0,120}' js | head
grep -rhoaE 'userRouteAuth|getResultTree|routeMap|routeLink|hasPermission|checkAuth' js | head
grep -rhoaE '([A-Z_][A-Z0-9_]*):\{name:"[^"]*",link:"/[^"]+"\}' js | head
```

**2層のデータ**（一般的なエンタープライズ管理画面）：

| API | 典型的な payload | 利用側 |
|---|---|---|
| `.../role_permissions` | `{ permissions: string[], role_type }` | ルートガード、ボタンレベルの ACL |
| `.../permissions/all` | `tree[{ code, position, children }]` | サイドバーメニューの描画 |
| bundle 内の `userRouteAuth` | `{ CODE: { url, name? } }` | code → フロントエンド path |
| bundle 内の `routeMap` | `{ KEY: { name, link } }` | エイリアス解決（webpack `o.DASHBOARD`） |

利用側のコードを読んで確認する：`getResultTree(tree, permissions)` がどうフィルタするか、`v-if` / `hasAuth(code)` がどのフィールドを検査するか。

**手動 forge**（小規模サイト）：permissive な payload を構築 → `stubs`。

**完全な権限ツリーの復元**（大規模サイトで、サイドバー/サブモジュールが依然として空白の場合）：**I 節**を参照。

---

## B. config.json テンプレート

```json
{
  "baseUrl": "https://target/",
  "runtimeMode": "both",
  "chromium": "/usr/bin/chromium",

  "cookies": [
    { "name": "auth", "value": "b64json:{\"id\":1,\"username\":\"admin\",\"role\":\"admin\",\"func\":{},\"permissions\":[\"*\"]}" }
  ],
  "localStorage": { "token": "faketoken", "isLogin": "1" },

  "neutralize": {
    "fields": ["response_code", "code", "errno", "ret", "status"],
    "success": 0,
    "flags": { "success": true, "message": "ok" }
  },
  "forward": true,
  "loginUrlPattern": "/login",
  "apiPattern": "/api/|/rest/|/graphql",

  "mockTier": "L1+L2",
  "recordDetail": true,
  "observe": {
    "storageReads": false,
    "cookieReads": false,
    "xhrHeaders": true
  },
  "neutralizeVueRouter": true,
  "stubs": [
    {
      "match": "permissions/all|/menu|role_permissions",
      "body": {
        "response_code": 0, "code": 0,
        "data": {
          "permissions": ["*"],
          "menus": [
            { "name": "dashboard", "path": "/dashboard", "show": true, "children": [] },
            { "name": "alert", "path": "/alert", "show": true, "children": [] }
          ]
        }
      }
    }
  ],

  "explore": {
    "clickTabs": true,
    "clickTables": true,
    "pushStateFallback": true,
    "maxMenuItems": 50
  },

  "routes": ["/dashboard", "/alert", "/asset", "/device", "/report", "/config", "/system"],
  "waitMs": 1500, "perRouteMs": 900, "headless": true,
  "waitUntil": "domcontentloaded",
  "routeTimeout": 12000,
  "proxy": "",

  "captureResponses": true, "recordWs": true, "respMax": 600
}
```

フィールドの説明：
- `runtimeMode`：`depth`（Puppeteer）、`coverage`（browser MCP）、`both`
- `cookies[].value` のプレフィックス：`b64json:` → base64(JSON)；`json:` → 生の JSON；プレフィックスなし → リテラル
- `forward: true` は実際のリクエストを転送してコードフィールドを書き換える；`false` は完全オフライン stub
- `mockTier`：coverage モードの preload が有効化する階層。例 `L1+L2`、`L1+L2+L3`
- `routes` は `routes.txt` 由来；メニューを forge すると harness が自動的に `<a href>` を追加する
- `captureResponses` / `recordWs` は depth モードでのみ有効
- `waitUntil`：大規模 SPA では `domcontentloaded` を使い、`networkidle2` によるハングアップを避ける
- `routeTimeout`：単一ルートの `page.goto` タイムアウト（ミリ秒）
- `proxy`：Puppeteer の `--proxy-server`；`HTTP_PROXY` / `HTTPS_PROXY` を設定してもよい

### B1. 2つの stub テンプレート（role_permissions + permissions/all）

```json
"stubs": [
  {
    "match": "role_permissions",
    "body": {
      "response_code": 0,
      "data": {
        "permissions": ["MONITOR", "MONITOR_ALERT", "THREAT", "ASSETS_RISK"],
        "role_type": "SUPER_ADMIN"
      }
    }
  },
  {
    "match": "permissions/all",
    "body": {
      "response_code": 0,
      "data": [
        {
          "code": "MONITOR",
          "position": 1,
          "children": [
            { "code": "MONITOR_ALERT", "position": 1, "children": [] }
          ]
        }
      ]
    }
  }
]
```

外側のフィールド名（`response_code` / `code` / `data`）は A2 のインターセプタゲートと一致させる；`permissions` は tree 内のすべての leaf code をカバーする必要がある。

---

## C. coverage モード：preload の設定

`scripts/preload.js` の先頭にある `CONFIG` オブジェクトを編集するか、CDP で注入する前に置換する：

```javascript
const CONFIG = {
  loginPathRe: /\/(login|signin)(\/|$|\?)/i,
  mockTier: 'L1+L2',
  forward: true,
  recordDetail: true,
  extractUrlsFromResponse: true,
  neutralizeVueRouter: true,
  observe: { storageReads: false, cookieReads: false, xhrHeaders: true },
  neutralize: { fields: ['response_code', 'code'], success: 0 },
  stubs: [ /* config.json の stubs と同じ */ ],
  apiPattern: /\/(api|apis|v\d+|dev|internal|graphql)\//i,
};
```

検証：`window.__API_RECON_PRELOAD__ === true` かつ pathname が安定している。

記録結果のエクスポート：

```javascript
JSON.stringify({
  apis: [...window.__API_RECON_LOG__],
  detail: window.__API_RECON_DETAIL__,
  routes: [...(window.__API_RECON_ROUTES__ || [])],
  observe: window.__API_RECON_OBSERVE__,
}, null, 2)
```

---

## D. preload / runtime の Hook 能力

preload（coverage）と runtime_harvest（depth）に組み込まれたブラウザ Hook の能力とカバー範囲：

| Hook 能力 | API 発見への価値 | カバー |
|---|---|---|
| Hook fetch / XHR.open | リクエストの URL/メソッドを記録 | ✅ `recordDetail` + `__API_RECON_LOG__` |
| Hook XHR.setRequestHeader | Authorization などのヘッダを発見 | ✅ `observe.xhrHeaders` |
| Hook localStorage/cookie の読み取り | セッションのキー名を確認 | ⚠️ 任意の `observe.storageReads/cookieReads` |
| Vue でルートを取得 | frontendRoutes を補完 | ✅ `__API_RECON_ROUTES__`（読み込み済みルート） |
| Vue ルートガードの中和 / ログイン遷移のブロック | モジュールを広げて API をトリガー | ✅ `neutralizeVueRouter` + ネイティブ遷移の中和 |
| React でルートを取得 | ルートを補完 | ⚠️ 静的 + クリック；専用 Hook なし |
| ページ遷移のブロック（ログイン path） | ページに留めて分析 | ⚠️ ログイン path のみをブロックし、ビジネスナビゲーションを妨げない |
| Hook 暗号化ライブラリ（CryptoJS/SM など） | 暗号化パラメータ → 平文 API body | ❌ 暗号化関数の入力を手動で Hook する必要がある；結論は config に書く |
| アンチデバッグの bypass | さもないと runtime が API を記録できない | ❌ 手動で処理する必要がある；静的解析は引き続き利用可能 |

---

## E. Endpoint 抽出の正規表現（静的解析が少なすぎる場合）

`harvest_static.py` の `extract_endpoints` を緩めるか、手動で行う：

```bash
grep -rhoaE '"/[a-z][A-Za-z0-9_/\-]{3,}"' js | sort -u
grep -rhoaE '/api/[a-zA-Z0-9_./-]+' js | sort -u
```

---

## F. トラブルシューティング

| 現象 | 原因 → 対処 |
|---|---|
| 静的 API が少ない | endpoint の書き方が一致しない → 正規表現を緩める（D 節） |
| chunk 数 ≪ manifest | CSS のみの chunk か、未デプロイの chunk；404 は再試行済み |
| runtime が依然としてログインページを表示する | レンダリングゲートの誤り → A1 を再確認：キー名、コンテナ、エンコーディング、domain |
| シェルには入るがモジュールが空白 | コンテンツゲート → メニューを forge（A3）；`routes` の path が正しくない可能性 |
| 各ルートが bootstrap/locale のみ | 権限コードが不十分 → I 節の権限ツリー復元；`role_permissions` + `permissions/all` の2つの stub を検査 |
| サイドバーに項目はあるがサブページが空白 | tree に intermediate ノードが欠けているか、code が `userRouteAuth` と一致しない |
| すべての API でログインへ遷移する | インターセプタゲート → `neutralize` を確認；ネストされたフィールドは walk ロジックの拡張が必要 |
| WS フレームが 0 | ユーザー操作の後に初めて subscribe する；`perRouteMs` を延ばす |
| レスポンスボディが空 | `forward: true` の場合のみ実際のレスポンスがある |
| Chromium が存在しない | chromium をインストールするか、`config.chromium` / `CHROMIUM` を設定する |
| Mock が多くてもログインに戻る | Hook が遅すぎるか、`location.href` の setter が欠けている → document-start + preload |
| リストがすべて空 | L3 の空配列は正常；引き続きタブ/設定/詳細をクリック |
| Redux action をルートと誤認する | get/set/change/clear/toggle/upload を含む内部 path をフィルタする |
| Vue が依然としてログインへ遷移する | preload が document-start でない → 注入タイミングを変更；または `neutralizeVueRouter: false` のときに手動でガードをクリアする |
| レスポンスに URL があるのに log に入らない | `extractUrlsFromResponse` を有効にする；または `__API_RECON_DETAIL__` から手動で抽出する |
| Authorization ヘッダ名がわからない | `observe.xhrHeaders` を有効にするか、DevTools でリクエストヘッダを確認する |
| runtime が非常に遅い / タイムアウト | `waitUntil: domcontentloaded` に変更；`routeTimeout` を下げる；`networkidle2` を使わない |
| プロキシ接続に失敗 | `proxy` / 環境変数を確認；Puppeteer と curl のプロキシポートを一致させる |

---

## G. hardened な対象

サーバー側がセッションを段階的に検証する場合（forge 不可能な署名付き cookie、サーバーサイドレンダリングで stub 不可能なメニュー）、runtime は shell で停止する。想定される挙動：

- **静的解析で endpoint の列挙は十分**。モジュールの path はコード内にある
- 認可が許す場合は、**実際のセッション**で同じ harness を実行する：`forward: true`、neutralize は不要、実際の methods/params/responses を取得する

---

## H. 単一タスクのチェックリスト

1. 認可範囲を確認
2. `scripts/harvest_static.py` を**読む** → 対象に合わせて調整 → 実行 → `api_static.txt`、`routes.txt` を確認
3. **Phase 1b**：path のアンカー拡窓 + バインディング層 → `param_candidates.json`（J 節）
4. A1/A2/A3 を逆解析 → サイト専用の `config.json` を書く
5. `runtime_harvest.js` / `preload.js` を**読んで調整してから**実行
6. `runtimeMode=depth`：`npm install` → 調整後の harvest スクリプトを実行
7. `runtimeMode=coverage/both`：document-start で調整後の preload を注入 → browser MCP の動的列挙 + **パラメータトリガーマトリクス**
8. モジュールが描画されない → **I 節の権限ツリー復元** → stubs を patch → 再実行
9. パラメータの複数サンプル diff + エラー逆推定 → `params_merged.json`
10. 統合 → `site_map.json` + `api_merged.txt`。カバレッジ、ギャップ、スクリプトの変更点を正直に明記する

---

## I. 権限ツリーの復元（Phase 4 の深化）

単純な `menus: [{ path, show: true }]` の forge が無効で、サブモジュールが依然として mount されない場合に使用する。

### I1. auth モジュールの特定

```bash
grep -l 'userRouteAuth' js/*.js
grep -l 'routeMap\|routeLink' js/*.js
grep -rhoaE 'getResultTree|role_permissions|permissions/all' js | head
```

記録する：**権限 API の path**、**レスポンスのフィールド名**、**利用側 chunk のファイル名**。

### I2. routeMap の抽出

```bash
python3 scripts/extract_route_map.py recon/js recon/
# 成果物: recon/route_map.json
```

`[!] no routeMap pattern found` の場合：`extract_route_map.py` 内の正規表現を緩めるか、手動で grep する：

```bash
grep -rhoaE '([A-Z_][A-Z0-9_]*):\{name:"[^"]*",link:"/[^"]+"\}' js | head -20
```

### I3. 権限ツリーと stub の構築

```bash
python3 scripts/build_perm_tree.py recon/js recon/ --config recon/config.json
```

スクリプトのロジック：
1. `userRouteAuth={MONITOR:{url:...},...}` を解析する（webpack エイリアス `He=o.DASHBOARD` を含む）
2. `route_map.json` で alias → 実際の path を解決する
3. code のプレフィックスから parent を推定する（`MONITOR_ALERT` → `MONITOR`）
4. `permissions_tree.json`、`permissions_all_stub.json`、`role_permissions_stub.json` を出力する
5. `--config` 指定時に `config.json` の `stubs` と拡張 `routes` を自動的に書き込む

**対象に合わせて調整**（スクリプト先頭で）：
- `DEFAULT_ROOTS`：トップレベルモジュールの code リスト
- `DEFAULT_PREFIX_PARENT`：`PREFIX_` → parent のマッピング
- `DEFAULT_EXTRA_PARENT`：プレフィックス関係にない orphan ノード

### I4. stub の整合性検証

```bash
# permissions の数は userRouteAuth のエントリ数とほぼ一致する
wc -l recon/perm_codes_all.txt
# routes は route_map のすべての link をカバーする
python3 -c "import json; m=json.load(open('recon/route_map.json')); r=set(json.load(open('recon/config.json'))['routes']); print('missing', [v['link'] for v in m.values() if v['link'] not in r])"
```

### I5. runtime を再実行して比較

```bash
node recon/runtime_harvest.js recon/config.json
# forge 前後の runtime_api.json のエントリ数を比較；/attack、/asset などにモジュール API が現れるか確認
```

| forge 前 | forge 後（成功） |
|---|---|
| 各ルートで同じ 3–5 件の bootstrap | ルートごとに異なる module API がトリガーされる |
| `/api/locale/language` のみ | `/api/web/...` のモジュール endpoint が現れる |
| `routes.txt` のルートが 1 桁 | `routes` は route_map 由来で 80–110 以上 |

### I6. それでも失敗する場合

- **coverage モード**：サイドバー + タブをクリック。権限の gating は操作後に初めてリクエストされる可能性がある
- **stub フィールド**：実際の API（curl + 実際の session）と stub の nesting を比較
- **追加ガード**：`hasPermission|checkRole|func.` などのボタンレベル検査を grep し、`role_permissions.permissions` を拡張する
- **静的解析のフォールバック**：モジュール API の path は `api_static.txt` に残る。runtime は METHOD/body を補完するのみ；パラメータは `param_candidates.json` + 記録済みサンプルを保持

---

## J. パラメータの逆解析（Phase 1b / 5b / 5c）

**方法論であり、汎用スクリプトではない。** path を探すには正規表現を使用；パラメータを探すにはアンカー拡窓 + UI バインディングチェーン + 複数サンプル diff + エラー逆推定を使用する。

### J1. アンカー拡窓 — path から組み立てオブジェクトを探す

```bash
# Phase 1 で判明した path をアンカーとする
grep -n '"/api/user/list"' js/*.js
grep -rhoaE '.{0,120}("/api[^"]+").{0,200}' js | head
grep -rhoaE '(params|data|body|payload)\s*:\s*\{' js | head
grep -rhoaE '(get|post|put|delete|patch)\([^,]+,\s*\{' js | head
```

### J2. ラッパー層と転送形態

```bash
# axios / 共通 request
grep -rhoaE '(axios|request)\.(get|post|put|delete|patch)\(' js | head
grep -rhoaE 'interceptors\.(request|response)' js | head

# GraphQL
grep -rhoaE '(query|mutation)\s+\w+|gql`|graphql\(' js | head
grep -rhoaE '\$[a-zA-Z_]+\s*:\s*(Int|String|Boolean|\[)' js | head

# FormData / multipart
grep -rhoaE 'FormData|\.append\(' js | head

# パスパラメータ
grep -rhoaE 'path:\s*"/[^"]*:[^"]+"' js | head
grep -rhoaE 'useParams|route\.params|\$route\.params' js | head
```

### J3. 検証ゲート — 必須 / 形式 / 列挙

```bash
grep -rhoaE '(required|message|pattern|enum|validator)\s*:' js | head
grep -rhoaE 'yup\.|zod\.|async-validator|Form\.Item|a-form-item|el-form-item' js | head
grep -rhoaE 'rules\s*:\s*\[|name:\s*["\'][a-zA-Z_]+["\']' js | head
grep -rhoaE 'label.*value|options\s*:\s*\[' js | head
```

### J4. バインディング層 — フォーム → API

```bash
grep -rhoaE 'onFinish|handleSubmit|getFieldsValue|validateFields' js | head
grep -rhoaE '(pick|omit|transform|dayjs|moment)\(' js | head
```

runtime での補完：DevTools → Network → リクエスト → **イニシエーター**（call stack）で `fetch`/`send` から上へ遡って組み立て関数を追う。

### J5. 暗号化パラメータ

```bash
grep -rhoaE 'encrypt|decrypt|sign|CryptoJS|sm2|sm3|sm4|RSA|AES' js | head
```

**暗号文の上でフィールドを推測しない**。暗号化関数の**入力**を Hook し、暗号化前に plaintext payload を記録する。結論は `config.json` / `param_candidates.json` に書く。

### J6. パラメータトリガーマトリクス（Phase 3 で必須）

各モジュールについて操作ごとに 1 回ずつ記録し、リクエストの body/query を diff する：

| 操作 | 注目点 |
|---|---|
| リスト初回表示 | ページネーションのデフォルト値 |
| 検索 | keyword、filters |
| 高度なフィルタ | optional フィールド |
| 新規作成/編集 | 完全な entity |
| 一括/エクスポート | `ids[]`、`exportType` |
| 並べ替え/ページ移動 | `sortField`、`order` |

成果物 `param_samples.json`：`[{ "path", "method", "action": "search", "body", "query", "headers" }]`

### J7. 信頼度ルール

| 信頼度 | 条件 |
|---|---|
| **高** | 静的 callsite + runtime のサンプル 2 件以上が一致 |
| **中** | 静的なみ、または runtime 1 回のみ |
| **低** | レスポンス/エラーからの逆推定で、二次検証なし |
| **トリガー待ち** | 静的解析で判明済みのフィールドで、UI/権限がそこまで到達していない |

### J8. シナリオ別クイック設定

| シナリオ | 手順 |
|---|---|
| REST リストページ | J1 組み立てオブジェクト → J6 で 4 回 diff → J3 の rules |
| 新規作成/編集フォーム | J3 の Form name → J4 の submit チェーン → runtime で送信 + 意図的に空欄にして 400 を確認 |
| GraphQL | J2 の variables 宣言 → runtime で各 operation の variables を記録 |
| 暗号化 body | J5 で入力パラメータを Hook → 暗号化前のフィールドが実際の params |

### J9. api-recon のフェーズとの対応

| api-recon | パラメータ recon |
|---|---|
| Phase 1 静的 | J1 アンカー拡窓 |
| Phase 2 A2 インターセプタ | グローバル注入フィールド（tenantId、sign） |
| Phase 3 runtime | J6 トリガーマトリクス + `param_samples.json` |
| Phase 4 権限ツリー | モジュールごとにフォームが異なる → 権限が十分な場合のみ全フィールドがトリガーされる |
| Phase 5 統合 | `params_merged.json` + 信頼度；単一サンプルで必須を確定しない |

### J10. トラブルシューティング

| 現象 | 対処 |
|---|---|
| 静的解析にフィールド名があるのに runtime で一度も現れない | 「トリガー待ち」と明記；権限ツリーを補完 / 高度なフィルタをクリック / 連動 select の各 option を確認 |
| 同じ path で body の形状が異なる | 正常。`action` ごとに分けて記録し、schema を無理に統合しない |
| stub のレスポンスは偽物だが params を確認したい | **outbound リクエストの** body/headers を見る。stub のレスポンスから逆推定しない |
| 400 で nested field が報告される | 外側のラッパー `data`/`bizData`/`variables` に注意 |
| GraphQL で operation 名しか見えない | `variables` JSON を展開；静的解析で `$var: Type` を探す |

---
