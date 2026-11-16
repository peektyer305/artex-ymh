package agent

// バッチ 5 B4（設計ドキュメント HONEYPOT-DETECTION-DESIGN.md 二-B「自己防護」第 4 条）: クライアント UA プール。
// 実環境のサンプルでハニーポットのトリガーになるのは「UA/TLS フィンガープリントの不一致」——裸の Chrome UA + curl の TLS という
// 中途半端な偽装は、しないに越したことはない。プラットフォームはタスクごとに主流 UA を 1 つ安定して割り当て、BashEnv 経由で ARTEX_UA に注入し、
// worker の HTTP クライアント（curl/httpx など）はタスク全体で同じものを使う。切り替えない、中途半端な偽装もしない。

import (
	"encoding/binary"
	"encoding/json"
	"hash/fnv"
	"strings"
)

// DefaultUAPool は settings キー web_ua_pool が未設定のときのデフォルトプール: 主流 Chrome 2 種
// （Windows x64、最近のバージョン）。
func DefaultUAPool() []string {
	return []string{
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36",
	}
}

// UAPoolFromSetting は settings キー web_ua_pool（JSON 文字列配列）を解析する。空/不正/
// 解析後に有効な項目がない場合はデフォルトプールへ戻す。
func UAPoolFromSetting(raw string) []string {
	var pool []string
	if err := json.Unmarshal([]byte(raw), &pool); err == nil {
		out := make([]string, 0, len(pool))
		for _, ua := range pool {
			if ua = strings.TrimSpace(ua); ua != "" {
				out = append(out, ua)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return DefaultUAPool()
}

// PickUA はタスクごとに安定して割り当てる: hash(taskID) % len(pool)。同じタスクは常に同じ UA を
// 受け取る（タスク内で変えないのは規律の一部）。pool が空ならデフォルトプールへ戻す。
func PickUA(pool []string, taskID int64) string {
	if len(pool) == 0 {
		pool = DefaultUAPool()
	}
	h := fnv.New64a()
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], uint64(taskID))
	_, _ = h.Write(buf[:])
	return pool[h.Sum64()%uint64(len(pool))]
}
