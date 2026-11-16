# `/btw` 検証記録

日付：2026-09-10。ブランチ：`codex/btw-side-question`。ベースライン：`8dae851b9b622f2ff2631f332fde9719d0b16fba`。

独立した PostgreSQL テスト用データベースとデータディレクトリを使用した。実際のモデル認証情報は独立したテスト環境にのみ注入しており、コードやこの記録には書き込んでおらず、製品のデフォルトモデルも変更していない。Go 1.26.3、norma v0.3.6、Next.js 16.2.9。

実際のモデル対話、返却オブジェクト、エンジニアリングアサーション、および Qwen の生のレビューテキストは [validation-2026-09-10.json](validation-2026-09-10.json) に保存されている。ここに API 認証情報は含まれない。

## エンジニアリング検証

| 範囲 | 結果 | エビデンス |
| --- | --- | --- |
| 構造化メッセージ、ツールパラメータのディープコピー | 合格 | `TestCheckpointDeepCopyAndBoundaries` |
| 要約 / 圧縮リクエストを上書きしないこと、完全な応答と終端状態の公開、部分応答の除外 | 合格 | `TestCheckpointDeepCopyAndBoundaries`、`TestSnapshotExcludesPartialStreamAndSelectsPoolMember` |
| 実際のモデルプールメンバーの識別 | 合格 | `TestSnapshotExcludesPartialStreamAndSelectsPoolMember` |
| ツールのペアリング、20 組のリプレイ、予算の刈り込みと上限超過エラー | 合格 | `TestBuildRequestCompactionToolPairingAndBudget` |
| メインとサイドクエスチョンの並列実行、双方向キャンセルの分離 | 合格 | ブロッキング方式の Provider、`TestMainSideConcurrencyAndIndependentCancellation` |
| ツール実行なし、ストリーミング / 非ストリーミング、失敗時の既存使用量 | 合格 | `TestServiceNoToolsAndUsageOnFailure` |
| 実際の norma ChatAgent + ローカル Read ツール、メイン transcript / アクティビティの分離 | 合格 | `TestSideActualChatCheckpointToolResultAndTranscriptIsolation`、ストリーミングと非ストリーミングのサブケース |
| 永続化、ページング、冪等性、再起動後の部分回答の保持 | 合格 | `TestSideHistoryIdempotencyPagingAndRecovery` |
| クリアと遅延書き込みの競合、親リソースの削除、バージョン比較 | 合格 | `TestSideClearLateWritersAndDeletedParent` |
| MainAgent / Worker のアーカイブと復元、v1/v2/v3 | 合格 | `TestSideTaskArchiveVersions` |
| 3 種類の親 API、認証、リソースの帰属、Worker の論理削除 | 合格 | `TestSideHTTPGlobalLimitTaskWorkerAndDeletion`、`TestSideCheckpointPersistsBeforeAdmissionAndRestart` |
| ビジー状態のメインセッションでもサイドクエスチョン可能、独立した SSE の再接続 / 切断、キャンセル、クリア | 合格 | `TestSideHTTPBusyIsolationClearAndReconnect` |
| 親セッションあたり 1 / 全体 4 の同時実行 | 合格 | 2 つの `TestSideHTTP…` テストケース |
| 送信前のスナップショット永続化、再起動後の継続質問、旧セッションではスナップショットを偽造できないこと | 合格 | `TestSideCheckpointPersistsBeforeAdmissionAndRestart` |
| キャッシュ内の設定が削除された、またはモデルが変更された後に継続を拒否する | 合格 | `TestSideRejectsDeletedOrChangedCachedProfile` |
| アーカイブ前のキャンセルと、最終回答および使用量の永続化待ち | 合格 | `TestSideTaskDrainPersistsBeforeArchive` |
| ストリーミングのコンシューマが早期にキャンセルした場合に、使用量とサイドクエスチョンの帰属を 1 回だけ記録する | 合格 | `TestSideUsageRecordedOnceOnConsumerCancellation` |
| 再起動時に自動復元された Worker / deadline の実行コンテキストが新しいスナップショットを公開し続ける | 合格 | `TestSideRestoredWorkerRuntimePublishesNewCheckpoint` |
| 関連パッケージの race チェック | 合格 | 下記のコマンド |
| TypeScript と本番ビルド | 合格 | `npx tsc --noEmit`、`npm run build` |
| 追加したフロントエンドモジュールの Biome | 合格 | `biome check`、追加モジュール 3 件 |

