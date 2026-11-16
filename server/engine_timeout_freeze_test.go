package server

import (
	"testing"
	"time"
)

// 時計の跳び検出の純粋関数:計画/実際の睡眠時間(偽の時計に相当)を注入し、凍結判定と繰り延べ量を検証する。
func TestDetectFreeze(t *testing.T) {
	cases := []struct {
		name    string
		planned time.Duration
		actual  time.Duration
		want    time.Duration
	}{
		{"正常なポーリング 2s は繰り延べなし", 2 * time.Second, 2*time.Second + 50*time.Millisecond, 0},
		{"正常な nap 30s は繰り延べなし", 30 * time.Second, 30*time.Second + 500*time.Millisecond, 0},
		{"初回 planned=0 は即起床で繰り延べなし", 0, 10 * time.Millisecond, 0},
		{"65s の跳びは 65s 繰り延べ", 2 * time.Second, 67 * time.Second, 65 * time.Second},
		{"7 時間の凍結は差分を繰り延べ", 30 * time.Second, 7*time.Hour + 30*time.Second, 7 * time.Hour},
		{"ちょうどしきい値未満は繰り延べなし", 30 * time.Second, 30*time.Second + freezeDetectThreshold - time.Second, 0},
		{"時計の巻き戻しは繰り延べなし", 30 * time.Second, 5 * time.Second, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := detectFreeze(c.planned, c.actual); got != c.want {
				t.Fatalf("detectFreeze(%v, %v) = %v, want %v", c.planned, c.actual, got, c.want)
			}
		})
	}
}

// 連続する小さな跳び:各回はしきい値未満で、累積も繰り延べもしない。
func TestDetectFreezeConsecutiveSmallJumps(t *testing.T) {
	total := time.Duration(0)
	for i := 0; i < 5; i++ {
		// 各回 30 秒余分に眠る(60 秒のしきい値未満)。5 回連続で計 150 秒を超えても繰り延べすべきではない。
		total += detectFreeze(2*time.Second, 32*time.Second)
	}
	if total != 0 {
		t.Fatalf("連続する小さな跳びの累計繰り延べ %v, want 0", total)
	}
}

// 繰り延べの累積が正しい：2 回の大きな跳びをそれぞれ検出し、呼び出し側が加算する。
func TestDetectFreezeAccumulates(t *testing.T) {
	var shift time.Duration
	shift += detectFreeze(30*time.Second, 30*time.Second+65*time.Second)
	shift += detectFreeze(30*time.Second, 30*time.Second+time.Hour)
	want := 65*time.Second + time.Hour
	if shift != want {
		t.Fatalf("累計繰り延べ %v, want %v", shift, want)
	}
}
