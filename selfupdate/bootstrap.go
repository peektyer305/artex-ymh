package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"
)

// smokeEnv はスモークテストが起動した子プロセスに Bootstrap を直接スキップさせる。
//
// 厳密には付けなくても問題は起きない：子プロセスの os.Executable() は artex.new で、導出される
// パスはすべて .new 付きになるため、実際のアップグレードファイルには触れない。ただしこうした偶然に
// 頼るのは脆すぎるので、明示的にショートカットすれば一目で分かり、子プロセスの無駄なディスク探索も省ける。
const smokeEnv = "ARTEX_SELFUPDATE_SMOKE"

// Action は Bootstrap が main に返す指示。
type Action int

const (
	// Continue：通常どおり server を起動する。
	Continue Action = iota
	// Restart：直ちに ExitRestart で終了し、監視スクリプトに再起動させる。
	Restart
)

// State は今回の起動時のアップグレード状態を表し、/api/update/check が
// 「前回のアップグレードが成功したのかロールバックされたのか」をフロントエンドへ正確に伝えるために使う。
type State struct {
	Pending     bool   // 差し替え後まだ安定を確認していない
	RolledBack  bool   // 今回の起動で自動ロールバックを実行した直後
	FailedStage bool   // 一時保存ファイルの検証/スモークテストに通らず、破棄済み
	Detail      string // ユーザー向けの一文の説明
}

// Bootstrap は main の冒頭で実行し、ポートの待ち受けやデータベースのオープンより前に呼ぶ必要がある。
//
// 3 つの状況：
//
//	① 一時保存ファイル artex.new がある → 検証 + スモークテスト、通れば差し替えて再起動を要求；通らなければ破棄して旧版を動かす
//	② マーカーファイルだけが残っている → 差し替え直後と分かるので試行を 1 回加算；失敗が続けばロールバック
//	③ 何もない → 通常起動
func Bootstrap() (Action, State) {
	if os.Getenv(smokeEnv) != "" {
		return Continue, State{}
	}
	p, err := ResolvePaths()
	if err != nil {
		log.Printf("[update] ブートストラップをスキップします：%v", err)
		return Continue, State{}
	}

	if _, err := os.Stat(p.New); err == nil {
		return applyStaged(p)
	}

	m, ok := readMarker(p.Marker)
	if !ok {
		return Continue, State{}
	}
	return confirmOrRollback(p, m)
}

// applyStaged は「一時保存ファイルがある」状況を処理する：検証に通れば差し替え、失敗すれば破棄する。
//
// ここはアップグレード経路で唯一実行ファイルを上書きする場所であり、最後の関門でもある。スモークテストが
// ダウンロードの破損、アーキテクチャの誤り、動的リンクの欠落といった問題を弾く。動かないバイナリを
// 一度通してしまうと、監視スクリプトは飽きもせず起動を繰り返し、Go コードは動く機会がなく、自動ロールバックもできない。
func applyStaged(p Paths) (Action, State) {
	m, _ := readMarker(p.Marker)

	if err := verifyStaged(p); err != nil {
		log.Printf("[update] 一時保存した新バージョンが検証に通りませんでした。破棄して現在のバージョンで続行します：%v", err)
		cleanStaged(p)
		_ = os.Remove(p.Marker)
		return Continue, State{FailedStage: true, Detail: "新バージョンの検証に失敗したため破棄しました：" + err.Error()}
	}

	if err := swap(p); err != nil {
		log.Printf("[update] 差し替えに失敗しました。現在のバージョンで続行します：%v", err)
		cleanStaged(p)
		_ = os.Remove(p.Marker)
		return Continue, State{FailedStage: true, Detail: "差し替えに失敗しました：" + err.Error()}
	}

	// 差し替え成功。マーカーは残し、次回起動（動くのは新版）に安定性の確認を委ねる。
	m.Attempts = 0
	if m.StagedAt == 0 {
		m.StagedAt = time.Now().Unix()
	}
	if err := writeMarker(p.Marker, m); err != nil {
		log.Printf("[update] アップグレードマーカーの書き込みに失敗しました（自動ロールバックが使えません）：%v", err)
	}
	log.Printf("[update] %s に差し替えました。再起動のため終了します（exit %d）", orUnknown(m.To), ExitRestart)
	return Restart, State{Pending: true}
}

