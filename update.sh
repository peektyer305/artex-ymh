#!/usr/bin/env bash
# ARTEX 更新スクリプト：① Docker 更新（ローカルの Dockerfile.source から再ビルド）  ② ローカルビルド更新（バイナリを再構築）
# install.sh と対になる：install は初回導入、update は新バージョンへのアップグレードを担当。
# DB マイグレーションは手動実行不要。artex は起動のたびに schema.sql を冪等に再実行する（ADD COLUMN/CREATE
# INDEX IF NOT EXISTS を含む）ため「再起動＝マイグレーション」。データ（pgdata ボリューム、./data、./skills）は影響なし。
set -euo pipefail
cd "$(cd "$(dirname "$0")" && pwd)"

info(){ printf '\033[36m[*]\033[0m %s\n' "$*"; }
ok(){   printf '\033[32m[+]\033[0m %s\n' "$*"; }
warn(){ printf '\033[33m[!]\033[0m %s\n' "$*"; }
die(){  printf '\033[31m[x]\033[0m %s\n' "$*" >&2; exit 1; }
ask(){  local p="$1" d="${2:-}" a; read -rp "$p${d:+ [$d]}: " a; echo "${a:-$d}"; }

# ── 任意：リポジトリを最新コードに同期（compose/スクリプト/ローカルビルドのソースはすべてこれで更新）───────
sync_repo(){
  [ -d .git ] && command -v git >/dev/null 2>&1 || { warn "git の作業コピーではないため、git pull をスキップします"; return; }
  [ "$(ask '最新コードを取得しますか（git pull --ff-only）（y/n）' y)" = y ] || return
  if ! git pull --ff-only; then
    warn "git pull を fast-forward できませんでした（ローカルに変更があるかブランチが分岐）。手動で対処してから再試行してください。今回は現在のコードを使用します"
  fi
}

# ── ① Docker の更新 ───────────────────────────────
update_docker(){
  command -v docker >/dev/null 2>&1 && docker compose version >/dev/null 2>&1 \
    || die "docker / docker compose が見つかりません。先に ./install.sh でインストール・デプロイしてください"
  [ -f .env ] || die ".env が見つかりません。先に ./install.sh を実行して初回デプロイを完了してください"

  # 任意：指定バージョンの tag にアップグレード（未入力なら .env の ARTEX_TAG、未設定なら latest を使用）
  local tag; tag="$(ask '対象イメージの tag（Enter で .env / latest を使用）' '')"
  if [ -n "$tag" ]; then
    if grep -q '^ARTEX_TAG=' .env; then
      sed -i.bak "s|^ARTEX_TAG=.*|ARTEX_TAG=${tag}|" .env && rm -f .env.bak
    else
      printf '\nARTEX_TAG=%s\n' "$tag" >> .env
    fi
    ok "ARTEX_TAG を ${tag} に設定しました"
  fi

  # artex のみを操作：postgres は固定の 16-alpine で、一緒にアップグレードする必要はない（取得しても帯域の無駄で、
  # メジャーバージョン変更には互換性リスクもある）。artex は depends_on postgres を宣言しているため、サービス名を指定して
  # up すると pg が未起動なら自動で起動し、起動済みならそのまま維持して再構築しない。
  info "最新コードからイメージを再ビルドします（artex のみ）…"
  docker compose build artex
  info "起動します（artex の再起動時に schema を自動マイグレーション）…"
  docker compose up -d artex
  ok "更新が完了しました → http://localhost:8787"
  info "ログ確認：docker compose logs -f artex"
  info "古いイメージの削除（任意）：docker image prune -f"
}

# ── ② ローカルビルドで更新 ──────────────────────────────
update_local(){
  command -v go >/dev/null 2>&1 || die "Go が見つかりません（>=1.26）：https://go.dev/dl/"
  [ -f config.json ] || warn "config.json が見つかりません。初回デプロイの場合は ./install.sh を使用してください"
  ok "Go: $(go version)"

  if command -v npm >/dev/null 2>&1; then
    info "フロントエンドの静的成果物を再ビルドします…"
    ( cd web && npm ci && npm run build:static )
    rm -rf server/webui/dist && cp -r web/out server/webui/dist
    info "フロントエンド埋め込みの単一バイナリを再ビルドします…"
    CGO_ENABLED=0 go build -tags embedui -trimpath -o artex ./cmd/artex
  else
    warn "npm が見つかりません：**フロントエンドを埋め込まない**バックエンドをビルドします（フロントエンドは別途 npm run dev）"
    CGO_ENABLED=0 go build -o artex ./cmd/artex
  fi
  ok "ビルド完了 → ./artex"
  warn "反映するには実行中の artex プロセスを再起動してください（再起動時に schema を自動マイグレーション）"
}

echo "=============================="
echo "  ARTEX の更新"
echo "  1) Docker 更新（ローカルの Dockerfile から再ビルド）"
echo "  2) ローカルで更新（go で再ビルド）"
echo "=============================="
case "$(ask '番号を選択' 1)" in
  1) sync_repo; update_docker ;;
  2) sync_repo; update_local ;;
  *) die "無効な選択です" ;;
esac
