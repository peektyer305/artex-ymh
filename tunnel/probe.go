// probe.go は 4 点セットの probe：セッション経由でターゲット側の外向き通信プローブを実際に実行する（TCP を各
// attackAddr へ、DNS 解決、HTTP の外向き通信、ICMP は任意）。応答は決定的なセンチネルマーカーで解析し、
// Conclude がエビデンスの段階分けによる方式選定を行う（pivot の骨格に倣う：TCP 外向き可→リバース socks、DNS のみ→コマンドチャネル
// 級の説明；全断→フォワード/webshell チャネルを提案）。解析と段階分けはどちらも純粋関数。
//
// TCP プローブは 3 状態判定で、「接続できたときだけ通」ではない：connect 成功 = reachable；即時 RST/接続拒否
// （refused）= reachable（経路は通っており、接続先に待受がないだけ——RST はパケットが届く証拠で、トンネルがその口を
// 待ち受ければコールバックできる）；タイムアウト/無応答 = filtered（本当にフィルタされている）。プローブ対象は常用口 443/80/53 のほか、
// トンネルポートプールのサンプル（ProbeAddrs：開始と終端を各 1 個）も含む。トンネルが実際に使うポートは必ずプローブ対象にする。
package tunnel

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/Autumn-27/artex/session"
)

// probeTimeout は 1 本のプローブコマンドのターゲット側タイムアウト。
const probeTimeout = 20 * time.Second

// TCPState は TCP プローブの 3 状態の結論。判定を「接続できたときだけ通」から経路のエビデンスの区別に変える：
// 認可されたイントラネットの現場では、ファイアウォール上の「未待受」と「パケット破棄によるフィルタ」はまったく異なる挙動——
// RST（refused）はパケットが接続先に届く証拠で、経路は通っており、トンネルがその口を待ち受ければコールバックできる；
// タイムアウトのときはパケットがフィルタされており、経路が本当に通らないと判断できる。
type TCPState string

const (
	TCPConnected TCPState = "connected" // サービスが待受中で、connect 成功
	TCPRefused   TCPState = "refused"   // 即時 RST/接続拒否：経路は通る、待受なし
	TCPFiltered  TCPState = "filtered"  // タイムアウト/無応答：フィルタされ経路不通
	TCPFail      TCPState = "fail"      // その他の失敗（bash がない、解析失敗など）
)

// ProbeResult は 1 本のプローブのエビデンス。
type ProbeResult struct {
	Name   string `json:"name"`   // tcp_1 / http_1 / dns / icmp
	Kind   string `json:"kind"`   // tcp | http | dns | icmp
	Target string `json:"target"` // プローブ対象の接続先
	OK     bool   `json:"ok"`
	State  string `json:"state,omitempty"` // tcp プローブの 3 状態（connected/refused/filtered/fail）
	Detail string `json:"detail"`          // 応答の要約（切り詰め）
}

// ProbeReport は構造化された外向き通信レポート。
type ProbeReport struct {
	SessionID int64         `json:"session_id"`
	Results   []ProbeResult `json:"results"`
}

// OKKind はある種類のプローブに通ったものがあるかを返す。
func (r *ProbeReport) OKKind(kind string) bool {
	for _, p := range r.Results {
		if p.Kind == kind && p.OK {
			return true
		}
	}
	return false
}

// FirstOKPort は最初に TCP で通った接続先ポートを返す（server の待受口の候補：コールバックが通る）。
func (r *ProbeReport) FirstOKPort() int {
	for _, p := range r.Results {
		if p.Kind != "tcp" || !p.OK {
			continue
		}
		if _, port, err := net.SplitHostPort(p.Target); err == nil {
			if n, err := strconv.Atoi(port); err == nil {
				return n
			}
		}
	}
	return 0
}

