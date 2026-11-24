#!/usr/bin/env bash
# =============================================================================
# ARTEX-ymh 武器庫（外部ツール）一括セットアップスクリプト
#
# Docker Compose で動かしている Linux 環境向け。どの Linux VM でも使えます。
# ・バイナリ系ツール（naabu/httpx/katana/chisel/fscan/gogo/ligolo/suo5/peass/...）を
#   GitHub の最新リリースから自動取得して data/tools/ に配置
# ・Python 系ツール（impacket/pypykatz/NetExec）をコンテナ内の venv に導入
# ・docker-compose.override.yml に PATH と manifest マウントを設定
#
# 使い方:
#   ./install-arsenal.sh [リポジトリのパス]
#   例: ./install-arsenal.sh ~/Desktop/artex-ymh
#       （省略時はカレントディレクトリ）
#
# 環境変数で上書きできます:
#   PIP_INDEX ... pip のインデックス URL
#                 既定: https://pypi.tuna.tsinghua.edu.cn/simple（中国向けミラー）
#                 中国以外なら: export PIP_INDEX=https://pypi.org/simple
# =============================================================================
set -uo pipefail

REPO_DIR="${1:-$(pwd)}"
cd "$REPO_DIR" 2>/dev/null || { echo "[エラー] リポジトリが見つかりません: $REPO_DIR"; exit 1; }

TOOLS="$PWD/data/tools"
PIP_INDEX="${PIP_INDEX:-https://pypi.tuna.tsinghua.edu.cn/simple}"
COMPOSE="${COMPOSE:-docker compose}"
TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT

ok=0; ng=0
say(){ printf '\n== %s ==\n' "$1"; }
stp(){ printf '  - %-26s ' "$1"; }

# --- 前提チェック ---
say "前提チェック"
for c in curl jq unzip tar; do
  command -v "$c" >/dev/null 2>&1 || { echo "  不足しています: $c（sudo apt install -y curl jq unzip tar）"; exit 1; }
done
command -v docker >/dev/null 2>&1 || { echo "  docker が見つかりません。先に Docker を導入してください。"; exit 1; }
echo "  前提 OK（リポジトリ: $PWD）"

# --- 1. compose 設定（PATH と manifest マウント） ---
say "compose 設定（docker-compose.override.yml）"
if [ -f docker-compose.override.yml ] && ! grep -q 'tools-manifest.json' docker-compose.override.yml; then
  cp docker-compose.override.yml docker-compose.override.yml.bak
  echo "  既存の override を docker-compose.override.yml.bak に退避しました"
fi
cat > docker-compose.override.yml <<'OVR'
services:
  artex:
    environment:
      PATH: /app/data/tools:/app/data/tools/venv/bin:/usr/local/bin:/usr/local/sbin:/usr/sbin:/usr/bin:/sbin:/bin
    volumes:
      - ./packaging/tools-manifest.json:/app/packaging/tools-manifest.json:ro
OVR
echo "  override を作成しました"

$COMPOSE up -d >/dev/null 2>&1 || { echo "  compose 起動に失敗しました"; exit 1; }
echo "  コンテナ起動済み"

# --- 2. バイナリ系ツールの取得 ---
say "バイナリ系ツールのダウンロード"
mkdir -p "$TOOLS" "$TOOLS/peass"

gh_asset() { # repo regex
  curl -fsSL "https://api.github.com/repos/$1/releases/latest" \
    | jq -r --arg p "$2" '[.assets[]|select(.name|test($p))][0].browser_download_url // empty'
}

install_bin() { # repo regex [srcname] destname
  local repo="$1" re="$2" srcname="${3:-}" dest="$4"
  local url f dir src
  stp "$dest"
  url="$(gh_asset "$repo" "$re")"
  [ -n "$url" ] || { echo "アセットなし（手動で配置）"; ng=$((ng+1)); return; }
  f="$TMP/$(basename "$url")"
  curl -fsSL -o "$f" "$url" || { echo "DL失敗"; ng=$((ng+1)); return; }
  dir="$TMP/u_${dest}"; mkdir -p "$dir"
  case "$f" in
    *.tar.gz) tar -xzf "$f" -C "$dir" ;;
    *.zip)    unzip -qo "$f" -d "$dir" ;;
    *.gz)     gunzip -c "$f" > "$dir/${srcname:-$(basename "$f" .gz)}" ;;
    *)        cp "$f" "$dir/${srcname:-$(basename "$f")}" ;;
  esac
  src="$(find "$dir" -type f -name "${srcname:-$(basename "$f")}" 2>/dev/null | head -n1)"
  [ -n "$src" ] || { echo "展開後に見つからない"; ng=$((ng+1)); return; }
  install -m755 "$src" "$TOOLS/$dest" 2>/dev/null \
    || sudo install -m755 "$src" "$TOOLS/$dest" \
    || { echo "配置失敗"; ng=$((ng+1)); return; }
  echo "OK"; ok=$((ok+1))
}

install_bin projectdiscovery/naabu   'linux_amd64\.zip$'          naabu  naabu
install_bin projectdiscovery/httpx   'linux_amd64\.zip$'          httpx  httpx
install_bin projectdiscovery/katana  'linux_amd64\.zip$'          katana katana
install_bin jpillora/chisel          'linux_amd64\.gz$'           chisel chisel
install_bin shadow1ng/fscan          'fscan_[0-9.]+_linux_x64$'   ''     fscan
install_bin chainreactors/gogo       'gogo_linux_amd64$'          ''     gogo
install_bin nicocha30/ligolo-ng      'agent_.*linux_amd64\.tar\.gz$' agent ligolo-agent
install_bin nicocha30/ligolo-ng      'proxy_.*linux_amd64\.tar\.gz$' proxy ligolo-proxy
install_bin zema1/suo5               'suo5-linux-amd64$'          ''     suo5

