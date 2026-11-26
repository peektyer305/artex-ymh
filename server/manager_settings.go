package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Autumn-27/artex/agent"
	pgdb "github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/enrich"
)

// ConcurrencyLimit returns whether the simultaneous-running-task cap is enabled and
// its limit (default 5 when enabled but unset). limit is always >=1 when enabled.
func (m *Manager) ConcurrencyLimit() (enabled bool, limit int) {
	on, _, _ := m.pg.GetSetting(settingConcurrencyOn)
	if strings.TrimSpace(on) != "true" {
		return false, 0
	}
	limit = defaultConcurrencyLimit
	if v, ok, _ := m.pg.GetSetting(settingConcurrencyLimit); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 1 {
			limit = n
		}
	}
	return true, limit
}

// SetConcurrency persists the running-task concurrency cap. limit<1 is clamped to 1.
func (m *Manager) SetConcurrency(enabled bool, limit int) error {
	if limit < 1 {
		limit = defaultConcurrencyLimit
	}
	if err := m.pg.SetSetting(settingConcurrencyLimit, strconv.Itoa(limit)); err != nil {
		return err
	}
	return m.pg.SetSetting(settingConcurrencyOn, strconv.FormatBool(enabled))
}

// Workers returns the configured concurrent work-agent count (default 3). Read
// per-task at engine.Run, so a change applies to tasks started afterwards.
func (m *Manager) Workers() int {
	v, ok, err := m.pg.GetSetting(settingWorkers)
	if err != nil || !ok {
		return defaultWorkers
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n <= 0 {
		return defaultWorkers
	}
	return n
}

// SetWorkers persists the concurrent work-agent count. Values <=0 are rejected.
func (m *Manager) SetWorkers(n int) error {
	if n <= 0 {
		return fmt.Errorf("workers は 0 より大きい必要があります")
	}
	return m.pg.SetSetting(settingWorkers, strconv.Itoa(n))
}

// Enrich returns the asset auto-completion engine (may be nil if init failed).
func (m *Manager) Enrich() *enrich.Engine { return m.enrich }

// llmRecordsRetentionDays reads settings.llm_records_retention_days (default 30,
// 0 = keep forever). Unset/unparseable/negative → default.
func (m *Manager) llmRecordsRetentionDays() int {
	v, ok, err := m.pg.GetSetting(settingLLMRecordsRetentionDays)
	if err != nil || !ok {
		return defaultLLMRecordsRetentionDays
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < 0 {
		return defaultLLMRecordsRetentionDays
	}
	return n
}

// startLLMRecordsRetention runs the llm_records retention cleanup once at startup
// (idempotent — expired rows may predate the setting) and then every 24h, same
// pattern as the other Manager-start background loops. Retention 0 disables it.
func (m *Manager) startLLMRecordsRetention() {
	clean := func() {
		days := m.llmRecordsRetentionDays()
		if days <= 0 {
			return
		}
		n, err := m.pg.DeleteLLMRecordsBefore(time.Now().Add(-time.Duration(days) * 24 * time.Hour))
		if err != nil {
			log.Printf("[llmrec] retention cleanup: %v", err)
		} else if n > 0 {
			log.Printf("[llmrec] retention cleanup: deleted %d records older than %d days", n, days)
		}
	}
	clean()
	go func() {
		t := time.NewTicker(24 * time.Hour)
		defer t.Stop()
		for range t.C {
			clean()
		}
	}()
}

// TrafficEnabled reports whether traffic capture is on (default off). When off,
// no proxy/traffic tools/prompt are injected into agents (nothing is recorded).
func (m *Manager) TrafficEnabled() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.trafficOn
}

// SetTrafficEnabled persists and applies the traffic-capture toggle. Callers must
// rebuild the agents (applyLLM) afterwards so the new proxy/tools/prompt take hold.
func (m *Manager) SetTrafficEnabled(on bool) error {
	if err := m.pg.SetBool(settingTrafficCapture, on); err != nil {
		return err
	}
	m.mu.Lock()
	m.trafficOn = on
	m.mu.Unlock()
	// Inject (on) or strip (off) the recording proxy + CA on the browser MCP so
	// Playwright routes through the MITM. Must run after the flag flip above, since
	// ProxyAddr/ProxyCACert honor it. putSettings rebuilds agents next (applyLLM),
	// which re-spawns the MCP with the new args/env.
	m.syncBrowserMCPProxy()
	return nil
}

// LLMRecordEnabled reports whether LLM request/response recording is on
// (デフォルト無効；settings.llm_record). The recorder consults this per call, so the
// toggle takes effect immediately without rebuilding agents.
func (m *Manager) LLMRecordEnabled() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.llmRecOn
}

