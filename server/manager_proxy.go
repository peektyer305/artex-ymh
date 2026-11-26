package server

import (
	"context"
	"strings"
	"time"

	pgdb "github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/traffic"
	actool "github.com/Autumn-27/norma/tool"
)

// HostTools are runtime host-provided tools added to EVERY agent's base list (via
// ToolAugment); the tools table then filters them per-agent binding. Currently the
// traffic tools, gated by the global capture switch: empty when capture is off, so
// no agent gets traffic_search/traffic_get regardless of binding.
func (m *Manager) HostTools() []actool.CoreTool {
	if m.traffic == nil || !m.TrafficEnabled() {
		return nil
	}
	return m.traffic.Tools()
}

func (m *Manager) Assets() *pgdb.AssetStore { return m.assets }

func (m *Manager) PG() *pgdb.DB { return m.pg }

func (m *Manager) Traffic() *traffic.Traffic { return m.traffic }

// ProxyAddr returns the egress proxy address agents route target traffic through:
//   - capture ON  → the recording MITM proxy (which itself exits via the global
//     proxy when one is set); agents also get its CA (see ProxyCACert).
//   - capture OFF → the global egress proxy directly (empty CA — real target
//     certs), or "" when no global proxy is set (direct, no recording).
//
// So the global proxy takes effect in both modes: at the MITM's upstream when
// capturing, in the agent's own bash env / WebFetch when not.
func (m *Manager) ProxyAddr() string {
	if m.traffic != nil && m.TrafficEnabled() {
		return m.traffic.ProxyAddr()
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.globalProxy
}

// ProxyCACert returns the CA cert path agents must trust to verify HTTPS through
// the egress proxy. Non-empty ONLY when traffic capture is on (the MITM re-signs
// certs): the global proxy used directly (capture off) is a plain forwarder that
// preserves real target certs, so no custom CA is needed there. Its emptiness is
// also the worker's "recording off" signal (see workerSystem).
func (m *Manager) ProxyCACert() string {
	if m.traffic == nil || !m.TrafficEnabled() {
		return ""
	}
	return m.traffic.CACertPath()
}

// GlobalProxy returns the configured global egress proxy URL (empty = direct).
func (m *Manager) GlobalProxy() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.globalProxy
}

// TaskProxyForTask is the worker's per-task proxy resolver (フェーズ 3b): when capture
// is on and the task-proxy feature is enabled it lazily builds/returns the task's
// own MITM instance (addr + CA); "" → the worker falls back to the global
// SetProxy value. Runs on its own short-timeout context so a cancelled worker
// run can't abort instance creation halfway.
func (m *Manager) TaskProxyForTask(taskID int64) (addr, caCert string) {
	if m.taskProxy == nil || !m.TrafficEnabled() {
		return "", ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return m.taskProxy.ForTask(ctx, taskID)
}

// RefreshTaskProxy re-resolves one task's MITM upstream after a tunnel state
// change (tunnel.Manager.OnStateChange). No-op when the feature is off or the
// task has no live instance (lazy-build semantics).
func (m *Manager) RefreshTaskProxy(taskID int64) {
	if m.taskProxy == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	m.taskProxy.Refresh(ctx, taskID)
}

// CloseTaskProxy destroys a task's MITM instance (task deleted/archived). The
// recorded tree (incl. CA dir) stays on disk — evidence is not instance lifetime.
func (m *Manager) CloseTaskProxy(taskID int64) {
	if m.taskProxy != nil {
		m.taskProxy.CloseTask(taskID)
	}
}

// SetGlobalProxy validates, persists and applies the global egress proxy
// (http/https/socks5, optional user:pass; empty = direct). It updates the MITM's
// upstream immediately; callers must rebuild agents (applyLLM) afterwards so the
// capture-off path (bash env / WebFetch) picks up the change too.
func (m *Manager) SetGlobalProxy(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw != "" {
		if _, err := traffic.ValidateProxyURL(raw); err != nil {
			return err
		}
	}
	if err := m.pg.SetSetting(settingGlobalProxy, raw); err != nil {
		return err
	}
	m.mu.Lock()
	m.globalProxy = raw
	m.mu.Unlock()
	if m.traffic != nil {
		if err := m.traffic.SetUpstreamProxy(raw); err != nil {
			return err
		}
	}
	// トンネルを持たないタスクの MITM インスタンスはグローバルプロキシをフォールバック上流にする。グローバルプロキシが変われば一緒に切り替わる。
	if m.taskProxy != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		m.taskProxy.RefreshAll(ctx)
		cancel()
	}
	// Keep the browser MCP's egress in sync with the new global proxy too.
	m.syncBrowserMCPProxy()
	return nil
}

func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.taskProxy != nil {
		m.taskProxy.Close()
	}
	if m.traffic != nil {
		m.traffic.Close()
	}
	return m.pg.Close()
}
