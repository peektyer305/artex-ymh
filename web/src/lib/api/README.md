# API の追加・修正方法

各機能の通信処理は同名のファイルに置きます。タスクは `tasks.ts`、LLM 設定は `llm.ts` に追加します。

共通の HTTP 処理は `transport.ts`、複数の API で使う検索条件は `query.ts` に置きます。公開するメソッドは `index.ts` に登録します。

既存の `@/lib/api` からの import は引き続き使えます。型を追加するときは `../types/` の該当ファイルに定義してください。
