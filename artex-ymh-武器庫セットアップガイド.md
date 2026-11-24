# ARTEX-ymh 武器庫（外部ツール）セットアップガイド

ARTEX-ymh はペネトレーションに使う外部ツール群（＝武器庫）を、イメージに同梱せず `data/tools/` に置く設計です。このガイドでは、どの Linux VM でも同じ手順で一括セットアップできる方法と、途中でつまずいたときの対処をまとめています。

> 対象: Docker Compose で動かしている Linux 環境（Kali / Ubuntu / Debian など）。x86_64（amd64）前提です。

---

## 1. 武器庫とは

| 分類 | ツール |
|---|---|
| スキャン | naabu / httpx / katana / fscan / gogo |
| トンネル | chisel / suo5 / suo5-payloads |
| 横展開 | ligolo-agent / ligolo-proxy |
| 権限昇格 | peass / penelope.py |
| Windows 認証 | impacket / nxc（NetExec）/ pypykatz / mimikatz.exe / laZagne.exe |

- `nmap` はイメージに同梱済みなので追加不要です。
- `mimikatz.exe` / `laZagne.exe` はターゲットへ送る用で、ホストでは実行しません（配置のみ）。
- 全部揃えなくても動きます（スキャン系だけでもタスクは回ります）。

---

## 2. 一括セットアップ（おすすめ）

以下のスクリプトを 1 つ用意して実行するだけで、バイナリ系・Python 系・compose 設定をまとめて行います。

### 手順 1: スクリプトを作成

Kali 上で以下を貼り付けて `~/install-arsenal.sh` を作成します。

