#!/usr/bin/env bash
# =============================================================================
# ARTEX 管理者パスワードリセットスクリプト
#
# ログインユーザー名は ARTEX 固定。パスワードは bcrypt ハッシュとしてデータベースの settings テーブルの
# auth.password_hash キーに保存される。本スクリプトはデータベースに接続した後、pgcrypto で DB 内に bcrypt ハッシュを生成し
# そのキーへ書き戻す——バックエンドのログイン検証（golang.org/x/crypto/bcrypt）と完全に互換。
#
# 2 種類のデプロイ：
#   local （デフォルト）—— ホストから psql で直接データベースに接続。接続情報は次の優先順位で取得：
#                    コマンドライン引数 > --dsn/$ARTEX_PG_DSN > config.json の database.*
#   docker        —— `docker compose exec`（または `docker exec`）で postgres
#                    コンテナ内の psql を実行（compose はデフォルトでホストに 5432 を公開しないため、コンテナ内で実行）。
#
# 使い方の例：
#   ./reset-password.sh                          # ローカル。config.json/環境を自動で読み、新しいパスワードを対話入力
#   ./reset-password.sh -p 'NewPass!'            # ローカル。新しいパスワードを直接指定
#   ./reset-password.sh --dsn postgres://u:p@h:5432/artex
#   ./reset-password.sh -H 127.0.0.1 -P 5433 -U autopentest -W pass -d artex
#   ./reset-password.sh -m docker                # docker デプロイ（.env の POSTGRES_* を読み込む）
#   ./reset-password.sh -m docker -c pgコンテナ名 --exec docker
#
# 安全：新しいパスワードは環境変数 + psql の \getenv で渡す（プロセスの argv には入らない）。また :'var' で
# 自動エスケープ（SQL インジェクション対策）。データベースのパスワードは PGPASSWORD で渡し、同様に argv には入らない。
# =============================================================================
set -euo pipefail

PASS_KEY="auth.password_hash"
BCRYPT_COST=10

MODE=""            # local | docker（空=自動で判定）
DSN=""
HOST="" PORT="" USER="" DBPASS="" DBNAME="" SSLMODE=""
CONFIG=""
CONTAINER=""       # docker モードの postgres サービス/コンテナ名（デフォルト postgres）
EXEC_KIND=""       # compose | docker（docker モードで使う exec の種類。空=自動）
NEWPASS=""
ASSUME_YES=0

die() { echo "エラー：$*" >&2; exit 1; }
info() { echo "· $*" >&2; }

usage() { sed -n '2,40p' "$0" | sed 's/^# \{0,1\}//'; exit 0; }

# ---- 引数の解析 -------------------------------------------------------------
while [[ $# -gt 0 ]]; do
  case "$1" in
    -m|--mode)        MODE="${2:-}"; shift 2 ;;
    --dsn)            DSN="${2:-}"; shift 2 ;;
    -H|--host)        HOST="${2:-}"; shift 2 ;;
    -P|--port)        PORT="${2:-}"; shift 2 ;;
    -U|--user)        USER="${2:-}"; shift 2 ;;
    -W|--db-password) DBPASS="${2:-}"; shift 2 ;;
    -d|--dbname)      DBNAME="${2:-}"; shift 2 ;;
    --sslmode)        SSLMODE="${2:-}"; shift 2 ;;
    --config)         CONFIG="${2:-}"; shift 2 ;;
    -c|--container)   CONTAINER="${2:-}"; shift 2 ;;
    --exec)           EXEC_KIND="${2:-}"; shift 2 ;;
    -p|--new-password) NEWPASS="${2:-}"; shift 2 ;;
    -y|--yes)         ASSUME_YES=1; shift ;;
    -h|--help)        usage ;;
    *) die "不明な引数：$1（-h で使い方を確認）" ;;
  esac
done

