@echo off
rem コンソールを UTF-8 に切り替える。さもないと本ファイル内の日本語が GBK 端末で文字化けする。
chcp 65001 >nul 2>&1
rem ARTEX 常駐起動スクリプト（Windows）
rem
rem 使い方：
rem   start.bat                  フォアグラウンド実行（Ctrl-C で停止）
rem   start.bat -addr :9000      追加の引数はそのまま artex に渡す
rem
rem やることは 1 つだけ：artex.exe を起動し、プロセス終了後は終了コードで再起動を判断する。
rem
rem

setlocal enabledelayedexpansion
cd /d "%~dp0"

set "BIN=artex.exe"
if not exist "%BIN%" (
	echo [artex] 実行ファイルが見つかりません %BIN% 1>&2
	exit /b 1
)

set "MAX_DELAY=60"
set /a delay=1

:loop
"%BIN%" %*
set "code=!ERRORLEVEL!"

if "!code!"=="0" (
	echo [artex] 正常に終了しました
	exit /b 0
)


echo [artex] 異常終了 ^(code=!code!^)、!delay!s 後に再起動 1>&2
rem timeout はリダイレクトされたコンソールでは失敗するため、ping で代用する（N 秒待つには N+1 回必要）。
set /a pings=!delay!+1
ping -n !pings! 127.0.0.1 >nul 2>&1
set /a delay=!delay!*2
if !delay! gtr %MAX_DELAY% set /a delay=%MAX_DELAY%
goto loop
