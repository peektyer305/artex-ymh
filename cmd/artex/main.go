// Command artex runs the ARTEX backend: the PostgreSQL stores (asset graph +
// per-task exploration graphs), the event-driven exploration engine, and the
// JSON HTTP API consumed by the shadcn/ui frontend. SQLite survives only as
// the traffic recorder's local index (see traffic/).
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/config"
	"github.com/Autumn-27/artex/server"
)

// version is the build version, injected at release time via
// -ldflags "-X main.version=<tag>". Defaults to "dev" for local builds.
var version = "dev"

const banner = `
    _    ____ _____ _______  __
   / \  |  _ \_   _| ____\ \/ /
  / _ \ | |_) || | |  _|  \  /
 / ___ \|  _ < | | | |___ /  \
/_/   \_\_| \_\|_| |_____/_/\_\
`

// printBanner writes the startup banner + version/runtime info to stdout.
func printBanner(addr string) {
	fmt.Print(banner)
	fmt.Println("  AI 自律ペネトレーションテストシステム")
	fmt.Printf("  バージョン %s  ·  %s/%s  ·  %s  ·  リッスン %s\n\n",
		version, runtime.GOOS, runtime.GOARCH, runtime.Version(), addr)
}

func main() {
	os.Exit(run())
}

func run() int {
	// `artex doctor` デプロイ事前チェック（フェーズ 6）。読み取り専用チェックで、サービス flag の解析や各サブシステムの起動より前に分岐する。
	if len(os.Args) > 1 && os.Args[1] == "doctor" {
		return runDoctor(os.Args[2:])
	}

	var (
		addr    = flag.String("addr", "127.0.0.1:8787", "HTTP listen address (loopback by default; pass 0.0.0.0:8787 explicitly to expose)")
		dataDir = flag.String("data", filepath.Join(config.BaseDir(), "data"), "data directory for the traffic recorder and other local state (default: data/ next to the executable)")
		proxy   = flag.String("proxy", "127.0.0.1:8788", "traffic recording proxy address (loopback by default; empty to disable)")
	)
	flag.Parse()
	if err := config.Initialize(); err != nil {
		log.Printf("[config] %v", err)
		return 1
	}

	// hand the build version to the server package so GET /api/health can report it
	// to the frontend top bar.
	server.BuildVersion = version

	printBanner(*addr)

	// capture backend logs into the in-memory sink (still to stderr) so the /logs
	// page can show a live log stream. Do this first, to catch startup logs too.
	server.StartLogCapture()

	// surface which config file the binary reads (absolute, so `go run`'s relative
	// "config.json" — resolved against the CWD — is unambiguous).
	cfgPath := config.Path()
	if abs, e := filepath.Abs(cfgPath); e == nil {
		cfgPath = abs
	}
	if _, e := os.Stat(cfgPath); e == nil {
		log.Printf("[config] 設定ファイル: %s", cfgPath)
	} else {
		log.Printf("[config] 設定ファイル: %s（存在しません — 環境変数 ARTEX_PG_DSN のみを試行します）", cfgPath)
	}

	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, shutdown := shutdownContext(sigCtx)
	defer shutdown(agent.AbortShutdown)

	mgr, err := server.NewManager(*dataDir, *proxy)
	if err != nil {
		log.Fatalf("open stores: %v", err)
	}
	defer mgr.Close()

	// Surviving this long means a freshly swapped-in build actually works, so drop
	// the upgrade marker and stop counting attempts. Until it fires, every boot
	// increments the count and a build that keeps dying gets rolled back.

	skillDir := config.SkillDir()
	if abs, err := filepath.Abs(skillDir); err == nil {
		skillDir = abs
	}
	log.Printf("[config] skill ディレクトリ: %s", skillDir)
	// ARTEX_CALLBACK_ADDR：リバースシェル/トンネル（フェーズ 3/5）のプラットフォームへのコールバック用。フェーズ 1a では起動時の検証メッセージのみ。
	if cb := config.CallbackAddr(); cb != "" {
		log.Printf("[config] コールバックアドレス（ARTEX_CALLBACK_ADDR）: %s", cb)
	} else {
		log.Printf("[config] ARTEX_CALLBACK_ADDR が未設定です：リバースシェル/トンネル（フェーズ 3/5）にはプラットフォームへのコールバックアドレスが必要です。フットホールド（webshell）機能には影響しません")
	}
	srv := server.New(ctx, mgr, skillDir, *dataDir, config.BaseDir())

	// 対偵察偽装ゲート（F14）:非 loopback にバインドした場合は既定で有効、ARTEX_GATE=off で無効化できる。
	gate, err := server.NewGate(*addr, *dataDir, config.BaseDir())
	if err != nil {
		log.Fatalf("gate: %v", err)
	}
	srv.SetGate(gate)

	// 管理対象ステージング（F13）のダウンロード URL はメインのリッスンアドレスを再利用する（ツールの戻り値で worker に使用可能な絶対 URL を渡す）。
	srv.SetStageBaseURL(*addr)

	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("ARTEX %s backend listening on %s (data=%s, workers=%d)", version, *addr, *dataDir, mgr.Workers())
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("serve: %v", err)
		}
	}()

	<-ctx.Done()

	log.Println("shutting down...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
	return 0
}

// shutdownContext deliberately does not derive from signalCtx. If it did, the
// parent's plain context.Canceled could win the race before AbortShutdown was
// attached to the child, losing the diagnostic cause in every running Agent.
func shutdownContext(signalCtx context.Context) (context.Context, context.CancelCauseFunc) {
	ctx, shutdown := context.WithCancelCause(context.Background())
	go func() {
		select {
		case <-signalCtx.Done():
			shutdown(agent.AbortShutdown)
		case <-ctx.Done():
		}
	}()
	return ctx, shutdown
}