// probeCmd は決定的なセンチネル付きのプローブコマンドを組み立てる。マーカーは ARTEX_<TAG>_OK /
// ARTEX_<TAG>_FAIL の形で、解析はマーカーだけを見る（PivotHub の意味論：出力の汚染/混入で誤判定してはならない）。
func probeCmd(kind, tag, target string) string {
	ok := "ARTEX_" + tag + "_OK"
	fail := "ARTEX_" + tag + "_FAIL"
	host, port, _ := net.SplitHostPort(target)
	switch kind {
	case "tcp":
		// /dev/tcp は bash の機能、sh にない場合は bash -c で補い、bash もなければ FAIL。
		// 3 状態判定：rc=0 は接続成功、rc=124 は timeout ラッパーが判定したタイムアウト → FILTERED、
		// 出力に refused を含む（大文字小文字を問わず、各 bash 版の "Connection refused" と
		// 中国語ロケールの拒否表現をカバー）→ REFUSED（即時 RST、経路は通るが待受なし）、それ以外は FAIL。
		return fmt.Sprintf(`out=$(timeout 6 bash -c '</dev/tcp/%s/%s' 2>&1); rc=$?; `+
			`if [ $rc -eq 0 ]; then echo %s; `+
			`elif [ $rc -eq 124 ]; then echo %s_FILTERED; `+
			`elif echo "$out" | grep -qiE 'refused|拒绝'; then echo %s_REFUSED; `+
			`else echo %s_FAIL; fi`,
			host, port, ok, ok, ok, ok)
	case "http":
		// HTTP 外向き通信：実際の HTTP ステータスコードが得られたら（403/502 を含む。多くは出口プロキシの応答）通とみなす；
		// 000 = 接続層の失敗。
		return fmt.Sprintf(`code=$(curl -s -o /dev/null -m 8 -w '%%{http_code}' http://%s/ 2>/dev/null); [ -n "$code" ] && [ "$code" != "000" ] && echo %s_$code || echo %s_$code`,
			target, ok, fail)
	case "dns":
		return fmt.Sprintf(`(getent hosts %s 2>/dev/null || nslookup %s 2>/dev/null) | grep -q . && echo %s || echo %s`,
			target, target, ok, fail)
	case "icmp":
		return fmt.Sprintf(`ping -c 1 -W 2 %s >/dev/null 2>&1 && echo %s || echo %s`, target, ok, fail)
	}
	return "echo " + fail
}

// ParseProbeOutput はプローブ応答を解析する：ARTEX_<TAG>_OK / _FAIL のセンチネルだけを見る。
// 2 番目の戻り値はマーカーが見つかったか（false = チャネル異常/出力汚染。通らないものとして扱うが Detail にはありのまま記録する）。純粋関数。
func ParseProbeOutput(tag, out string) (ok, found bool) {
	if strings.Contains(out, "ARTEX_"+tag+"_OK") {
		return true, true
	}
	if strings.Contains(out, "ARTEX_"+tag+"_FAIL") {
		return false, true
	}
	return false, false
}

// ParseTCPOutput は TCP プローブの応答を 3 状態に解析する（TCPState 参照）。センチネルだけを見る；
// 接尾辞付きのマーカー（_OK_REFUSED など）は接尾辞なしの _OK/_FAIL より先にマッチさせる必要がある。純粋関数。
func ParseTCPOutput(tag, out string) (state TCPState, found bool) {
	base := "ARTEX_" + tag + "_OK"
	switch {
	case strings.Contains(out, base+"_REFUSED"):
		return TCPRefused, true
	case strings.Contains(out, base+"_FILTERED"):
		return TCPFiltered, true
	case strings.Contains(out, base+"_FAIL"):
		return TCPFail, true
	case strings.Contains(out, base):
		return TCPConnected, true
	}
	return TCPFail, false
}

// ProbeAddrs は TCP プローブ対象を組み立てる：常用コールバック口 443/80/53 + トンネルポートプールのサンプル（開始と終端
// を各 1 個、例えば 20000 と 21000）——トンネルが実際に使うポートは必ずプローブ対象にする。そうしないと「常用口がフィルタされている」
// ことが「トンネルポートプールは実は到達可能」を覆い隠す。poolRange が不正ならプールのサンプルは無視し、基本のプローブは止めない；
// 常用口と重複するプールポートは重複除去する。純粋関数。
func ProbeAddrs(host, poolRange string) []string {
	ports := []int{443, 80, 53}
	if lo, hi, err := ParsePortRange(poolRange); err == nil {
		ports = append(ports, lo)
		if hi != lo {
			ports = append(ports, hi)
		}
	}
	seen := map[int]bool{}
	out := make([]string, 0, len(ports))
	for _, p := range ports {
		if seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, net.JoinHostPort(host, strconv.Itoa(p)))
	}
	return out
}