```bash
cat > ~/install-arsenal.sh <<'SCRIPT'
#!/usr/bin/env bash
# ARTEX-ymh 武器庫 一括セットアップ（Docker Compose 環境向け・どの Linux VM でも利用可）
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

say "前提チェック"
for c in curl jq unzip tar; do
command -v "$c" >/dev/null 2>&1 || { echo "  不足: $c（sudo apt install -y curl jq unzip tar）"; exit 1; }
done
command -v docker >/dev/null 2>&1 || { echo "  docker が見つかりません"; exit 1; }
echo "  前提 OK（リポジトリ: $PWD）"

say "compose 設定（docker-compose.override.yml）"
if [ -f docker-compose.override.yml ] && ! grep -q 'tools-manifest.json' docker-compose.override.yml; then
cp docker-compose.override.yml docker-compose.override.yml.bak
echo "  既存 override を .bak に退避"
fi
cat > docker-compose.override.yml <<'OVR'
services:
artex:
environment:
PATH: /app/data/tools:/app/data/tools/venv/bin:/usr/local/bin:/usr/local/sbin:/usr/sbin:/usr/bin:/sbin:/bin
volumes:
- ./packaging/tools-manifest.json:/app/packaging/tools-manifest.json:ro
OVR
$COMPOSE up -d >/dev/null 2>&1 || { echo "  compose 起動失敗"; exit 1; }
echo "  コンテナ起動済み"

say "バイナリ系ツールのダウンロード"
mkdir -p "$TOOLS" "$TOOLS/peass"

gh_asset() { curl -fsSL "https://api.github.com/repos/$1/releases/latest" | jq -r --arg p "$2" '[.assets[]|select(.name|test($p))][0].browser_download_url // empty'; }

install_bin() {
local repo="$1" re="$2" srcname="${3:-}" dest="$4"
local url f dir src
stp "$dest"
url="$(gh_asset "$repo" "$re")"
[ -n "$url" ] || { echo "アセットなし（手動配置）"; ng=$((ng+1)); return; }
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
[ -n "$src" ] || { echo "展開後になし"; ng=$((ng+1)); return; }
install -m755 "$src" "$TOOLS/$dest" 2>/dev/null || sudo install -m755 "$src" "$TOOLS/$dest" || { echo "配置失敗"; ng=$((ng+1)); return; }
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

stp "penelope.py"
if curl -fsSL -o "$TOOLS/penelope.py" https://raw.githubusercontent.com/brightio/penelope/main/penelope.py; then chmod +x "$TOOLS/penelope.py"; echo "OK"; ok=$((ok+1)); else echo "失敗"; ng=$((ng+1)); fi

for f in linpeas.sh winPEASx64.exe; do
stp "peass/$f"
if curl -fsSL -o "$TOOLS/peass/$f" "https://github.com/carlospolop/PEASS-ng/releases/latest/download/$f"; then chmod +x "$TOOLS/peass/$f" 2>/dev/null; echo "OK"; ok=$((ok+1)); else echo "失敗"; ng=$((ng+1)); fi
done

stp "mimikatz.exe"
if curl -fsSL -o "$TMP/mimi.zip" https://github.com/gentilkiwi/mimikatz/releases/latest/download/mimikatz_trunk.zip; then
unzip -qo "$TMP/mimi.zip" -d "$TMP/mimi" >/dev/null
src="$(find "$TMP/mimi" -type f -iname mimikatz.exe | grep -i x64 | head -n1)"
if [ -n "$src" ]; then install -m644 "$src" "$TOOLS/mimikatz.exe" && { echo "OK"; ok=$((ok+1)); }; else echo "x64 なし"; ng=$((ng+1)); fi
else echo "DL失敗"; ng=$((ng+1)); fi

stp "laZagne.exe"
if curl -fsSL -o "$TOOLS/laZagne.exe" https://github.com/AlessandroZ/LaZagne/releases/latest/download/laZagne.exe; then echo "OK"; ok=$((ok+1)); else rm -f "$TOOLS/laZagne.exe"; echo "スキップ（手動配置）"; fi

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

say "実行権限の付与"
chmod 755 "$TOOLS"/* 2>/dev/null || sudo chmod 755 "$TOOLS"/* 2>/dev/null || true
chmod 755 "$TOOLS"/suo5-payloads/* 2>/dev/null || true
chmod 755 "$TOOLS"/peass/* 2>/dev/null || true
echo "  完了"

say "Python 系ツール（impacket / pypykatz / NetExec）"
$COMPOSE exec -T artex sh -c 'apt-get update -qq && apt-get install -y -qq --no-install-recommends build-essential >/dev/null 2>&1' && echo "  build-essential 導入済み"
$COMPOSE exec -T artex sh -c 'test -x /app/data/tools/venv/bin/python || python3 -m venv /app/data/tools/venv' && echo "  venv 準備済み"
$COMPOSE exec -T artex sh -c "/app/data/tools/venv/bin/pip install -q -i ${PIP_INDEX} impacket pypykatz" && echo "  impacket / pypykatz 導入済み" || echo "  impacket / pypykatz 失敗（PIP_INDEX=${PIP_INDEX} を確認）"
$COMPOSE exec -T artex sh -c "/app/data/tools/venv/bin/pip install -q -i ${PIP_INDEX} git+https://github.com/Pennyw0rth/NetExec" && echo "  NetExec（nxc）導入済み" || echo "  NetExec 失敗"
$COMPOSE exec -T artex sh -c 'for s in /app/data/tools/venv/bin/*.py; do b="$(basename "$s" .py)"; ln -sf "$s" "/app/data/tools/venv/bin/impacket-$b"; done' 2>/dev/null
$COMPOSE exec -T artex sh -c 'mkdir -p /app/data/tools/impacket' 2>/dev/null
echo "  impacket-* エイリアス作成済み"

say "最終確認（doctor）"
$COMPOSE exec -T artex /app/artex doctor || true

echo
echo "完了: バイナリ系 成功 $ok / 失敗 $ng"
echo "目指す形: 武器庫ツール 存在 14/14、FAIL 0"
SCRIPT
chmod +x ~/install-arsenal.sh
```

### 手順 2: 実行

```bash
~/install-arsenal.sh ~/Desktop/artex-ymh
```

- 中国以外のネットワークでは `export PIP_INDEX=https://pypi.org/simple` を先に実行してください。
- 別の VM では引数をその VM のリポジトリパスに変えるだけです（例: `~/install-arsenal.sh /opt/artex`）。

### 手順 3: 確認

```bash
docker compose exec artex /app/artex doctor
# 武器庫ツール: 存在 14/14   ← これが目標
```

---

## 3. スクリプトがやること（全体像）

1. compose 設定— `docker-compose.override.yml` に PATH（ツールを名前で呼ぶため）と manifest マウント（doctor が集計するため）を書く
2. バイナリ系— GitHub の最新リリースを正規表現で自動選択し、`data/tools/` に配置
3. 実行権限— 全部に `chmod 755`
4. Python 系— コンテナ内に venv を作り、impacket / pypykatz / NetExec を導入
5. doctor — 最終確認