// SetLLMRecordEnabled persists and applies the LLM-record toggle. Effective at
// once — no applyLLM needed, since the recorder reads the flag on every call.
func (m *Manager) SetLLMRecordEnabled(on bool) error {
	if err := m.pg.SetBool(settingLLMRecord, on); err != nil {
		return err
	}
	m.mu.Lock()
	m.llmRecOn = on
	m.mu.Unlock()
	return nil
}

// LLMPoolEnabled reports whether LLM failover ("ポーリング") is on (デフォルト無効；
// settings.llm_pool_enabled). Read when the provider chain is built (applyLLM),
// so a change requires a rebuild — putSettings does that.
func (m *Manager) LLMPoolEnabled() bool {
	if m.pg == nil {
		return false
	}
	return m.pg.GetBool(settingLLMPoolOn, false)
}

// SetLLMPoolEnabled persists the failover toggle. Callers rebuild agents
// (applyLLM) afterwards so it takes effect.
func (m *Manager) SetLLMPoolEnabled(on bool) error { return m.pg.SetBool(settingLLMPoolOn, on) }

// LLMPoolBindFallback reports whether an agent/task that is BOUND to a specific
// profile still falls back to the chain when that profile fails (デフォルト無効:紐付けたら
// 専用、失敗は失敗のまま). Only meaningful while LLMPoolEnabled.
func (m *Manager) LLMPoolBindFallback() bool {
	if m.pg == nil {
		return false
	}
	return m.pg.GetBool(settingLLMPoolBindFallback, false)
}

// SetLLMPoolBindFallback persists the bound-profile fallback toggle. Callers
// rebuild agents (applyLLM) afterwards.
func (m *Manager) SetLLMPoolBindFallback(on bool) error {
	return m.pg.SetBool(settingLLMPoolBindFallback, on)
}

// WebSearch returns the current web-search config: whether it is enabled, the
// backend ("ddgs" | "brave-free" | "tavily"), the Brave API key, the Tavily API
// key (each empty unless set), and the dedicated egress proxy (empty = direct).
func (m *Manager) WebSearch() (on bool, backend, braveKey, tavilyKey, proxy string) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	backend = m.webSearchBackend
	if backend == "" {
		backend = defaultWebSearchBackend
	}
	return m.webSearchOn, backend, m.braveKey, m.tavilyKey, m.webSearchProxy
}

// WebSearchOpts returns the config as the agent-package struct the server pushes
// into each agent. Disabled when off, or when a keyed backend is selected without
// its key (so a half-configured backend never silently drops the tool at session build).
func (m *Manager) WebSearchOpts() agent.WebSearchOpts {
	on, backend, braveKey, tavilyKey, proxy := m.WebSearch()
	if on && backend == "brave-free" && strings.TrimSpace(braveKey) == "" {
		on = false
	}
	if on && backend == "tavily" && strings.TrimSpace(tavilyKey) == "" {
		on = false
	}
	o := agent.WebSearchOpts{Enabled: on, Backend: backend, BraveKey: braveKey, TavilyKey: tavilyKey, Proxy: proxy}
	if backend == deepSeekWebSearchBackend {
		o.DeepSeekBaseURL, o.DeepSeekAPIKey, o.DeepSeekModel = m.deepSeekSearchCreds()
	}
	return o
}

// deepSeekSearchCreds resolves the credentials the "deepseek" search backend
// borrows from the active LLM profile (it has no key of its own). Whether that
// profile can actually drive server-side search — DeepSeek exposes it only on
// the Anthropic-format endpoint — is deliberately NOT validated here: the UI
// states the requirement and the user decides. A profile that can't serve it
// simply fails at search time (or at the settings page's テスト button), which is
// the same feedback every other backend gives for a bad key.
func (m *Manager) deepSeekSearchCreds() (baseURL, apiKey, model string) {
	p, err := m.pg.ActiveProfile()
	if err != nil || p == nil {
		return "", "", ""
	}
	return p.BaseURL, p.APIKey, p.Model
}