# ---- config.json から database.* を読み込む（local モードかつ接続先が明示されていない場合のみ）-----
# python3 での解析を優先（堅牢）。python3 がない場合は grep に退避（config.json は整形された個別フィールド）。
read_config_json() {
  local path="$1"
  [[ -f "$path" ]] || return 1
  if command -v python3 >/dev/null 2>&1; then
    python3 - "$path" <<'PY'
import json, sys
try:
    d = json.load(open(sys.argv[1])).get("database", {})
except Exception:
    sys.exit(1)
# dsn 直接指定、またはフィールド分割の両方に対応
if d.get("dsn"):
    print("DSN\t" + d["dsn"]); sys.exit(0)
for k in ("host","port","user","password","dbname","sslmode"):
    if d.get(k) is not None:
        print(k.upper() + "\t" + str(d[k]))
PY
  else
    # 簡易フォールバック：キーごとに grep（値は文字列または数値）
    local k
    for k in host port user password dbname sslmode; do
      local v
      v=$(grep -oE "\"$k\"[[:space:]]*:[[:space:]]*(\"[^\"]*\"|[0-9]+)" "$path" 2>/dev/null \
            | head -1 | sed -E "s/.*:[[:space:]]*//; s/^\"//; s/\"$//") || true
      [[ -n "$v" ]] && echo -e "${k^^}\t$v"
    done
  fi
}

apply_config_fields() {
  local line key val
  while IFS=$'\t' read -r key val; do
    [[ -z "$key" ]] && continue
    case "$key" in
      DSN)      [[ -z "$DSN" ]] && DSN="$val" ;;
      HOST)     [[ -z "$HOST" ]] && HOST="$val" ;;
      PORT)     [[ -z "$PORT" ]] && PORT="$val" ;;
      USER)     [[ -z "$USER" ]] && USER="$val" ;;
      PASSWORD) [[ -z "$DBPASS" ]] && DBPASS="$val" ;;
      DBNAME)   [[ -z "$DBNAME" ]] && DBNAME="$val" ;;
      SSLMODE)  [[ -z "$SSLMODE" ]] && SSLMODE="$val" ;;
    esac
  done
}

# ---- モードの自動判定 ---------------------------------------------------------
if [[ -z "$MODE" ]]; then
  if [[ -n "$DSN$HOST$USER$DBNAME" || -n "${ARTEX_PG_DSN:-}" || -f "${CONFIG:-config.json}" ]]; then
    MODE="local"
  elif command -v docker >/dev/null 2>&1 && [[ -f docker-compose.yml ]]; then
    MODE="docker"
  else
    MODE="local"
  fi
fi
info "デプロイモード：$MODE"

# ---- 新しいパスワードの取得 -----------------------------------------------------------
if [[ -z "$NEWPASS" ]]; then
  read -r -s -p "新しいパスワードを入力（ユーザー名は ARTEX 固定）：" NEWPASS; echo >&2
  [[ -n "$NEWPASS" ]] || die "パスワードを入力してください"
  read -r -s -p "確認のため再入力：" NEWPASS2; echo >&2
  [[ "$NEWPASS" == "$NEWPASS2" ]] || die "2 回の入力が一致しません"
fi
[[ -n "$NEWPASS" ]] || die "パスワードを入力してください"

# 環境変数でパスワードを psql に渡す（\getenv で読み取り、argv/ps には出ない）
export ARTEX_RESET_NEWPASS="$NEWPASS"

# DB 内で bcrypt を生成して upsert。パスワードは :'newpw' で自動エスケープ。CREATE EXTENSION は冪等で、
# データベースロールに拡張作成権限がない場合はここでエラーになる（案内は後述の実行失敗時の分岐を参照）。
SQL=$(cat <<SQL
\\set ON_ERROR_STOP on
\\getenv newpw ARTEX_RESET_NEWPASS
CREATE EXTENSION IF NOT EXISTS pgcrypto;
INSERT INTO settings(key, value)
VALUES ('$PASS_KEY', crypt(:'newpw', gen_salt('bf', $BCRYPT_COST)))
ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now();
SQL
)

