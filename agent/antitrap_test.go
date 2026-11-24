package agent

import (
	"strings"
	"testing"
)

// バッチ 5 B1: 対 AI ハニーポットのレッドラインはコード固定テール。非イントラネットの worker は必ずレッドライン部分を含み、イントラネット worker は
// 2 段（対 AI レッドライン + イントラネットのレッドライン）がそろう。DB の本文編集ではレッドライン部分を消せない。
func TestWorkerSystemAntiTrapRules(t *testing.T) {
	t.Cleanup(func() { PromptOverride = nil })
	PromptOverride = nil

	// 非イントラネット: 対 AI レッドラインを含み、イントラネットのレッドラインは含まない。
	ext := workerSystem("", "", "/data", "/data", false)
	if !strings.Contains(ext, "対 AI ハニーポットのレッドライン") {
		t.Fatalf("non-intranet worker missing anti-trap rules: %q", ext)
	}
	if strings.Contains(ext, "内部ネットワークのレッドライン") {
		t.Fatalf("non-intranet worker must not append the intranet redline tail: %q", ext)
	}

	// イントラネット: 2 段のレッドラインがそろう。
	in := workerSystem("", "", "/data", "/data", true)
	if !strings.Contains(in, "対 AI ハニーポットのレッドライン") || !strings.Contains(in, "内部ネットワークのレッドライン") {
		t.Fatalf("intranet worker should carry both rule tails: %q", in)
	}

	// DB の上書き本文（intranet 変種を含む）でもレッドライン部分は消せない。
	PromptOverride = func(k string) (string, bool) { return "ユーザーが編集した本文", true }
	if got := workerSystem("", "", "/data", "/data", false); !strings.Contains(got, "対 AI ハニーポットのレッドライン") {
		t.Fatalf("edited body must not drop anti-trap rules: %q", got)
	}
	if got := workerSystem("", "", "/data", "/data", true); !strings.Contains(got, "対 AI ハニーポットのレッドライン") || !strings.Contains(got, "内部ネットワークのレッドライン") {
		t.Fatalf("edited intranet body must not drop either rule tail: %q", got)
	}
}
