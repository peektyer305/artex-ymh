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
#
set -u

cd "$(dirname "$0")" || exit 1

BIN=./artex
[ -x "$BIN" ] || { echo "[artex] 実行ファイルが見つかりません $BIN" >&2; exit 1; }

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
		*)
			echo "[artex] 異常終了（code=$code）、${delay}s 後に再起動" >&2
			sleep "$delay"
			delay=$((delay * 2))
			[ "$delay" -gt "$MAX_DELAY" ] && delay=$MAX_DELAY
			;;
	esac
done
