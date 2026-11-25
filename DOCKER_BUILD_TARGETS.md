<!-- artex-unified-build-targets-v1 -->
# Docker のビルド対象

Dockerfile は1つです。ソースから作る場合は source ターゲットを指定します。
旧ソース用 Dockerfile を参照していた Compose は、Dockerfile と target: source に変更しています。
ビルド済みバイナリを使う場合は prebuilt ターゲットを指定します。省略時も prebuilt です。

ソースからビルドする場合

    docker build --target source -t artex-ymh:local .

dist/<TARGETARCH>/artex を配置してから、ビルド済みバイナリを組み込む場合

    docker build --target prebuilt -t artex-ymh:prebuilt .

両方のターゲットは、旧ソース版の実行環境を共有します。
旧 Dockerfile の NodeSource 20.x は、Node.js 22.14.0 の tarball に切り替わります。
パッケージ構成も旧ソース版に揃うため、prebuilt 側にも build-essential が含まれます。
Node.js のアーカイブは TARGETARCH に応じて amd64/arm64 を選択します。
一覧ファイルは /app/packaging/tools-manifest.json に配置します。

この変更スクリプトは Docker コマンドを実行しません。
実際のイメージビルドとアプリの起動は別途確認してください。
リポジトリの外にある Ansible などの設定が旧ソース用 Dockerfile を参照している場合は、
Dockerfile と source ターゲットに変更する必要があります。
