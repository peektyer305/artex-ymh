package server

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/Autumn-27/artex/config"
	pgdb "github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/enrich"
	"github.com/Autumn-27/artex/guard"
	"github.com/Autumn-27/artex/honeydetect"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/artex/traffic"
	"github.com/Autumn-27/artex/tunnel"
)

// NewManager connects to PostgreSQL and, if proxyAddr is non-empty, starts the
// traffic-recording proxy. PostgreSQL is required (it is the single data source).
func NewManager(dir, proxyAddr string) (*Manager, error) {
	// Resolve the data dir to an ABSOLUTE path up front. Every data path derives
	// from it — notably the MITM CA cert, whose path is injected into worker shells
	// (SSL_CERT_FILE/CURL_CA_BUNDLE) and read by WebFetch. A relative path (the
	// default is "./data" under `go run`) only resolves when the current working
	// directory happens to match, so curl/WebFetch in a different CWD fail to load
	// the CA → TLS to the proxy breaks (curl 000 / EOF). Absolute makes it CWD-proof.
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	// バッチ 6 L1 ハニーポット静的シグネチャライブラリ：dataDir 配下の署名ファイルの絶対パスを組み立てる(存在すれば優先し、
	// mtime でホットリロードするので、再リリースせずに署名ライブラリを運用できる;無ければエンジンは埋め込み署名ライブラリに落ちる)。
	honeydetect.SetSignaturesPath(filepath.Join(dir, "signatures", "honeypot.json"))
	dsn, source, err := pgdb.DSN()
	if err != nil {
		return nil, err
	}
	log.Printf("[pg] データベース設定のソース: %s", source)
	pg, err := pgdb.Open(dsn)
	if err != nil {
		return nil, err
	}
	if err := pg.RecoverFindingRetests(); err != nil {
		pg.Close()
		return nil, fmt.Errorf("recover finding retests: %w", err)
	}
	if err := pg.RecoverFindingChecks(); err != nil {
		pg.Close()
		return nil, fmt.Errorf("recover finding checks: %w", err)
	}
	if err := pg.EnsureLLMRecordsTable(); err != nil {
		log.Printf("[llmrec] create table: %v", err)
	}
	if err := pg.EnsureLLMUsageTable(); err != nil {
		log.Printf("[llmusage] create table: %v", err)
	}
	m := &Manager{dir: dir, pg: pg, assets: pg.Assets(), tasks: map[string]*Task{}, interceptor: intercept.New(pg), egress: guard.NewEgressGuard()}
	m.startLLMRecordsRetention()
	m.startPortAudit() // F13 台帳外の待受ポート監査(Linux のみ;観測のみで対処しない)
	if proxyAddr != "" {
		tr, err := traffic.Open(filepath.Join(dir, "traffic"), proxyAddr)
		if err != nil {
			log.Printf("[traffic] disabled: %v", err)
		} else {
			err = tr.RecoverHostDeleteStages(func(_ int64, taskID int64) (bool, error) {
				if taskID <= 0 {
					return false, errors.New("アーカイブトラフィックの一時退避ログにタスク ID がありません")
				}
				task, taskErr := pg.GetTask(taskID)
				if taskErr != nil {
					return false, taskErr
				}
				// Archived/permanently deleted tasks are hidden from GetTask. A
				// restored task is visible and needs the staged traffic put back.
				return task == nil, nil
			})
			if err != nil {
				_ = tr.Close()
				_ = pg.Close()
				return nil, fmt.Errorf("recover traffic delete staging: %w", err)
			}
		}
		if tr != nil {
			m.traffic = tr
			go func() {
				log.Printf("[traffic] recording proxy on %s (set HTTP_PROXY=%s + trust _ca CA; パスワードは data/traffic/_auth を参照、ログには出さない)", proxyAddr, tr.ProxyAddrRedacted())
				if err := tr.Start(); err != nil {
					log.Printf("[traffic] proxy stopped: %v", err)
				}
			}()
		}
	}
	// Asset auto-completion engine (§5): HTTP probes routed through the recording
	// proxy (via m.ProxyAddr, which honors the traffic-capture toggle).
	m.trafficOn = pg.GetBool(settingTrafficCapture, false)
	// LLM レコードのスイッチ（デフォルト無効）。レコーダーは呼び出しのたびにこのフラグを読む。
	m.llmRecOn = pg.GetBool(settingLLMRecord, false)
	// Load persisted web-search config (default: off, ddgs).
	m.webSearchOn = pg.GetBool(settingWebSearchOn, false)
	if v, ok, _ := pg.GetSetting(settingWebSearchBackend); ok && v != "" {
		m.webSearchBackend = v
	} else {
		m.webSearchBackend = defaultWebSearchBackend
	}
	if v, ok, _ := pg.GetSetting(settingBraveKey); ok {
		m.braveKey = v
	}
	if v, ok, _ := pg.GetSetting(settingTavilyKey); ok {
		m.tavilyKey = v
	}
	if v, ok, _ := pg.GetSetting(settingWebSearchProxy); ok {
		m.webSearchProxy = v
	}
	// Global egress proxy (default: direct). When capture is on, feed it to the
	// MITM as its upstream so recorded traffic exits through it; when capture is
	// off, ProxyAddr hands it to agents directly (bash env / WebFetch).
	if v, ok, _ := pg.GetSetting(settingGlobalProxy); ok {
		m.globalProxy = strings.TrimSpace(v)
	}
	if m.traffic != nil {
		if err := m.traffic.SetUpstreamProxy(m.globalProxy); err != nil {
			log.Printf("[proxy] グローバルプロキシ %q は無効なので無視しました: %v", m.globalProxy, err)
		}
	}
	m.enrich = enrich.New(m.assets, m.ProxyAddr, 4)
	// フェーズ 3b:タスクごとに MITM を遅延生成(worker トラフィック → タスクインスタンス → タスクトンネルの socks → イントラネット、
	// 記録の連鎖は途切れない)。ARTEX_TASK_PROXY=off で無効;ポートプールは ARTEX_TASK_PROXY_PORT_RANGE。
	if config.TaskProxyEnabled() {
		if pmin, pmax, err := tunnel.ParsePortRange(config.TaskProxyPortRange()); err != nil {
			log.Printf("[taskproxy] ポートプールが不正、タスク別 MITM は無効: %v", err)
		} else {
			m.taskProxy = newTaskProxyManager(dir, pgdb.NewTunnelStore(pg), m.GlobalProxy, pmin, pmax)
		}
	}
	// Reconcile the seeded browser MCP with the persisted capture state, so a
	// restart with capture already on keeps Playwright routed through the proxy.
	m.syncBrowserMCPProxy()
	return m, nil
}