func detailOf(out string) string {
	out = strings.TrimSpace(out)
	const max = 200
	if len(out) > max {
		out = out[:max] + "…"
	}
	return out
}

// Probe はセッション経由でターゲット側の外向き通信プローブを実行する。attackAddrs は "host:port" のリスト（通常はプラットフォームの
// callback アドレス + 常用コールバックポート）、icmp=true なら ICMP プローブも追加する（任意、ノイズが多い）。
// 個々のプローブの失敗では中断せず、エビデンスはすべてレポートに入れる；セッション自体の失敗だけが error になる。
func Probe(ctx context.Context, sess session.Session, attackAddrs []string, icmp bool) (*ProbeReport, error) {
	rep := &ProbeReport{SessionID: sess.ID()}
	run := func(kind, tag, target string) {
		stdout, _, err := sess.Exec(ctx, probeCmd(kind, tag, target), probeTimeout)
		res := ProbeResult{Name: strings.ToLower(tag), Kind: kind, Target: target}
		if err != nil {
			res.Detail = "セッションの実行に失敗しました: " + err.Error()
		} else if kind == "tcp" {
			state, found := ParseTCPOutput(tag, stdout)
			res.State = string(state)
			// refused も通とみなす：認可されたイントラネットの現場では「未待受」と「パケット破棄によるフィルタ」は別の挙動で、
			// RST はパケットが接続先に届く証拠——トンネル server がその口を待ち受ければコールバックできる。
			res.OK = state == TCPConnected || state == TCPRefused
			res.Detail = detailOf(stdout)
			if !found {
				res.Detail = "プローブのセンチネルマーカーが見つかりません（チャネル異常/出力汚染）: " + res.Detail
			}
		} else {
			ok, found := ParseProbeOutput(tag, stdout)
			res.OK = ok
			res.Detail = detailOf(stdout)
			if !found {
				res.Detail = "プローブのセンチネルマーカーが見つかりません（チャネル異常/出力汚染）: " + res.Detail
			}
		}
		rep.Results = append(rep.Results, res)
	}
	for i, addr := range attackAddrs {
		run("tcp", "TCP_"+strconv.Itoa(i+1), addr)
	}
	for i, addr := range attackAddrs {
		run("http", "HTTP_"+strconv.Itoa(i+1), addr)
	}
	dnsHost := ""
	if len(attackAddrs) > 0 {
		dnsHost, _, _ = net.SplitHostPort(attackAddrs[0])
	}
	if dnsHost == "" || net.ParseIP(dnsHost) != nil {
		dnsHost = "www.baidu.com" // 接続先が IP のときは公衆ドメインに変えて測り、DNS 再帰そのものを検証する
	}
	run("dns", "DNS", dnsHost)
	if icmp {
		icmpHost := dnsHost
		if net.ParseIP(icmpHost) == nil {
			icmpHost = "223.5.5.5"
		}
		run("icmp", "ICMP", icmpHost)
	}
	return rep, nil
}

// エビデンスの段階分け（Conclusion.Grade）。
const (
	GradeTCPEgress = "tcp_egress" // TCP 外向き可 → リバース socks
	GradeHTTPOnly  = "http_only"  // HTTP のみ外向き可
	GradeDNSOnly   = "dns_only"   // DNS のみ外向き可
	GradeICMPOnly  = "icmp_only"  // ICMP のみ
	GradeIsolated  = "isolated"   // 全断
)

// Conclusion はエビデンスの段階分けによる方式選定の結論。
type Conclusion struct {
	Grade            string `json:"grade"`
	Feasible         bool   `json:"feasible"`                 // リバーストンネル（chisel）をそのまま使えるか
	SuggestedPort    int    `json:"suggested_port,omitempty"` // 推奨する server 待受口（最初に TCP で通った口）
	SuggestedAdapter string `json:"suggested_adapter"`        // 推奨アダプタ：chisel | suo5
	Recommendation   string `json:"recommendation"`
}

