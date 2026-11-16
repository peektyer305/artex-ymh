# syntax=docker/dockerfile:1
#
# 実行イメージ（イメージ内ではビルドしない）：定番ツールのみを入れ、**ビルド済みの Linux 単一バイナリ**を配置する。
# バイナリは CI の binaries job でクロスコンパイルし（純 Go・QEMU 不使用）、ターゲットアーキテクチャ別に
# ビルドコンテキストの dist/<TARGETARCH>/artex へ置く。これによりマルチアーキテクチャビルド時、arm64 が
# エミュレートするのは apt 層のみとなり、Next/Go のコンパイルを行わないため大幅に速くなる。
#
# ローカルで手動ビルドする場合は、先にバイナリを用意する：
#   cd web && npm run build:static && cd ..
#   cp -r web/out server/webui/dist
#   CGO_ENABLED=0 GOARCH=amd64 go build -tags embedui -o dist/amd64/artex ./cmd/artex
#   docker build -t artex:local .
FROM python:3.12-slim-bookworm
ARG TARGETARCH
# 基本ツール：ripgrep / curl / vim に加え、偵察用の定番ツールを一式入れる（必要に応じて増減）。
# Node は NodeSource から 20.x を導入する。bookworm 標準の apt nodejs は 18 で、Playwright は >=20 を要求する。
# setup_20.x は NodeSource の**可変スクリプト**（内容は上流の更新で変わる）で、これをそのままパイプ実行するのは
# 毎回のビルドで上流を盲信するのと同じ。そこで先にファイルへ落とし、固定した SHA256 と照合してから実行する（fail-closed）：
# ハッシュが一致しなければ上流がスクリプトを改変した証拠なので、まず diff を監査してハッシュ更新の可否を判断する。盲目的に更新しない。
# ハッシュ取得日は 2026-09-13。代替案：nodejs 公式 tarball（nodejs.org、固定
# バージョン + 公式 SHA256SUMS 署名ファイル）へ切り替え、第三者リポジトリのスクリプトを完全に回避する。
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates ripgrep curl wget vim git jq unzip \
      dnsutils iputils-ping netcat-openbsd inetutils-telnet whois nmap \
    && curl -fsSL https://deb.nodesource.com/setup_20.x -o /tmp/nodesource_setup.sh \
    && echo "2c4c6683a17b6f4128898a7b521e3c8bb725a99ffaf1b5e32ac97c6fa7d381be  /tmp/nodesource_setup.sh" | sha256sum -c - \
    && bash /tmp/nodesource_setup.sh \
    && apt-get install -y --no-install-recommends nodejs \
    && rm -f /tmp/nodesource_setup.sh \
    && rm -rf /var/lib/apt/lists/*
# Playwright MCP と CLI をグローバルに事前インストールし、実行時に npx でネットワーク取得しないようにする。
# バージョンは固定する（2026-09-13 に npm registry の latest から取得した値）：@latest を使うと同一の Dockerfile から
# ビルド時期によって異なるイメージができてしまう。アップグレード時は暗黙のドリフトに任せず、意図的に bump して記録する。
# @playwright/mcp：browser MCP は `npx @playwright/mcp` を直接使う（グローバル導入済みのため -y/@latest は不要）。
# @playwright/cli：playwright-cli を提供する。インストール後に --help で実行できることを確認する。
# さらに playwright（ブラウザ管理を提供）を導入し、--with-deps で chromium とそのシステム依存を事前に揃える。
# これによりコンテナ内の MCP/CLI は初回起動から使え、ブラウザのネットワークダウンロードも不要になる。
RUN npm install -g @playwright/mcp@0.0.80 @playwright/cli@0.1.19 playwright@1.63.0 \
    && playwright-cli --help \
    && playwright install --with-deps chromium \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /app
# ビルド済みの該当アーキテクチャ向けバイナリ（dist/amd64/artex または dist/arm64/artex）
COPY dist/${TARGETARCH}/artex /app/artex
# スーパーバイザ起動スクリプト：プロセス終了後、終了コードに応じて再起動するかを判断する。画面からのワンクリック更新によるバイナリ差し替えもこれが担う。
# また SIGTERM を artex へ転送する役割も持つ —— docker stop は信号を PID 1 にしか送らないため、
# 転送しないと artex は受け取れず、正常終了もできず 10 秒後に SIGKILL で強制終了される。
COPY start.sh /app/start.sh
RUN chmod +x /app/artex /app/start.sh
COPY skills/ /app/skills/
# data/（SQLite + jwt.key）の永続化ポイント
VOLUME ["/app/data"]
EXPOSE 8787 8788
ENTRYPOINT ["/app/start.sh"]
CMD ["-addr", ":8787", "-proxy", ":8788"]
