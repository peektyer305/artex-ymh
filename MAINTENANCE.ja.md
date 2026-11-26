# 保守と診断

## 設定を確認する

設定ファイルの場所は`config.Path()`で決まります。`ARTEX_CONFIG`、作業ディレクトリの`config.json`、実行ファイル付近の`config.json`の順に確認します。

DB接続先は`config.PostgresDSN()`が決め、`db.DSN()`はそれを呼びます。`ARTEX_PG_DSN`を設定すると、ファイル内のDB設定より優先されます。起動時はJSONの形式と読み取りエラーを確認します。

関連するコードは[config/config.go](config/config.go)、[cmd/artex/main.go](cmd/artex/main.go)、[cmd/artex/doctor.go](cmd/artex/doctor.go)です。

## doctorを実行する

```bash
./artex doctor
```

| 表示 | 意味 |
|---|---|
| OK | 確認できました。 |
| WARN | 設定や状態を確認してください。 |
| FAIL | 確認に失敗しました。理由を直して再実行してください。 |
| SKIP | 前提条件を満たさず、確認していません。 |

FAILが0件でも、SKIPがあれば未確認の項目が残ります。接続文字列や認証情報が含まれるログを共有する際は、その値を伏せてください。

## 更新する

データとDBのバックアップを取得した後、ソースを更新し、[README](README.md)の利用方式に合わせて再ビルドします。Dockerは`docker compose up -d --build`で更新します。

## 開発資料

[ドキュメント一覧](docs/README.md)から、構成、テスト、DBの変更方法を確認できます。