// Conclude はエビデンスの段階分けによる方式選定（純粋関数。段階は厳密に区別し混同しない。pivot の骨格に倣う）：
//   - いずれかの TCP ターゲットが reachable（refused を含む：RST はパケットが届く証拠で、接続先に待受がないだけ。
//     トンネル server がその口を待ち受ければコールバックできる）→ tcp_egress、リバース socks（chisel --reverse）、
//     待受口は最初に通った口を取る；
//   - すべて filtered（タイムアウトで破棄）だが HTTP は通る → http_only：HTTP で運ぶ——suo5 は受信方向の
//     HTTP を使い、外向き通信に依存しないため優先；または chisel を WebSocket over 443 で通して特徴を変える；
//   - すべて filtered だが DNS は通る → dns_only：コマンドチャネル級（KB/s 以下）。データトンネルを張らない；
//   - ICMP のみ → icmp_only：制御シグナリング級；
//   - 全断 → no_egress（isolated）：リバーストンネルは不可。ただしプローブ自体は webshell セッション経由で完了している
//     （= webshell は必ず使える）→ suo5 を推奨（HTTP トンネル、ターゲットの外向き通信に依存しない）。
func Conclude(rep *ProbeReport) Conclusion {
	if rep.OKKind("tcp") {
		return Conclusion{
			Grade:            GradeTCPEgress,
			Feasible:         true,
			SuggestedPort:    rep.FirstOKPort(),
			SuggestedAdapter: AdapterChisel,
			Recommendation: "ターゲットは TCP で外向き通信できる（connect 成功も refused 接続拒否も通とみなす——RST は経路が到達可能な証拠で、" +
				"接続先に待受がないだけ）：リバース socks トンネルを直接張る（tunnel_deploy adapter=chisel kind=socks）。" +
				"プラットフォームの chisel server 待受口は最初に通ったコールバックポート（トンネルポートプールのサンプル口を含む）を優先する；チャネルポートは 443>80>53 を優先し、4444/8888 を避ける。",
		}
	}
	if rep.OKKind("http") {
		return Conclusion{
			Grade:            GradeHTTPOnly,
			Feasible:         true,
			SuggestedAdapter: AdapterSuo5,
			Recommendation: "HTTP のみ外向き通信可（強制プロキシ/許可リストの可能性）：tunnel_deploy adapter=suo5 を優先" +
				"（HTTP トンネル：payload をターゲットの Web ディレクトリに配信し、プラットフォームが受信方向の HTTP で socks5 を張る。ターゲットの外向き通信に一切依存しない）、" +
				"または chisel client を WebSocket over 443 で通すことを試し、警報ウィンドウにわざとぶつからないようにする。",
		}
	}
	if rep.OKKind("dns") {
		return Conclusion{
			Grade:            GradeDNSOnly,
			Feasible:         false,
			SuggestedAdapter: AdapterSuo5,
			Recommendation: "DNS のみ外向き通信可：データトンネルは tunnel_deploy adapter=suo5（HTTP トンネル、外向き通信に依存しない）、" +
				"DNS は低頻度のコマンドチャネル（dnscat2/iodine の類、KB/s 級）にしか使えず、DNS 上にデータトンネルを張らない。",
		}
	}
	if rep.OKKind("icmp") {
		return Conclusion{
			Grade:            GradeICMPOnly,
			Feasible:         false,
			SuggestedAdapter: AdapterSuo5,
			Recommendation: "ICMP のみ到達可能：制御シグナリング級のチャネル（icmpsh の類）；データトンネルは tunnel_deploy adapter=suo5" +
				"（HTTP トンネル、受信方向の Web 口経由、外向き通信に依存しない）。",
		}
	}
	return Conclusion{
		Grade:            GradeIsolated,
		Feasible:         false,
		SuggestedAdapter: AdapterSuo5,
		Recommendation: "4 種類のプローブが全断し、ターゲットに外向き通信がない：リバーストンネル（chisel）は不可。ただし本プローブは webshell セッション経由で完了しており、" +
			"webshell は必ず使える——tunnel_deploy adapter=suo5 を推奨（payload を webshell と同じディレクトリに配信し、" +
			"プラットフォームが受信方向の HTTP で socks5 を張る。外向き通信のない状況をカバー）。その他の方向転換：① ターゲットの受信方向に穴があればフォワードトンネルを張る；" +
			"② webshell セッションのチャネルを続けて使う（session_exec がイントラネットの実行面）、③ 許可リストのドメインを定期的に pull して指令を取る。",
	}
}
