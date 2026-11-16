package guard

// バッチ 5 B2（設計ドキュメント HONEYPOT-DETECTION-DESIGN.md 二-B「自己防護」第 3 条）:出口審査。
// 対 AI ハニーポットの狙いはプラットフォームの機密情報（LLM API key、jwt.key、gate 入口パス、
// PG DSN パスワード、callback_addr）。本ファイルは guard に機密フィンガープリントのインターセプトを
// 組み込む:登録時は原文の SHA256 先頭部分（16 個の 16 進文字 = 8 バイト）のみを保持し、原文は保存しない。
// 検査時は登録済みの原文長の窓を検査対象テキスト上で滑らせてハッシュを取る。命中即 deny。
//
// コスト:スライド窓ハッシュは O(テキスト長 × 原文長) × 異なる長さの個数。検査対象は Bash 系
// コマンド全文と WebFetch の url/body のみで、通常は KB 級なのでコストは無視できる。

import (
	"crypto/sha256"
	"crypto/subtle"
	"sync"
)

// egressMinSecretLen は登録するフィンガープリントの原文の最短長。これより短い文字列（弱いパスワードなど）は
// 通常のコマンドに出現する頻度が高すぎ、スライド窓が必ず誤検知するため登録を拒否する。
const egressMinSecretLen = 8

// egressFingerprint は 1 件の機密フィンガープリント:ハッシュの先頭部分と原文長のみを保持し、原文は決して保持しない。
type egressFingerprint struct {
	kind   string
	sum    [sha256.Size]byte
	length int // 原文長。スライド窓のウィンドウサイズ
}

// EgressGuard は機密フィンガープリントの集合で、プロセス内で共有する（server の組み立て時に登録し、
// すべてのタスクの guard が同じ 1 つを参照する。profile 変更時に再登録すれば全タスクへ反映）。並行安全。
type EgressGuard struct {
	mu sync.RWMutex
	fps []egressFingerprint
	// byLen は原文長でグループ化したフィンガープリント索引。Check ではテキスト内で各長さにつき 1 回ずつ窓を滑らせるだけでよい。
	byLen map[int][]int // length → fps の添字
}

func NewEgressGuard() *EgressGuard {
	return &EgressGuard{byLen: map[int][]int{}}
}

// AddFingerprint は機密フィンガープリントを 1 件登録する（内部でハッシュ化し、原文は保持しない）。空文字列/短すぎる文字列は黙ってスキップする。
// 同じ (kind, 原文) の重複登録は冪等な no-op。
func (e *EgressGuard) AddFingerprint(kind, secret string) {
	if len(secret) < egressMinSecretLen {
		return
	}
	sum := sha256.Sum256([]byte(secret))
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, fp := range e.fps {
		if fp.kind == kind && fp.length == len(secret) && fp.sum == sum {
			return // 冪等:重複登録しても重複項目は生じない
		}
	}
	e.fps = append(e.fps, egressFingerprint{kind: kind, sum: sum, length: len(secret)})
	e.byLen[len(secret)] = append(e.byLen[len(secret)], len(e.fps)-1)
}

// RemoveKind はある kind のフィンガープリントをすべて取り除く（LLM profile 変更時に llm_api_key を丸ごと差し替える用途）。
func (e *EgressGuard) RemoveKind(kind string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	kept := e.fps[:0]
	for _, fp := range e.fps {
		if fp.kind != kind {
			kept = append(kept, fp)
		}
	}
	e.fps = kept
	e.byLen = map[int][]int{}
	for i, fp := range e.fps {
		e.byLen[fp.length] = append(e.byLen[fp.length], i)
	}
}

// Check は text 上でスライド窓ハッシュの照合を行う。命中時はそのフィンガープリントの kind を返す。
// 各候補フィンガープリントとの比較は subtle.ConstantTimeCompare を用い、命中位置によって比較自体を早期終了しない。
func (e *EgressGuard) Check(text string) (kind string, hit bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if len(e.fps) == 0 || len(text) < egressMinSecretLen {
		return "", false
	}
	for length, idxs := range e.byLen {
		if length > len(text) {
			continue
		}
		for i := 0; i+length <= len(text); i++ {
			sum := sha256.Sum256([]byte(text[i : i+length]))
			for _, idx := range idxs {
				if subtle.ConstantTimeCompare(sum[:], e.fps[idx].sum[:]) == 1 {
					return e.fps[idx].kind, true
				}
			}
		}
	}
	return "", false
}
