#!/bin/sh
# ARTEX 常駐起動スクリプト（Linux / macOS / Docker ENTRYPOINT）
#
# 使い方：
#   ./start.sh                       フォアグラウンド実行（Ctrl-C で停止）
#   nohup ./start.sh >artex.log 2>&1 &   バックグラウンドで常駐
#   ./start.sh -addr :9000           追加の引数はそのまま artex に渡す
#
# やることは 1 つだけ：artex を起動し、プロセス終了後は終了コードで再起動を判断する。
#
#   0      ユーザーが正常停止   → ループを抜ける
#   75     プログラムが再起動を要求 → すぐ再実行（画面で「ワンクリック更新」または「ロールバック」を押下）
#   その他 クラッシュ           → バックオフ後に再実行（1→2→4…最大 60 秒）
#
# ダウンロード、SHA256 検証、差し替えは意図的にここでは行わない：その処理は sh と bat で
# 二重に書く必要があり、しかも最も失敗できない部分。起動しないバイナリに差し替えると、
# 本スクリプトは忠実にそれを起動し続け、ユーザーは手作業で復旧するしかなくなる。
# そのため検証／差し替えはすべて Go 側（selfupdate パッケージ）に残し、artex 自身が
# 起動時に実行する。スクリプトはあえて単純に保つ。
set -u

cd "$(dirname "$0")" || exit 1

BIN=./artex
[ -x "$BIN" ] || { echo "[artex] 実行ファイルが見つかりません $BIN" >&2; exit 1; }

RESTART_CODE=75
MAX_DELAY=60

child=0
stopping=0

# 停止シグナルを artex 本体へ転送する。
#
# Docker ではこれが必須：docker stop は SIGTERM を PID 1（つまり本スクリプト）にしか
# 送らず、子プロセスには送らない。転送しないと artex はシグナルを受け取れず正常終了できず、
# 10 秒後に SIGKILL で強制終了され、実行中のタスクが途中で切れる。
forward() {
	stopping=1
	if [ "$child" -ne 0 ]; then
		kill -TERM "$child" 2>/dev/null || true
	fi
}
trap forward INT TERM

delay=1
while :; do
	"$BIN" "$@" &
	child=$!

	# シグナルは wait を中断し、>128 を返させる。このとき子プロセスはまだ正常終了処理中で、
	# もう一度 wait しないと本当の終了コードが取れない。
	wait "$child"
	code=$?
	if [ "$code" -gt 128 ]; then
		wait "$child"
		code=$?
	fi
	child=0

	if [ "$stopping" -eq 1 ]; then
		echo "[artex] 停止しました"
		exit 0
	fi

	case "$code" in
		0)
			echo "[artex] 正常に終了しました"
			exit 0
			;;
		"$RESTART_CODE")
			# 更新／ロールバックの準備完了：再実行後、artex は起動時に差し替えを完了する（selfupdate.Bootstrap 参照）。
			echo "[artex] 再起動を要求（新しいバージョンを適用）…"
			delay=1
			;;
		*)
			echo "[artex] 異常終了（code=$code）、${delay}s 後に再起動" >&2
			sleep "$delay"
			delay=$((delay * 2))
			[ "$delay" -gt "$MAX_DELAY" ] && delay=$MAX_DELAY
			;;
	esac
done
