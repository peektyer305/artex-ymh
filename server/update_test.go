package server

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/Autumn-27/artex/selfupdate"
)

// releaseCache は GitHub のクォータを守る層である：未認証の API は 1 時間/IP あたり
// 60 回しかなく、トップバーの「新しいバージョンがあります」通知はページを読み込むたびに
// 1 回問い合わせる。キャッシュが効かないと、ユーザーがタブをいくつか開いただけで
// クォータを使い切り、いざ更新したいときに問い合わせすらできなくなる。

func newTestCache(fetch func(context.Context, *http.Client) (*selfupdate.Release, error)) *releaseCache {
	return &releaseCache{fetch: fetch}
}

func TestReleaseCacheServesFromCache(t *testing.T) {
	calls := 0
	c := newTestCache(func(context.Context, *http.Client) (*selfupdate.Release, error) {
		calls++
		return &selfupdate.Release{TagName: "v0.3.8"}, nil
	})

	for range 5 {
		rel, err := c.get(t.Context(), nil, false)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if rel.TagName != "v0.3.8" {
			t.Fatalf("TagName = %q", rel.TagName)
		}
	}
	if calls != 1 {
		t.Errorf("5 回の取得でオリジンへの問い合わせは 1 回だけのはずですが、実際は %d 回でした", calls)
	}
}

func TestReleaseCacheForceBypasses(t *testing.T) {
	calls := 0
	c := newTestCache(func(context.Context, *http.Client) (*selfupdate.Release, error) {
		calls++
		return &selfupdate.Release{TagName: "v0.3.8"}, nil
	})

	if _, err := c.get(t.Context(), nil, false); err != nil {
		t.Fatal(err)
	}
	// ユーザーが「更新を確認」を押したら必ず最新の結果を返す必要がある。そうでないと、
	// 出たばかりのバージョンはキャッシュが切れるまで見えない。
	if _, err := c.get(t.Context(), nil, true); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("force はキャッシュを迂回するはずで、オリジンへの 2 回の問い合わせを期待しましたが、実際は %d 回でした", calls)
	}
}

func TestReleaseCacheExpiresAfterTTL(t *testing.T) {
	calls := 0
	c := newTestCache(func(context.Context, *http.Client) (*selfupdate.Release, error) {
		calls++
		return &selfupdate.Release{TagName: "v0.3.8"}, nil
	})

	if _, err := c.get(t.Context(), nil, false); err != nil {
		t.Fatal(err)
	}
	// 保存時刻をちょうど失効した時点まで戻し、TTL 満了を模擬する。
	c.at = time.Now().Add(-releaseTTL - time.Second)
	if _, err := c.get(t.Context(), nil, false); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("TTL 失効後は再取得が必要で、2 回を期待しましたが、実際は %d 回でした", calls)
	}
}

func TestReleaseCacheUsesShorterTTLForErrors(t *testing.T) {
	calls := 0
	c := newTestCache(func(context.Context, *http.Client) (*selfupdate.Release, error) {
		calls++
		return nil, errors.New("github に到達できません")
	})

	if _, err := c.get(t.Context(), nil, false); err == nil {
		t.Fatal("エラーが返ることを期待しました")
	}
	// 失敗した結果も少しの間キャッシュする。そうしないと GitHub に到達できない間、
	// ページを読み込むたびにタイムアウトを待つことになる。
	if _, err := c.get(t.Context(), nil, false); err == nil {
		t.Fatal("エラーが返ることを期待しました")
	}
	if calls != 1 {
		t.Errorf("エラーは短時間キャッシュされるはずで、1 回の問い合わせを期待しましたが、実際は %d 回でした", calls)
	}

	// ただしエラーの TTL は成功時より明らかに短くなければならず、ネットワークが
	// 復旧したらすぐに自己回復できる必要がある。
	if releaseErrTTL >= releaseTTL {
		t.Fatalf("エラー TTL（%v）は成功 TTL（%v）より短くなければなりません", releaseErrTTL, releaseTTL)
	}
	c.at = time.Now().Add(-releaseErrTTL - time.Second)
	if _, err := c.get(t.Context(), nil, false); err == nil {
		t.Fatal("エラーが返ることを期待しました")
	}
	if calls != 2 {
		t.Errorf("エラー TTL の失効後は再試行が必要で、2 回を期待しましたが、実際は %d 回でした", calls)
	}
}

func TestReleaseCacheDoesNotPoisonOnCallerCancel(t *testing.T) {
	good := &selfupdate.Release{TagName: "v0.3.8"}
	c := newTestCache(func(ctx context.Context, _ *http.Client) (*selfupdate.Release, error) {
		return good, nil
	})
	if _, err := c.get(t.Context(), nil, false); err != nil {
		t.Fatal(err)
	}

	// 訪問者がタブを閉じるとリクエストはキャンセルされる。それは GitHub 側の問題では
	// ないので、「キャンセル済み」を絶対にキャッシュへ書いてはならない。さもないと
	// 次の 30 分間、すべての訪問者が不可解なエラーを受け取ることになる。
	c.fetch = func(ctx context.Context, _ *http.Client) (*selfupdate.Release, error) {
		return nil, ctx.Err()
	}
	c.at = time.Now().Add(-releaseTTL - time.Second) // キャッシュを失効させて問い合わせを強制する

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.get(ctx, nil, false); err == nil {
		t.Fatal("呼び出し側がキャンセルした場合はそのエラーをそのまま返す必要があります")
	}

	// 重要な不変条件：キャンセルされたあの 1 回は何も痕跡を残さない。キャッシュには
	// 「キャンセル済み」エラーが無く、前回の正常な結果が保持されたままになる。
	if c.err != nil {
		t.Fatalf("キャンセルのエラーをキャッシュに書いてはなりません。得られた値: %v", c.err)
	}
	if c.rel == nil || c.rel.TagName != "v0.3.8" {
		t.Fatalf("キャッシュは前回の正常な結果を保持している必要があります。得られた値: %+v", c.rel)
	}

	// あのキャンセルは新しいデータを何ももたらさなかったので、次の訪問者は当然
	// 再取得すべきであり、しかも前回のキャンセルに巻き込まれず正常に結果を得られる。
	c.fetch = func(context.Context, *http.Client) (*selfupdate.Release, error) {
		return good, nil
	}
	rel, err := c.get(t.Context(), nil, false)
	if err != nil {
		t.Fatalf("キャンセル後の正常なリクエストはエラーになってはなりません: %v", err)
	}
	if rel == nil || rel.TagName != "v0.3.8" {
		t.Fatalf("正常な結果が得られるはずです。得られた値: %+v", rel)
	}
}
