package server

import (
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"
)

// 台帳外の待ち受けポート監査（F13 軽量版、観測のみで対処しない）：worker の Bash は
// 任意の待ち受けを起動できる（python -m http.server のような孤児サービスはよくある）。
// プラットフォームは 5 分ごとにローカルの待ち受けを走査し、台帳になく非 loopback に
// バインドされた待ち受けを WARN で記録する。自動 kill はしない。まず観測のみ。
// プラットフォームが知らない正当なサービスを誤って殺すほうが、見逃すより悪い。

// knownManagedPorts はプラットフォーム台帳内の待ち受けポート：メインサービス（8787、
// stage のダウンロードもこのポートを再利用）、トラフィック記録プロキシ（8788）、SSH（22）、
// さらに動的に登録された管理ポート（トンネル server の待ち受け口など。詳細は registerManagedPort を参照）。
func knownManagedPorts() map[int]bool {
	known := map[int]bool{
		8787: true, // メイン HTTP（UI/API + /s/ の管理下キャッシュダウンロード）
		8788: true, // トラフィック記録プロキシ
		22:   true, // sshd
	}
	dynManagedPorts.Range(func(k, _ any) bool {
		known[k.(int)] = true
		return true
	})
	return known
}

// dynManagedPorts は実行時に動的登録される管理下の待ち受けポート（トンネル chisel server の
// 待ち受け口など）。管理下リソースの原則：プラットフォーム自身が起動した待ち受けはすべて
// 台帳に登録し、ポート監査で誤検知しないようにする。
var dynManagedPorts sync.Map // map[int]struct{}

// registerManagedPort は管理下の待ち受けポートを登録する（トンネル server の起動時に呼ぶ）。
func registerManagedPort(port int) { dynManagedPorts.Store(port, struct{}{}) }

// unregisterManagedPort は管理下の待ち受けポートを登録解除する（トンネル teardown/ロールバック時に呼ぶ）。
func unregisterManagedPort(port int) { dynManagedPorts.Delete(port) }

const portAuditInterval = 5 * time.Minute

// listenSock は /proc/net/tcp{,6} の LISTEN 状態のローカル待ち受け 1 件。
type listenSock struct {
	IP   net.IP
	Port int
}

// startPortAudit は startLLMRecordsRetention のパターンに倣う：起動時に 1 回走査し、
// 以降は 5 分ごと。Linux（/proc/net/tcp{,6}）のみで、他のプラットフォームはスキップする。
func (m *Manager) startPortAudit() {
	if runtime.GOOS != "linux" {
		return
	}
	audit := func() {
		for _, w := range scanUnmanagedListeners() {
			log.Printf("[portaudit] WARN 台帳外の待ち受け: %s（非 loopback かつプラットフォームのポート台帳に無い。忘れられた一時サービスなら対処してください）", w)
		}
	}
	audit()
	go func() {
		t := time.NewTicker(portAuditInterval)
		defer t.Stop()
		for range t.C {
			audit()
		}
	}()
}

// scanUnmanagedListeners は /proc/net/tcp{,6} を読み、「台帳外ポートかつ非 loopback」の
// 待ち受け記述をすべて返す。ファイルが読めない場合（コンテナでの切り詰めなど）は黙って
// スキップ。監査の機能低下は主処理に影響しない。
func scanUnmanagedListeners() []string {
	known := knownManagedPorts()
	var out []string
	seen := map[string]bool{}
	for _, path := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, ls := range parseProcNetTCP(string(data)) {
			if known[ls.Port] || ls.IP.IsLoopback() {
				continue
			}
			desc := fmt.Sprintf("%s:%d", ls.IP, ls.Port)
			if !seen[desc] {
				seen[desc] = true
				out = append(out, desc)
			}
		}
	}
	return out
}

// parseProcNetTCP は /proc/net/tcp または /proc/net/tcp6 の内容を解析し、LISTEN（状態 0A）
// のローカルアドレスを返す。純粋関数なので fixture での単体テストが容易。
func parseProcNetTCP(content string) []listenSock {
	var out []listenSock
	for _, line := range strings.Split(content, "\n") {
		f := strings.Fields(line)
		// 列：sl local_address rem_address st ...（データ行の先頭列は "  0:" の形）
		if len(f) < 4 || !strings.HasSuffix(f[0], ":") || f[3] != "0A" {
			continue
		}
		ip, port, err := parseProcAddr(f[1])
		if err != nil {
			continue
		}
		out = append(out, listenSock{IP: ip, Port: port})
	}
	return out
}

// parseProcAddr は "0100007F:1F91" のような hex アドレスを解析する。IPv4 は 8 hex
// （全体がリトルエンディアン）、IPv6 は 32 hex（32 ビット語 4 個、各語の内部がリトルエンディアン）。
func parseProcAddr(s string) (net.IP, int, error) {
	host, portHex, ok := strings.Cut(s, ":")
	if !ok {
		return nil, 0, fmt.Errorf("bad addr %q", s)
	}
	var port int
	if _, err := fmt.Sscanf(portHex, "%X", &port); err != nil {
		return nil, 0, err
	}
	raw, err := hex.DecodeString(host)
	if err != nil {
		return nil, 0, err
	}
	switch len(raw) {
	case 4: // IPv4、リトルエンディアン
		return net.IPv4(raw[3], raw[2], raw[1], raw[0]), port, nil
	case 16: // IPv6、32 ビット語ごとにリトルエンディアン
		ip := make(net.IP, 16)
		for w := 0; w < 4; w++ {
			ip[w*4+0] = raw[w*4+3]
			ip[w*4+1] = raw[w*4+2]
			ip[w*4+2] = raw[w*4+1]
			ip[w*4+3] = raw[w*4+0]
		}
		return ip, port, nil
	}
	return nil, 0, fmt.Errorf("bad addr len %d", len(raw))
}