使い捨ての独立したデータベースで `ARTEX_PG_DSN` を設定すれば、自動チェックを再現できる（本番データベースを指さないこと）：

```sh
go test -race ./agent ./db ./server ./sidequestion ./llmrec ./llmpool \
  -run 'Test(Side|Checkpoint|Snapshot|BuildRequest|Service|MainSide|CaptureRun|TaskArchive|CompleteForwards|StopIntent|CancelIntent)' -count=1
cd web
npx tsc --noEmit
npx biome check src/lib/side-questions.ts src/hooks/use-side-questions.ts src/components/side-question-workspace.tsx
npm run build
```

Go の全量リグレッションは全緑ではない。`server` パッケージには、一時ディレクトリのクリーンアップ段階で失敗する既存テストが 2 件あり、いずれも `TempDir RemoveAll … directory not empty` を報告する：

- `TestInheritedActivityDetailAndRelationDeletion`
- `TestTaskMetadataPatchReturnsRenameAndPin`

上記の未変更ベースラインからソースをエクスポートし、同じ分離環境で `server` パッケージを再実行しても、この 2 件のクリーンアップ失敗が再現した。ベースラインの実行ではさらに `TestCoreTaskLifecyclePG` の目標ノード数アサーションの失敗が発生したが、最終変更後の `server` リグレッションではこのアサーション失敗は発生していない。ほかのパッケージは合格し、今回のサイドクエスチョン関連のテストケースおよび race チェックも合格した。ベースラインの問題を今回の受け入れ合格として扱っておらず、問題を隠すために既存のアサーションを変更してもいない。

Next.js のビルドでは既存の複数 lockfile / workspace root 推論の警告が出力されるが、ビルドは完了し、すべてのページの生成に成功した。

## ブラウザチェック

Codex In-app Browser を使用し、独立したローカル Go サービスと Next.js 開発サーバーに接続した。デスクトップと 390 × 844 の狭い画面で以下を人手で自動操作し、スクリーンショットとブラウザログを確認した：

- 通常のチャット実行中に `/btw` を入力すると、メインの内容とサイドクエスチョンが同時に表示される。デスクトップのサイドバーも正常。
- 連続して追加質問する。サイドクエスチョンを停止した後も生成済みの部分は保持され、メインの処理は継続する。
- パネルを閉じてもリクエストは継続し、再度開くと完了した回答が復元される。ページを更新すると、空の `/btw` でも履歴が復元される。
- 狭い画面の Drawer では、入力、ボタン、履歴、閉じる操作が正常で、横方向のオーバーフローもない。
- クリアは確認ダイアログを使用し、クリア後は履歴が消えるが、メイン transcript とスナップショットは保持される。
- タスクの MainAgent と 2 つの Worker でそれぞれ質問して切り替えても、Agent タグと履歴が混線しない。
- ブロッキング方式のローカルモデルフィクスチャで Worker を実行状態に保つ。Worker のメイン入力欄から `/btw` を送信し、サイドクエスチョンを停止しても Worker はリアルタイム実行中と自身の一時停止ボタンを表示し続け、サイドクエスチョンは部分回答を保存する。
- ブラウザのエラー / 警告ログは空だった。

制御可能なフィクスチャは同時実行のタイミングを正確に検証するために用い、実際のモデルの出力速度には依存しない。デバッグ中、Worker 実行時のチェックを 2 回行ったがいずれも有効な同時実行の窓が形成されなかった（タスクが終了済み / 回答が早期に終了）。フィクスチャを修正してやり直し、合格した。これらの初回の操作は有効な合格として記録していない。

