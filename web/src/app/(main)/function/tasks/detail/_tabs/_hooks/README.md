# セッション画面の修正箇所

`sessions-tab.tsx` は表示部品を組み合わせます。イベント処理と表示用の値は `use-task-sessions.ts` でまとめます。

| 変更したい動作 | ファイル |
|---|---|
| 状態と参照の初期値 | `use-task-session-state.ts` |
| 履歴のページ取得、SSEの開始・終了、受信データの反映 | `use-session-history.ts` |
| トークン数、実行状態、承認待ちの定期取得 | `use-session-runtime-polling.ts` |
| Workerの一覧、関連アセットの定期取得 | `use-worker-session-polling.ts` |
| 添付ファイルのアップロード | `use-session-attachments.ts` |
| メッセージ送信と停止 | `use-session-message-actions.ts` |
| 新規セッション作成とWorkerの制御 | `use-session-actions.ts` |
| スクロールの追従と位置の記録 | `use-session-scroll.ts` |

表示を変える場合は `../_components/` の該当部品を編集します。履歴の統合やセッションIDの変換は `../_lib/session-model.ts` に置き、Reactに依存せずテストできるようにしています。