> `data/` は `docker compose` が `/app/data` にマウントしているので、ホスト側の `data/tools/` に置けばそのままコンテナから見えます。

---

## 4. よくあるつまずきと対策

セットアップ中によく遭遇する症状と、その対処です。

| # | 症状 | 原因 | 対策 |
|---|---|---|---|
| 1 | `docker compose up` で `no configuration file provided` | リポジトリ外のディレクトリで実行している | `cd` でリポジトリへ移動してから実行 |
| 2 | doctor で `ツール一覧: manifest が欠落`／`武器庫ツール SKIP` | イメージに `packaging/tools-manifest.json` が入っていない | `docker-compose.override.yml` で `./packaging/tools-manifest.json:/app/packaging/tools-manifest.json:ro` をマウント |
| 3 | `command -v naabu` しか出ない | 他ツールに実行権限（+x）が無い | `chmod 755 data/tools/*` |
| 4 | fscan のアセットが見つからない | fscan は `linux_x64` 表記（`amd64` ではない）で、生バイナリ | `fscan_<ver>_linux_x64` を選ぶ |
| 5 | gogo / suo5 が見つからない | どちらも生バイナリ（`gogo_linux_amd64` / `suo5-linux-amd64`） | 無拡張子のそのまま配置 |
| 6 | suo5-payloads が見つからない | リリースに同梱されず、リポジトリの `assets/` にある | tarball で取得して `assets/` の中身をコピー |
| 7 | `pip install netexec` で `No matching distribution` | netexec は PyPI に無い| `pip install git+https://github.com/Pennyw0rth/NetExec` |
| 8 | pypykatz で `gcc: No such file or directory` | `netifaces`（C 拡張）のビルドに gcc が必要 | コンテナに `build-essential` を導入（`Dockerfile.source` にも追加すると恒久化） |
| 9 | `command -v impacket-psexec` が出ない | pip 版 impacket は `secretsdump.py` などの `.py` 名で入る（`impacket-*` は apt 版の慣習） | `secretsdump.py` 等を呼ぶ。必要なら `impacket-*` の symlink を作る |
| 10 | pip が `from versions: none` | PyPI（pypi.org）に到達できない | ミラー `-i https://pypi.tuna.tsinghua.edu.cn/simple` を指定 |
| 11 | `chmod: cannot operate on dangling symlink` | venv を作る前に symlink だけ作った | venv を作ってから symlink。このエラー自体は無害 |
| 12 | chisel の `sha256 不一致` | 使った chisel が manifest 固定版とバージョン違い | 警告のみで動作する。気になれば manifest と同じ版に揃える |

---

## 5. 手動で入れる場合の早見表

`data/tools/` 以下に、manifest の `path` と同じ名前で置きます。

| ツール | 配置先 | 備考 |
|---|---|---|
| naabu / httpx / katana / chisel | `data/tools/<name>` | 実行ファイル。`chmod +x` |
| fscan | `data/tools/fscan` | `fscan_<ver>_linux_x64`（生バイナリ） |
| gogo | `data/tools/gogo` | `gogo_linux_amd64`（生バイナリ） |
| suo5 | `data/tools/suo5` | `suo5-linux-amd64`（生バイナリ） |
| suo5-payloads | `data/tools/suo5-payloads/` | ディレクトリ（`.jsp` / `.php` / `.aspx` など） |
| ligolo-agent / ligolo-proxy | `data/tools/…` | tar.gz 内の `agent` / `proxy` を改名 |
| peass | `data/tools/peass/` | ディレクトリ（linpeas.sh / winPEAS） |
| impacket | `data/tools/impacket/` | ディレクトリ（実体は venv の `*.py`） |
| nxc / pypykatz | `data/tools/<name>` | venv の bin への symlink |
| penelope.py | `data/tools/penelope.py` | スクリプト |
| mimikatz.exe / laZagne.exe | `data/tools/…` | Windows 用（配置のみ） |

---

## 6. 確認方法（`artex doctor`）

```bash
docker compose exec artex /app/artex doctor
```

- `[OK]` = 問題なし、`[WARN]` = 動くが注意、`[FAIL]` = 起動を妨げる可能性
- 最後の `集計：OK n · WARN n · FAIL n · SKIP n` を見て、`FAIL 0` なら起動して OK
- 武器庫は `武器庫ツール: 存在 X/Y` で確認（目標は `14/14`）