## 実際のモデルとの対話

まず `grok-4.6` をプローブした。OpenAI 互換 API は `http://127.0.0.1:12580/tingly/openai`。プローブは HTTP 200 で、モデル名 `grok-4.6` と `READY` を返し、所要時間は 2.82 秒だった。第一候補が利用可能だったため、Tingly `glm` や智谱 `glm-5.3` のフォールバックチェーンは有効にしていない。この 2 つのバックアップサービスは今回検証していない。

| シナリオ | 実際の結果 |
| --- | --- |
| メインセッション実行中にアセット、ゴール、マーカーを質問 | `redhaze.top`、トップページの読み取りとゴールの要約、`BTW-REAL-0910` を返した。サイドクエスチョンは完了、16.97 秒 |
| メインセッションがトップページの読み取りを完了した後にツールの根拠を質問 | WebFetch 200、curl のリダイレクト 301 → 302 → 200、ページタイトルを正しく引用。7.24 秒 |
| サイドクエスチョンが Bash でテストファイルの作成を要求 | 実行を拒否し、対象ファイルは作成されなかった。7.74 秒 |
| 完了後のサイドクエスチョンはメインのコンテキストを変更しない | メイン transcript の SHA-256 がメインのアクティビティ記録と一致。サイドクエスチョンのツール実行回数は 0 |
| Go サービスを実際に停止 / 再起動した後に追加質問 | 以前のサイドクエスチョン履歴 3 件を保持し、永続化スナップショットから直接アセット、マーカー、タイトルに回答。メイン Agent は再実行していない |
| 新しいセッションで Grok の非ストリーミング設定を使用 | アセットと `ATOMIC-0910` に正しく回答。使用量を返却して保存：input 11734、output 138、cache_read 11520 |

アセットのケースでは、メインセッションが WebFetch と Bash/curl で公開トップページを読み取り、ランディングページは `https://id.redhaze.top/home`、タイトルは「红幕科技 RedHaze Group · 全球综合集团门户」だった。Bash はレスポンスをローカルのテストファイルに一時保存し、リモートへの書き込みは実行していない。この事実は「サイドクエスチョンがツールを実行していない」ことと分けて検証した。

メイン transcript のチェックサム値：`e7e61f135a4a120954b539f357e8c4205d7d5cd7460dcaf3dc0fd066463e1d00`。

**使用量の制限：** Tingly の Grok ストリーミングレスポンスは usage を返さなかった。別途 `stream_options.include_usage=true` を直接送信して検証したところ、HTTP 200、データフレーム 12 個、usage フレーム 0 個だった。したがってストリーミングテストにおける 0 は、エンドポイントが使用量を提供していないことを意味し、課金されていないと解釈することはできない。非ストリーミングの使用量、およびフィクスチャの失敗 / キャンセル時の使用量はいずれも正しく保存された。

## Qwen レビュー

レビューモデルは `qwen-flash`、OpenAI 互換 API は `https://dashscope.aliyuncs.com/compatible-mode/v1`、HTTP 200。実際のサイドクエスチョン対話のうち最初の 3 件、メインセッションのツール根拠、エンジニアリングアサーションを提供した。返却値は `verdict: accept`、`concerns: []` で、回答がアセット、マーカー、ページ読み取りのエビデンスと一致し、サイドクエスチョンでのツール拒否が制約に適合していると判断した。レビューの使用量：prompt 6625、completion 312、total 6937。

今回の Qwen レビューの範囲には、後から追加したサービス再起動と非ストリーミングのテストは含まれない。Qwen の「書き込みなし」という要約は範囲が広すぎる。メインセッションの curl は実際にレスポンスのローカル一時ファイルを作成しており、上記で明確に記録している。同時実行、ツール実行ゼロ、transcript の分離はエンジニアリングアサーションで判断し、モデルレビューは回答品質の評価を補助するだけである。
