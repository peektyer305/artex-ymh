#!/usr/bin/env bash
# ARTEX インストールスクリプト：① 全て Docker  ② ローカルでビルド・実行
set -euo pipefail
cd "$(cd "$(dirname "$0")" && pwd)"

info(){ printf '\033[36m[*]\033[0m %s\n' "$*"; }
ok(){   printf '\033[32m[+]\033[0m %s\n' "$*"; }
warn(){ printf '\033[33m[!]\033[0m %s\n' "$*"; }
die(){  printf '\033[31m[x]\033[0m %s\n' "$*" >&2; exit 1; }
ask(){  local p="$1" d="${2:-}" a; read -rp "$p${d:+ [$d]}: " a; echo "${a:-$d}"; }
rand(){ head -c 18 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | head -c 24; }

# ── docker 環境検出 / 自動インストール ───────────────────
ensure_docker(){
  if command -v docker >/dev/null 2>&1 && docker compose version >/dev/null 2>&1; then
    ok "docker と docker compose を検出しました"; return
  fi
  warn "docker / docker compose が見つかりません"
  case "$(uname -s)" in
    Linux)
      if [ "$(ask 'Docker を自動インストールしますか（y/n）' y)" = y ]; then
        curl -fsSL https://get.docker.com | sh
        sudo usermod -aG docker "$USER" || true
        ok "Docker のインストールが完了しました（グループ変更の反映には再ログインが必要です）"
      else
        die "docker をインストールしてから再試行してください"
      fi ;;
    Darwin) die "macOS では Docker Desktop をインストールしてください：https://www.docker.com/products/docker-desktop/" ;;
    *)      die "docker をインストールしてから再試行してください" ;;
  esac
}

# ── ① 全て Docker ───────────────────────────────
install_docker(){
  ensure_docker
  if [ ! -f .env ]; then
    cp .env.example .env 2>/dev/null || true
    local pw key
    pw="$(ask 'Postgres パスワード（Enter でランダム生成）' "$(rand)")"
    key="$(ask 'ANTHROPIC_API_KEY（空欄可、後で UI から設定）' '')"
    sed -i.bak "s|^POSTGRES_PASSWORD=.*|POSTGRES_PASSWORD=${pw}|" .env
    sed -i.bak "s|^ANTHROPIC_API_KEY=.*|ANTHROPIC_API_KEY=${key}|" .env
    rm -f .env.bak
    ok ".env を生成しました（POSTGRES_PASSWORD 設定済み）"
  else
    info "既存の .env を使用します"
  fi
  info "ローカルの Dockerfile.source からイメージをビルドして起動します…"
  docker compose up -d --build
  ok "起動しました → http://localhost:8787"
  info "ログ確認：docker compose logs -f artex"
}