// SetWebSearch persists and applies the web-search settings. braveKey, tavilyKey, and
// proxy are each left untouched when nil (so toggling the switch doesn't wipe a saved
// key/proxy; pass a pointer to "" to clear). Callers must rebuild agents (applyLLM)
// afterwards so the settings take effect.
func (m *Manager) SetWebSearch(on bool, backend string, braveKey, tavilyKey, proxy *string) error {
	backend = strings.TrimSpace(backend)
	if backend == "" {
		backend = defaultWebSearchBackend
	}
	if err := m.pg.SetBool(settingWebSearchOn, on); err != nil {
		return err
	}
	if err := m.pg.SetSetting(settingWebSearchBackend, backend); err != nil {
		return err
	}
	m.mu.Lock()
	m.webSearchOn = on
	m.webSearchBackend = backend
	m.mu.Unlock()
	if braveKey != nil {
		if err := m.pg.SetSetting(settingBraveKey, *braveKey); err != nil {
			return err
		}
		m.mu.Lock()
		m.braveKey = *braveKey
		m.mu.Unlock()
	}
	if tavilyKey != nil {
		if err := m.pg.SetSetting(settingTavilyKey, *tavilyKey); err != nil {
			return err
		}
		m.mu.Lock()
		m.tavilyKey = *tavilyKey
		m.mu.Unlock()
	}
	if proxy != nil {
		p := strings.TrimSpace(*proxy)
		if err := m.pg.SetSetting(settingWebSearchProxy, p); err != nil {
			return err
		}
		m.mu.Lock()
		m.webSearchProxy = p
		m.mu.Unlock()
	}
	return nil
}

// browserMCPName is the seeded Playwright MCP whose proxy args + CA env are kept
// in sync with the traffic-capture toggle.
const browserMCPName = "browser"

// syncBrowserMCPProxy reconciles the seeded browser MCP's proxy args + CA env with
// the current traffic-capture state: capture on → route Playwright through the
// recording proxy and trust its MITM CA (NODE_EXTRA_CA_CERTS); capture off →
// strip both. Idempotent, and a no-op if the user deleted/renamed the MCP.
// Must be called WITHOUT m.mu held (ProxyAddr/ProxyCACert take the lock).
//
// When the proxy URL carries credentials (F2-1 proxy auth), they cannot go
// through --proxy-server: playwright-mcp forwards that flag verbatim to
// Chromium, which ignores userinfo in proxy URLs, and Playwright only answers
// the 407 challenge from launchOptions.proxy.username/password. Those fields
// exist only in the playwright-mcp JSON config file, so a managed config
// (browserMCPProxyConfigPath, 0600 — it holds the password) is written and
// attached via --config. CLI args keep precedence over the config file, so the
// user's own flags (--headless etc.) still apply.
func (m *Manager) syncBrowserMCPProxy() {
	servers, err := m.pg.ListMCP()
	if err != nil {
		log.Printf("[mcp] browser プロキシ同期: MCP 一覧の読み取りに失敗: %v", err)
		return
	}
	var srv *pgdb.MCPServer
	for _, s := range servers {
		if s.Name == browserMCPName {
			srv = s
			break
		}
	}
	if srv == nil {
		return // user removed/renamed it — leave it alone
	}

	proxy := m.ProxyAddr()  // "" when capture off
	cert := m.ProxyCACert() // "" when capture off

	raw := decodeStrSlice(srv.Args)
	args := stripProxyArgs(raw)
	cfgPath := m.browserMCPProxyConfigPath()
	args = stripManagedProxyConfig(args, cfgPath)
	env := decodeStrMap(srv.Env)
	delete(env, "NODE_EXTRA_CA_CERTS")
	if proxy != "" {
		if u, perr := url.Parse(proxy); perr == nil && u.User != nil {
			user := u.User.Username()
			pass, _ := u.User.Password()
			u.User = nil
			switch {
			case hasForeignProxyConfig(raw, cfgPath):
				// A user-supplied --config would be fully overridden by ours
				// (commander keeps the last occurrence). Rather than silently
				// dropping their settings, leave the proxy unattached and warn.
				log.Printf("[mcp] browser MCP が独自の --config を持つため、プロキシ認証情報を注入できません。ブラウザのトラフィックは記録プロキシを通りません。その config の browser.launchOptions.proxy に server=%s と username/password（パスワードは data/traffic/_auth を参照）を設定してください", u.String())
			default:
				if err := writeBrowserMCPProxyConfig(cfgPath, u.String(), user, pass); err != nil {
					log.Printf("[mcp] browser プロキシ同期: プロキシ config の書き込みに失敗: %v", err)
				} else {
					args = append(args, "--config", cfgPath)
				}
			}
		} else {
			args = append(args, "--proxy-server", proxy)
		}
		if cert != "" {
			env["NODE_EXTRA_CA_CERTS"] = cert
		}
	}
	// The managed config holds the proxy password; delete it once the args no
	// longer reference it (capture off, or auth switched off).
	if !referencesConfig(args, cfgPath) {
		_ = os.Remove(cfgPath)
	}
	srv.Args = encodeJSON(args)
	srv.Env = encodeJSON(env)
	if _, err := m.pg.SaveMCP(srv); err != nil {
		log.Printf("[mcp] browser プロキシ同期に失敗: %v", err)
		return
	}
	if proxy != "" {
		log.Printf("[mcp] browser MCP にキャプチャプロキシ %s を設定しました (CA %s)", redactURL(proxy), cert)
	} else {
		log.Printf("[mcp] browser MCP からキャプチャプロキシ設定を削除しました")
	}
}

