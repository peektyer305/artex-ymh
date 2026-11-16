// Package tunnel は多層プロキシ（イントラネット期 3a）の中核サブシステム。
// probe/plan/deploy/health の 4 点セット（docs/architecture/intranet-pivot.md §4.3）と、
// 管理対象リソースの原則（§4.3a）からなる。
//
// 意味づけは PivotHub pivothub（Go での書き直し。丸写しではない）を参考にし、
// その 3 つの欠陥を修正している：
//  1. デプロイパラメータを完全に永続化（deploy_params JSONB にセッション参照/
//     auth/リモートパス/検証対象を含む）。断線時の自動再起動は本物の機能で、
//     名前だけの飾りではない；
//  2. 破棄時のフォールバックは【リモート】のプロセスを調べる（セッション経由の
//     kill -0 + /proc/<pid>/cmdline の照合）。PivotHub のように自ホストを調べる
//     誤りを犯さない；
//  3. 死んだリンクを生きた上流として扱わない：トンネル内の決定的な検証（socks5
//     経由の実疎通確認 / portfwd による target:port への実アクセス）を通った
//     ときだけ alive を付ける。リバース待ち受け口の accept ≠ 目標への到達性。
//
// トンネルは寿命の長い管理対象リソース。プラットフォーム server プロセスの pid/
// コマンドライン、目標 client の pid、stage 配信エントリは、すべて deploy_params
// の台帳に保存する。使い終わったら必ず Teardown すること。agent 側から見えるのは
// tunnel_* ツールだけで、素の Bash によるトンネルプロセスの自前起動は許されない
// （ポート監査/回収のフォールバックで検出される）。
package tunnel

import (
	"fmt"
	"strconv"
	"strings"
)

// トンネル種別とアダプタ。
const (
	KindSocks     = "socks"
	KindPortfwd   = "portfwd"
	AdapterChisel = "chisel"
)

// DefaultPortRange はプラットフォーム側トンネルのポートプールのデフォルト値（env ARTEX_TUNNEL_PORT_RANGE で上書き可能）。
const DefaultPortRange = "20000-21000"

// ParsePortRange は "min-max" 形式のポートプールを解析する。純関数。
func ParsePortRange(s string) (min, max int, err error) {
	s = strings.TrimSpace(s)
	if s == "" {
		s = DefaultPortRange
	}
	a, b, ok := strings.Cut(s, "-")
	if !ok {
		return 0, 0, fmt.Errorf("ポートプールの形式は min-max である必要があります（例 %s）。実際は %q", DefaultPortRange, s)
	}
	min, err1 := strconv.Atoi(strings.TrimSpace(a))
	max, err2 := strconv.Atoi(strings.TrimSpace(b))
	if err1 != nil || err2 != nil || min <= 0 || max > 65535 || min > max {
		return 0, 0, fmt.Errorf("不正なポートプール %q（1-65535 かつ min<=max である必要があります）", s)
	}
	return min, max, nil
}

// AllocatePort はポートプール [min,max] から、used 集合になく実際にバインドできる
// ポートを 1 つ選ぶ。canListen は実バインドの検査を注入する（本番は net.Listen で
// 試し、テストは純関数）。純粋なロジックなのでテストできる。
func AllocatePort(min, max int, used map[int]bool, canListen func(port int) bool) (int, error) {
	for p := min; p <= max; p++ {
		if used[p] {
			continue
		}
		if canListen != nil && !canListen(p) {
			continue
		}
		return p, nil
	}
	return 0, fmt.Errorf("ポートプール %d-%d に利用可能なポートがありません", min, max)
}