// confirmOrRollback は「差し替え後の起動」を処理する：試行回数を加算し、上限を超えたら旧版に戻す。
//
// カウントは Go コードが動き出してからしか増えないため、対象は「実行はできるが初期化で落ちる」
// （設定の非互換、ポートの占有、DB マイグレーションの失敗）といった障害；「exec すらできない」場合は
// 差し替え前のスモークテストが弾く。両者を合わせて初めて完全になる。
func confirmOrRollback(p Paths, m marker) (Action, State) {
	m.Attempts++
	if m.Attempts > maxAttempts {
		if err := rollback(p); err != nil {
			// ロールバックまで失敗したら再起動はしない。さもないと無限再起動に陥る。マーカーを消し、
			// 現在の状態で起動させる。起動できなければ、少なくともログから原因が分かる。
			log.Printf("[update] 新バージョンが %d 回連続で起動に失敗し、ロールバックも失敗しました：%v", maxAttempts, err)
			_ = os.Remove(p.Marker)
			return Continue, State{Detail: "新バージョンの起動に失敗し、ロールバックも失敗しました：" + err.Error()}
		}
		log.Printf("[update] 新バージョンが %d 回連続で起動に失敗しました。%s にロールバックし、再起動のため終了します（exit %d）",
			maxAttempts, orUnknown(m.From), ExitRestart)
		_ = os.Remove(p.Marker)
		return Restart, State{RolledBack: true, Detail: fmt.Sprintf("新バージョンの起動に失敗したため、%s にロールバックしました", orUnknown(m.From))}
	}
	if err := writeMarker(p.Marker, m); err != nil {
		log.Printf("[update] アップグレードマーカーの更新に失敗しました：%v", err)
	}
	log.Printf("[update] 新バージョン起動中（%d/%d 回目の試行）。安定して動作すればアップグレードを確定します",
		m.Attempts, maxAttempts)
	return Continue, State{Pending: true}
}

// Settle は新バージョンが安定して動作していることを確認し、アップグレードマーカーを消す。
//
// main が HTTP の待ち受けを開始した後に遅延して呼ぶ：この時間を生き延びて初めて確定し、そうでなければ
// マーカーはそのまま残り、次回起動で試行回数の加算が続き、やがてロールバックが起こる。
func Settle() {
	p, err := ResolvePaths()
	if err != nil {
		return
	}
	settle(p)
}

func settle(p Paths) {
	if _, ok := readMarker(p.Marker); !ok {
		return // アップグレード後の起動ではないので、何もしない
	}
	if err := os.Remove(p.Marker); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("[update] アップグレードマーカーの削除に失敗しました：%v", err)
		return
	}
	log.Printf("[update] 新バージョンは安定して動作しています。アップグレード完了（前のバージョンは %s に保持）", p.Old)
}

// SettleDelay は「新バージョンが生き延びた」と判定するのに必要な稼働時間。
const SettleDelay = 30 * time.Second

// verifyStaged は一時保存ファイルを検証する：まず SHA256 を照合し、その後で実際に起動して 1 回動かす。
func verifyStaged(p Paths) error {
	want, err := os.ReadFile(p.Sum)
	if err != nil {
		return fmt.Errorf("チェックサムの読み込みに失敗しました: %w", err)
	}
	got, err := fileSHA256(p.New)
	if err != nil {
		return fmt.Errorf("チェックサムの計算に失敗しました: %w", err)
	}
	if !strings.EqualFold(strings.TrimSpace(string(want)), got) {
		return errors.New("SHA256 が一致しません（ダウンロードの破損または改ざん）")
	}
	return smokeTest(p.New)
}