// stripProxyArgs removes any --proxy-server/--proxy-bypass flags (both "--flag val"
// and "--flag=val" forms) so they can be re-added cleanly from current state,
// without mutating the input slice.
func stripProxyArgs(args []string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--proxy-server" || a == "--proxy-bypass" {
			i++ // skip the following value too
			continue
		}
		if strings.HasPrefix(a, "--proxy-server=") || strings.HasPrefix(a, "--proxy-bypass=") {
			continue
		}
		out = append(out, a)
	}
	return out
}

// browserMCPProxyConfigPath is the playwright-mcp JSON config ARTEX manages to
// carry proxy credentials (F2-1). It lives next to the data store and is
// written 0600 because it contains the proxy password.
func (m *Manager) browserMCPProxyConfigPath() string {
	return filepath.Join(m.dir, "browser-mcp-proxy.json")
}

// writeBrowserMCPProxyConfig persists the minimal playwright-mcp config that
// routes the browser through a credentialed proxy. CLI args keep precedence
// over a config file, so this only needs the proxy block.
func writeBrowserMCPProxyConfig(path, server, username, password string) error {
	cfg := map[string]any{
		"browser": map[string]any{
			"launchOptions": map[string]any{
				"proxy": map[string]string{
					"server":   server,
					"username": username,
					"password": password,
				},
			},
		},
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return err
	}
	_ = os.Chmod(path, 0o600) // umask backstop
	return nil
}

// stripManagedProxyConfig removes a previously injected "--config <path>"
// (both "--flag val" and "--flag=val" forms) so it can be re-added cleanly.
// Only OUR path is stripped — a user-supplied --config is left untouched.
func stripManagedProxyConfig(args []string, cfgPath string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] == "--config" && i+1 < len(args) && args[i+1] == cfgPath {
			i++ // skip our value too
			continue
		}
		if args[i] == "--config="+cfgPath {
			continue
		}
		out = append(out, args[i])
	}
	return out
}

// hasForeignProxyConfig reports whether the args carry a --config that is not
// the ARTEX-managed one.
func hasForeignProxyConfig(args []string, cfgPath string) bool {
	for i, a := range args {
		if a == "--config" && i+1 < len(args) && args[i+1] != cfgPath {
			return true
		}
		if strings.HasPrefix(a, "--config=") && a != "--config="+cfgPath {
			return true
		}
	}
	return false
}

// referencesConfig reports whether the args still attach the managed config.
func referencesConfig(args []string, cfgPath string) bool {
	for i, a := range args {
		if a == "--config" && i+1 < len(args) && args[i+1] == cfgPath {
			return true
		}
		if a == "--config="+cfgPath {
			return true
		}
	}
	return false
}

// redactURL masks the password in a proxy URL for logging.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	if _, ok := u.User.Password(); ok {
		u.User = url.UserPassword(u.User.Username(), "****")
	}
	return u.String()
}

func decodeStrSlice(raw json.RawMessage) []string {
	var out []string
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out)
	}
	return out
}

func decodeStrMap(raw json.RawMessage) map[string]string {
	out := map[string]string{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out)
	}
	return out
}

func encodeJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}