# ── ② ローカルでビルド・実行 ──────────────────────────────
install_local(){
  echo "データベースの準備方法："
  echo "  1) 既存の PostgreSQL に接続"
  echo "  2) Docker で PostgreSQL を起動（docker が必要）"
  case "$(ask '番号を選択' 1)" in
    2)
      ensure_docker
      local pw; pw="$(ask 'Postgres パスワード（Enter でランダム）' "$(rand)")"
      docker run -d --name artex-pg -p 5432:5432 \
        -e POSTGRES_USER=artex -e POSTGRES_PASSWORD="$pw" -e POSTGRES_DB=artex \
        -v artex-pg:/var/lib/postgresql/data postgres:16-alpine
      DB_HOST=127.0.0.1 DB_PORT=5432 DB_USER=artex DB_PASS="$pw" DB_NAME=artex DB_SSL=disable ;;
    *)
      DB_HOST="$(ask 'データベースのアドレス' 127.0.0.1)"
      DB_PORT="$(ask 'ポート' 5432)"
      DB_USER="$(ask 'アカウント' artex)"
      DB_PASS="$(ask 'パスワード' '')"
      DB_NAME="$(ask 'データベース名' artex)"
      DB_SSL="$(ask 'sslmode (disable/require)' disable)" ;;
  esac

  # config.json を生成
  cat > config.json <<JSON
{
  "database": {
    "host": "${DB_HOST}",
    "port": ${DB_PORT},
    "user": "${DB_USER}",
    "password": "${DB_PASS}",
    "dbname": "${DB_NAME}",
    "sslmode": "${DB_SSL}"
  }
}
JSON
  ok "config.json を生成しました"

  # go 環境の確認
  command -v go >/dev/null 2>&1 || die "Go が見つかりません。先に Go（>=1.26）をインストールしてください：https://go.dev/dl/"
  ok "Go: $(go version)"

  # フロントエンドの埋め込みには node による静的成果物の生成が必要
  if command -v npm >/dev/null 2>&1; then
    info "フロントエンドの静的成果物をビルドします…"
    ( cd web && npm ci && npm run build:static )
    # dist は全体が gitignore 対象。新規クローンでは親ディレクトリが存在しないことがあるため、先に作成してからコピーする（2026-09 実測で発生）
    mkdir -p server/webui/dist && rm -rf server/webui/dist && cp -r web/out server/webui/dist
    info "フロントエンド埋め込みの単一バイナリをビルドします…"
    CGO_ENABLED=0 go build -tags embedui -trimpath -o artex ./cmd/artex
  else
    warn "npm が見つかりません：**フロントエンドを埋め込まない**バックエンドをビルドします（フロントエンドは別途 npm run dev）"
    CGO_ENABLED=0 go build -o artex ./cmd/artex
  fi
  ok "ビルド完了 → ./artex"

  # 任意：systemd に管理を委ねる（クラッシュ時に自動再起動 + 起動時自動開始）。監視の役割は systemd に移し、
  # start.sh は経由しない。ページのワンクリック更新（終了コード 75）は Restart=on-failure で再起動される。
  if [ "$(uname -s)" = Linux ] && command -v systemctl >/dev/null 2>&1; then
    if [ "$(ask 'systemd サービスとしてインストールし起動時に自動開始しますか（y/n）' y)" = y ]; then
      install_systemd
      return
    fi
  fi

  info "起動します…（Ctrl-C で終了）"
  ./artex
}

# ── ③ 任意：systemd による管理 ────────────────────────
install_systemd(){
  local svc_user="${SUDO_USER:-$(id -un)}"
  asroot(){
    if [ "$(id -u)" -eq 0 ]; then "$@"; else sudo "$@"; fi
  }
  if ! id -u "$svc_user" >/dev/null 2>&1; then
    svc_user="$(id -un)"
    warn "ユーザー ${SUDO_USER:-} が存在しないため、現在のユーザー $svc_user を使用します"
  fi

  # 環境変数テンプレート：存在しない場合のみ生成し、既存設定の上書きを回避
  if [ ! -f /etc/artex.env ]; then
    asroot tee /etc/artex.env >/dev/null <<'ENV'
# ARTEX 環境変数（すべて任意）。変更後は systemctl restart artex で反映されます。
# リバースシェル / トンネルのコールバック先アドレス（host:port）：
# ARTEX_CALLBACK_ADDR=1.2.3.4:8787
ENV
    asroot chmod 600 /etc/artex.env
    ok "/etc/artex.env のテンプレートを生成しました"
  else
    info "既存の /etc/artex.env を使用します"
  fi

  # テンプレートから unit を生成：インストールディレクトリと実行アカウントを置換
  sed -e "s|^User=.*|User=${svc_user}|" \
      -e "s|^WorkingDirectory=.*|WorkingDirectory=${PWD}|" \
      -e "s|^ExecStart=.*|ExecStart=${PWD}/artex -addr :8787 -proxy :8788|" \
      packaging/artex.service | asroot tee /etc/systemd/system/artex.service >/dev/null
  ok "/etc/systemd/system/artex.service をインストールしました（User=${svc_user}, ディレクトリ=${PWD}）"

  asroot systemctl daemon-reload
  asroot systemctl enable --now artex
  ok "ARTEX は systemd 管理になりました → http://localhost:8787"
  info "ログ確認：journalctl -u artex -f"
}

echo "=============================="
echo "  ARTEX インストール"
echo "  1) 全て Docker でインストール"
echo "  2) ローカルで実行（go ビルド）"
echo "=============================="
case "$(ask '番号を選択' 1)" in
  1) install_docker ;;
  2) install_local ;;
  *) die "無効な選択です" ;;
esac