# ---- 実際の実行 -----------------------------------------------------------------
if [[ "$MODE" == "local" ]]; then
  # 接続情報の優先順位：コマンドライン > --dsn/$ARTEX_PG_DSN > config.json
  if [[ -z "$DSN" && -z "$HOST$USER$DBNAME" ]]; then
    [[ -n "${ARTEX_PG_DSN:-}" ]] && DSN="$ARTEX_PG_DSN"
  fi
  if [[ -z "$DSN" && -z "$HOST$USER$DBNAME" ]]; then
    cfg="${CONFIG:-config.json}"
    if [[ -f "$cfg" ]]; then
      info "$cfg からデータベース設定を読み込みます"
      apply_config_fields < <(read_config_json "$cfg")
    fi
  fi

  command -v psql >/dev/null 2>&1 || die "ローカルに psql が見つかりません（postgresql-client をインストールするか、-m docker を使用してください）"

  declare -a PSQL_ARGS=()
  if [[ -n "$DSN" ]]; then
    PSQL_ARGS=("$DSN")
    target="$DSN"
  else
    [[ -n "$USER"   ]] || die "データベースユーザー（-U）または有効な config.json/DSN がありません"
    [[ -n "$DBNAME" ]] || die "データベース名（-d）または有効な config.json/DSN がありません"
    HOST="${HOST:-127.0.0.1}"; PORT="${PORT:-5432}"; SSLMODE="${SSLMODE:-disable}"
    PSQL_ARGS=(-h "$HOST" -p "$PORT" -U "$USER" -d "$DBNAME")
    [[ -n "$SSLMODE" ]] && export PGSSLMODE="$SSLMODE"
    [[ -n "$DBPASS" ]] && export PGPASSWORD="$DBPASS"
    target="$USER@$HOST:$PORT/$DBNAME"
  fi

  info "対象データベース：$target"
  if [[ "$ASSUME_YES" -ne 1 ]]; then
    read -r -p "このデータベースで ARTEX のパスワードをリセットしますか？ [y/N] " ans
    [[ "$ans" == "y" || "$ans" == "Y" ]] || die "キャンセルしました"
  fi

  if ! printf '%s\n' "$SQL" | psql "${PSQL_ARGS[@]}" -v ON_ERROR_STOP=1 -q >/dev/null; then
    die "書き込みに失敗しました。pgcrypto の権限不足/未導入が報告された場合は、拡張作成権限を持つロールを使用するか、先に手動で CREATE EXTENSION pgcrypto を実行してください。"
  fi

else
  # ---- docker ----
  command -v docker >/dev/null 2>&1 || die "docker が見つかりません"
  CONTAINER="${CONTAINER:-postgres}"

  # exec 方式の選択：docker compose exec（サービス名）を優先し、なければ docker exec（コンテナ名）
  if [[ -z "$EXEC_KIND" ]]; then
    if docker compose version >/dev/null 2>&1 && [[ -f docker-compose.yml ]]; then
      EXEC_KIND="compose"
    else
      EXEC_KIND="docker"
    fi
  fi

  # コンテナ内 psql の認証情報：コマンドラインを優先し、次に .env の POSTGRES_*、最後に compose のデフォルト（artex）
  if [[ -f .env ]]; then
    # shellcheck disable=SC1091
    set -a; . ./.env; set +a
  fi
  DUSER="${USER:-${POSTGRES_USER:-artex}}"
  DNAME="${DBNAME:-${POSTGRES_DB:-artex}}"
  [[ -n "$DBPASS" ]] && export PGPASSWORD="$DBPASS"
  [[ -z "${PGPASSWORD:-}" && -n "${POSTGRES_PASSWORD:-}" ]] && export PGPASSWORD="$POSTGRES_PASSWORD"

  info "対象：コンテナ $CONTAINER 内で psql -U $DUSER -d $DNAME（exec=$EXEC_KIND）"
  if [[ "$ASSUME_YES" -ne 1 ]]; then
    read -r -p "このコンテナのデータベースで ARTEX のパスワードをリセットしますか？ [y/N] " ans
    [[ "$ans" == "y" || "$ans" == "Y" ]] || die "キャンセルしました"
  fi

  # -e は名前のみで値を伴わない → 現在の環境から継承され、パスワードは docker コマンドの argv に出ない。
  declare -a EXEC_CMD
  if [[ "$EXEC_KIND" == "compose" ]]; then
    EXEC_CMD=(docker compose exec -T -e ARTEX_RESET_NEWPASS -e PGPASSWORD "$CONTAINER"
              psql -U "$DUSER" -d "$DNAME" -v ON_ERROR_STOP=1 -q)
  else
    EXEC_CMD=(docker exec -i -e ARTEX_RESET_NEWPASS -e PGPASSWORD "$CONTAINER"
              psql -U "$DUSER" -d "$DNAME" -v ON_ERROR_STOP=1 -q)
  fi

  if ! printf '%s\n' "$SQL" | "${EXEC_CMD[@]}" >/dev/null; then
    die "書き込みに失敗しました。コンテナ名（-c）、データベースアカウント（.env の POSTGRES_*）、およびロールの pgcrypto 権限を確認してください。"
  fi
fi

unset ARTEX_RESET_NEWPASS
echo "✓ ARTEX 管理者パスワードをリセットしました。ユーザー名 ARTEX + 新しいパスワードでログインしてください（サービスの再起動は不要）。"