# penelope.py
stp "penelope.py"
if curl -fsSL -o "$TOOLS/penelope.py" https://raw.githubusercontent.com/brightio/penelope/main/penelope.py; then
  chmod +x "$TOOLS/penelope.py"; echo "OK"; ok=$((ok+1))
else echo "失敗"; ng=$((ng+1)); fi

# peass（linpeas / winPEAS）
for f in linpeas.sh winPEASx64.exe; do
  stp "peass/$f"
  if curl -fsSL -o "$TOOLS/peass/$f" "https://github.com/carlospolop/PEASS-ng/releases/latest/download/$f"; then
    chmod +x "$TOOLS/peass/$f" 2>/dev/null; echo "OK"; ok=$((ok+1))
  else echo "失敗"; ng=$((ng+1)); fi
done

# mimikatz.exe（Windows へ配布する用。配置のみ）
stp "mimikatz.exe"
if curl -fsSL -o "$TMP/mimi.zip" https://github.com/gentilkiwi/mimikatz/releases/latest/download/mimikatz_trunk.zip; then
  unzip -qo "$TMP/mimi.zip" -d "$TMP/mimi" >/dev/null
  src="$(find "$TMP/mimi" -type f -iname mimikatz.exe | grep -i x64 | head -n1)"
  if [ -n "$src" ]; then install -m644 "$src" "$TOOLS/mimikatz.exe" && { echo "OK"; ok=$((ok+1)); }; else echo "x64 なし"; ng=$((ng+1)); fi
else echo "DL失敗"; ng=$((ng+1)); fi

# laZagne.exe（Windows へ配布する用。配置のみ）
stp "laZagne.exe"
if curl -fsSL -o "$TOOLS/laZagne.exe" https://github.com/AlessandroZ/LaZagne/releases/latest/download/laZagne.exe; then
  echo "OK"; ok=$((ok+1))
else rm -f "$TOOLS/laZagne.exe"; echo "スキップ（手動配置）"; fi

# suo5-payloads（リリースには無いため、リポジトリの assets/ から取得）
stp "suo5-payloads"
suo5_tag="$(curl -fsSL https://api.github.com/repos/zema1/suo5/releases/latest | jq -r '.tag_name')"
if [ -n "$suo5_tag" ] && curl -fsSL -o "$TMP/suo5-src.tar.gz" "https://github.com/zema1/suo5/archive/refs/tags/${suo5_tag}.tar.gz"; then
  tar -xzf "$TMP/suo5-src.tar.gz" -C "$TMP"
  asset_dir="$(find "$TMP" -maxdepth 2 -type d -name assets | head -n1)"
  if [ -n "$asset_dir" ]; then
    mkdir -p "$TOOLS/suo5-payloads"
    find "$asset_dir" -type f \( -name '*.jsp' -o -name '*.php' -o -name '*.aspx' -o -name '*.js' -o -name '*.mjs' \) -exec cp -n {} "$TOOLS/suo5-payloads/" \;
    echo "OK"; ok=$((ok+1))
  else echo "assets なし"; ng=$((ng+1)); fi
else echo "取得失敗"; ng=$((ng+1)); fi

# --- 3. 実行権限をまとめて付与 ---
say "実行権限の付与"
chmod 755 "$TOOLS"/* 2>/dev/null || sudo chmod 755 "$TOOLS"/* 2>/dev/null || true
chmod 755 "$TOOLS"/suo5-payloads/* 2>/dev/null || true
chmod 755 "$TOOLS"/peass/* 2>/dev/null || true
echo "  完了"

# --- 4. Python 系ツール（venv） ---
say "Python 系ツール（impacket / pypykatz / NetExec）"

$COMPOSE exec -T artex sh -c 'apt-get update -qq && apt-get install -y -qq --no-install-recommends build-essential >/dev/null 2>&1' \
  && echo "  build-essential 導入済み（netifaces ビルド用）"

$COMPOSE exec -T artex sh -c 'test -x /app/data/tools/venv/bin/python || python3 -m venv /app/data/tools/venv' \
  && echo "  venv 準備済み"

$COMPOSE exec -T artex sh -c "/app/data/tools/venv/bin/pip install -q -i ${PIP_INDEX} impacket pypykatz" \
  && echo "  impacket / pypykatz 導入済み" \
  || echo "  impacket / pypykatz 導入に失敗（ネットワーク/ミラーを確認: PIP_INDEX=${PIP_INDEX}）"

$COMPOSE exec -T artex sh -c "/app/data/tools/venv/bin/pip install -q -i ${PIP_INDEX} git+https://github.com/Pennyw0rth/NetExec" \
  && echo "  NetExec（nxc）導入済み" \
  || echo "  NetExec 導入に失敗"

# impacket は *.py 名で入るため、impacket-* 名でも呼べるように symlink を作成
$COMPOSE exec -T artex sh -c '
  for s in /app/data/tools/venv/bin/*.py; do
    b="$(basename "$s" .py)"
    ln -sf "$s" "/app/data/tools/venv/bin/impacket-$b"
  done' 2>/dev/null
echo "  impacket-* エイリアス作成済み"

# manifest のディレクトリ項目（impacket など）を用意
$COMPOSE exec -T artex sh -c 'mkdir -p /app/data/tools/impacket' 2>/dev/null

# --- 5. 最終確認 ---
say "最終確認（doctor）"
$COMPOSE exec -T artex /app/artex doctor || true

echo
echo "======================================================"
echo "完了: バイナリ系 成功 $ok / 失敗 $ng"
echo "失敗したものは手動で data/tools/ に配置してください。"
echo "目指す形: 武器庫ツール 存在 14/14、FAIL 0"
