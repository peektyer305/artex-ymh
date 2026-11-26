# Dockerのビルド対象

`Dockerfile`には、ソースからビルドする`source`と、ビルド済みバイナリを配置する`prebuilt`があります。指定を省略すると`prebuilt`になります。実行環境のパッケージ導入は共通の`runtime`にあります。

ソースから作る場合は、次を実行します。

```bash
docker build --target source -t artex-ymh:local .
```

`prebuilt`を使う場合は、対象のCPUに合わせた実行ファイルを`dist/amd64/artex`または`dist/arm64/artex`に置き、次を実行します。

```bash
docker build --target prebuilt -t artex-ymh:prebuilt .
```

通常のDocker Composeは`source`を使用します。外部のAnsibleなどからビルドする場合も、利用するターゲットを明示してください。