// smokeTest は -h で新しいバイナリを起動し、現在のシステムで本当に実行できるか確認する。
// これによりダウンロードの切断、アーキテクチャの誤り（exec format error）、依存関係の欠落といった問題をまとめて弾ける。
func smokeTest(bin string) error {
	if err := os.Chmod(bin, 0o755); err != nil {
		return fmt.Errorf("実行権限の付与に失敗しました: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, "-h")
	cmd.Env = append(os.Environ(), smokeEnv+"=1")
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return errors.New("スモークテストがタイムアウトしました（新しいバイナリが応答しません）")
	}
	if err != nil {
		snippet := strings.TrimSpace(string(out))
		if len(snippet) > 300 {
			snippet = snippet[:300] + "…"
		}
		return fmt.Errorf("スモークテストに失敗しました: %v: %s", err, snippet)
	}
	return nil
}

// swap は現在のバイナリを一時保存した新バージョンに置き換える。
//
// Unix も Windows も実行中の実行ファイルの rename を許す（Windows が禁じるのは削除と
// 上書きで、rename は含まれない）。そのためここではプラットフォーム分岐も、自分を先に止める必要もない。
func swap(p Paths) error {
	// Windows の rename は既存の対象を上書きしないため、前回のアップグレードで残った .old を先に消す必要がある。
	if err := os.Remove(p.Old); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("古いバックアップ %s の削除に失敗しました: %w", p.Old, err)
	}
	if err := os.Rename(p.Current, p.Old); err != nil {
		return fmt.Errorf("現在のバージョンのバックアップに失敗しました: %w", err)
	}
	if err := os.Rename(p.New, p.Current); err != nil {
		// 差し替えには失敗したが現在のバージョンは既に移しているので、必ず元に戻す。さもないと次回起動時に実行ファイルがない。
		if rerr := os.Rename(p.Old, p.Current); rerr != nil {
			return fmt.Errorf("新バージョンの配置に失敗し(%v)、現在のバージョンの復元にも失敗しました: %w", err, rerr)
		}
		return fmt.Errorf("新バージョンの配置に失敗しました: %w", err)
	}
	_ = os.Remove(p.Sum)
	return nil
}

// rollback は swap がバックアップした旧バージョンに戻す。
func rollback(p Paths) error {
	if _, err := os.Stat(p.Old); err != nil {
		return fmt.Errorf("ロールバックできるバックアップ %s がありません: %w", p.Old, err)
	}
	// 起動しない新版は直接削除せず、.failed に移して調査用に残す。
	failed := p.Current + ".failed"
	_ = os.Remove(failed)
	if err := os.Rename(p.Current, failed); err != nil {
		return fmt.Errorf("失敗したバージョンの退避に失敗しました: %w", err)
	}
	if err := os.Rename(p.Old, p.Current); err != nil {
		return fmt.Errorf("旧バージョンの復元に失敗しました: %w", err)
	}
	return nil
}

// Rollback は /api/update/rollback の実装：能動的に前のバージョンへ戻す。
// 差し替えのみを行い、再起動は同じく監視スクリプトに任せる（呼び出し側はその後 ExitRestart で終了する）。
func Rollback() error {
	p, err := ResolvePaths()
	if err != nil {
		return err
	}
	if _, err := os.Stat(p.Old); err != nil {
		return errors.New("ロールバックできる前のバージョンがありません（" + p.Old + " が存在しません）")
	}
	cleanStaged(p)
	if err := smokeTest(p.Old); err != nil {
		return fmt.Errorf("前のバージョンが実行できないため、ロールバックを拒否します: %w", err)
	}
	// 現在とバックアップを交換する：ロールバック後に再び戻せるようにする。
	tmp := p.Current + ".swap"
	_ = os.Remove(tmp)
	if err := os.Rename(p.Current, tmp); err != nil {
		return fmt.Errorf("現在のバージョンの退避に失敗しました: %w", err)
	}
	if err := os.Rename(p.Old, p.Current); err != nil {
		_ = os.Rename(tmp, p.Current)
		return fmt.Errorf("前のバージョンの配置に失敗しました: %w", err)
	}
	if err := os.Rename(tmp, p.Old); err != nil {
		log.Printf("[update] ロールバック後のバックアップ整理に失敗しました（動作には影響しません）：%v", err)
	}
	_ = os.Remove(p.Marker)
	return nil
}

// HasBackup はロールバックできる前のバージョンがあるかを報告し、フロントエンドがロールバックボタンを出すか決めるために使う。
func HasBackup() bool {
	p, err := ResolvePaths()
	if err != nil {
		return false
	}
	_, err = os.Stat(p.Old)
	return err == nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func orUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return "不明なバージョン"
	}
	return s
}
