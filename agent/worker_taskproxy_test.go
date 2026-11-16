package agent

import "testing"

// proxyForTask: タスク resolver がヒットすればタスクインスタンスを使い、未ヒット/未設定ならグローバルの SetProxy へ戻す。
func TestWorkerProxyForTask(t *testing.T) {
	w := &Worker{proxyAddr: "http://global:8788", proxyCACert: "/data/traffic/_ca/cert.pem"}

	// resolver 未設定 → グローバル。
	a, c := w.proxyForTask(7)
	if a != w.proxyAddr || c != w.proxyCACert {
		t.Errorf("resolver がない場合はグローバルプロキシを使うべき, got (%q, %q)", a, c)
	}

	// resolver が空を返す → グローバルへ戻す。
	w.SetTaskProxyResolver(func(taskID int64) (string, string) { return "", "" })
	a, c = w.proxyForTask(7)
	if a != w.proxyAddr || c != w.proxyCACert {
		t.Errorf("resolver の空結果はグローバルへ戻すべき, got (%q, %q)", a, c)
	}

	// resolver ヒット → タスクインスタンスのアドレス+CA、かつ taskID を渡す。
	w.SetTaskProxyResolver(func(taskID int64) (string, string) {
		if taskID != 7 {
			t.Errorf("resolver はタスク id 7 を受け取るべき, got %d", taskID)
		}
		return "http://task:21100", "/data/traffic-tasks/7/_ca/cert.pem"
	})
	a, c = w.proxyForTask(7)
	if a != "http://task:21100" || c != "/data/traffic-tasks/7/_ca/cert.pem" {
		t.Errorf("resolver ヒット時はタスクインスタンスを使うべき, got (%q, %q)", a, c)
	}
}
