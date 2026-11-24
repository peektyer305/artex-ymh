// doctor.go。`artex doctor` デプロイ事前チェックサブコマンド（フェーズ 6 デプロイチェーン強化）。
// 読み取り専用チェック：サービスを起動せず、DB にも書き込まない。data ディレクトリの書き込み可否は一時ファイルで確認する（都度削除）。
// 各項目は OK/WARN/FAIL/SKIP + 1 行の説明を出力し、最後に集計する。FAIL があれば終了コード 1。
package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/Autumn-27/artex/config"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/tools"
)

// doctorStatus は個別の事前チェック結果。SKIP は前提条件を満たさない場合に使う（例: DB に到達できず
// LLM profile を確認できない）。
type doctorStatus int

const (
	docOK doctorStatus = iota
	docWarn
	docFail
	docSkip
)

func (s doctorStatus) String() string {
	switch s {
	case docOK:
		return "OK  "
	case docWarn:
		return "WARN"
	case docFail:
		return "FAIL"
	case docSkip:
		return "SKIP"
	}
	return "?"
}

type doctorItem struct {
	status doctorStatus
	name   string
	note   string
}

// runDoctor はすべての事前チェックを実行してレポートを出力する。プロセスの終了コードを返す（FAIL あり → 1）。
func runDoctor(args []string) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:8787", "HTTP listen address（ゲート判定用、メインプログラムの -addr と同じ）")
	dataDir := fs.String("data", filepath.Join(config.BaseDir(), "data"), "data directory（メインプログラムの -data と同じ）")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	fmt.Printf("ARTEX doctor。デプロイ事前チェック（読み取り専用、サービスを起動せず、DB にも書き込まない）\n\n")
	var items []doctorItem
	check := func(status doctorStatus, name, note string) {
		items = append(items, doctorItem{status, name, note})
		fmt.Printf("[%s] %s: %s\n", status, name, note)
	}

	// 1. PostgreSQL に接続可能（既存の設定解析を再利用。軽量な ping で、Open のマイグレーション/seed は実行しない）
	dsn, source, dsnErr := db.DSN()
	profiles, dbOK := -1, false
	if dsnErr != nil {
		check(docFail, "PostgreSQL", dsnErr.Error())
	} else if n, err := db.Probe(dsn); err != nil {
		check(docFail, "PostgreSQL", fmt.Sprintf("接続失敗（ソース: %s）: %v", source, err))
	} else {
		dbOK, profiles = true, n
		check(docOK, "PostgreSQL", fmt.Sprintf("接続可能（ソース: %s、%s）", source, config.Redact(dsn)))
	}

	// 2. LLM profile が設定済み（DB に到達できない場合は SKIP）
	if !dbOK {
		check(docSkip, "LLM profile", "DB に到達できません。スキップします（先に UI または llm_profiles で設定してください）")
	} else if profiles < 0 {
		check(docWarn, "LLM profile", "DB が未初期化です（llm_profiles テーブルが存在しません）。起動後に UI で設定してください")
	} else if profiles == 0 {
		check(docWarn, "LLM profile", "LLM profile が 1 つも設定されていないため、探索を実行できません。UI の「LLM」ページで追加してください")
	} else {
		check(docOK, "LLM profile", fmt.Sprintf("%d 件設定済み", profiles))
	}

	// 3. data ディレクトリが書き込み可能（一時ファイルで確認し、都度削除）
	if st, err := os.Stat(*dataDir); err != nil {
		check(docWarn, "data ディレクトリ", fmt.Sprintf("%s は存在しません（起動時に自動作成されます）", *dataDir))
	} else if !st.IsDir() {
		check(docFail, "data ディレクトリ", fmt.Sprintf("%s はディレクトリではありません", *dataDir))
	} else if f, err := os.CreateTemp(*dataDir, ".doctor-*"); err != nil {
		check(docFail, "data ディレクトリ", fmt.Sprintf("%s に書き込めません: %v", *dataDir, err))
	} else {
		_ = f.Close()
		_ = os.Remove(f.Name())
		check(docOK, "data ディレクトリ", *dataDir+" は書き込み可能")
	}

	// 4/5. 外部ツール一覧：トンネルツールは個別に、武器庫は存在状況をまとめて確認
	manifest, mErr := tools.Load(tools.ManifestPath())
	if mErr != nil && !os.IsNotExist(mErr) {
		check(docWarn, "ツール一覧", "解析に失敗しました（ツールチェックをスキップ）: "+mErr.Error())
	}
	var results []tools.CheckResult
	if manifest != nil {
		results = tools.Check(manifest, *dataDir)
	} else if os.IsNotExist(mErr) {
		// 一覧が欠落している場合（開発時/最小構成デプロイ）:トンネルツールは実行時と同じ解決順（env > data/tools）でフォールバックして存在確認する。
		for _, name := range []string{"suo5", "chisel"} {
			env := "ARTEX_" + strings.ToUpper(name) + "_PATH"
			results = append(results, tools.CheckEntry(tools.Entry{
				Name: name, Path: "tools/" + name, Env: env, Category: "tunnel",
			}, *dataDir))
		}
		results = append(results, tools.CheckEntry(tools.Entry{
			Name: "suo5-payloads", Path: "tools/suo5-payloads", Env: "ARTEX_SUO5_PAYLOADS_DIR", Category: "tunnel",
		}, *dataDir))
		check(docWarn, "ツール一覧", "packaging/tools-manifest.json が欠落しています。トンネルツールは既定パスで確認し、武器庫は集計できません")
	}

	// 4. トンネルツール（suo5/chisel/payloads）：存在確認 +（一覧にハッシュが固定されている場合は）sha256
	{
		var parts []string
		worst := docOK
		for _, r := range results {
			if r.Entry.Category != "tunnel" {
				continue
			}
			parts = append(parts, doctorToolNote(r))
			if r.Status == tools.StatusMissing || r.Status == tools.StatusMismatch {
				worst = docWarn // トンネルツールの欠落はトンネル機能にのみ影響し、プラットフォームは停止しない
			}
		}
		check(worst, "トンネルツール", strings.Join(parts, ";"))
	}

	// 5. 武器庫ツールの存在状況を集計（存在 X/Y）
	{
		var present, missing []string
		for _, r := range results {
			if r.Entry.Category != "arsenal" {
				continue
			}
			if r.Status == tools.StatusMissing {
				missing = append(missing, r.Entry.Name)
			} else {
				present = append(present, r.Entry.Name)
			}
		}
		total := len(present) + len(missing)
		switch {
		case total == 0:
			check(docSkip, "武器庫ツール", "一覧が欠落しているため集計できません")
		case len(missing) == 0:
			check(docOK, "武器庫ツール", fmt.Sprintf("存在 %d/%d", len(present), total))
		default:
			check(docWarn, "武器庫ツール", fmt.Sprintf("存在 %d/%d（欠落: %s）。必要に応じて %s に揃えてください",
				len(present), total, strings.Join(missing, ", "), filepath.Join(*dataDir, "tools")))
		}
	}

	// 6. ゲート状態：リッスンアドレスが loopback でない場合に ARTEX_GATE が有効か（server.NewGate と同じ規則）
	{
		gateEnv := strings.ToLower(strings.TrimSpace(os.Getenv("ARTEX_GATE")))
		off := gateEnv == "off" || gateEnv == "0" || gateEnv == "false"
		on := gateEnv == "on" || gateEnv == "1" || gateEnv == "true"
		switch {
		case doctorLoopback(*addr):
			check(docOK, "偽装ゲート", fmt.Sprintf("リッスン %s は loopback のため、ゲートは任意（loopback でない場合は既定で有効）", *addr))
		case off:
			check(docWarn, "偽装ゲート", fmt.Sprintf("リッスン %s は loopback ではありませんが ARTEX_GATE=off です。UI/API が直接公開されるため、前段の保護を確認してください", *addr))
		case on:
			check(docOK, "偽装ゲート", fmt.Sprintf("リッスン %s は loopback ではなく、ARTEX_GATE が明示的に有効です", *addr))
		default:
			check(docOK, "偽装ゲート", fmt.Sprintf("リッスン %s は loopback ではないため、ゲートは既定で有効になります（起動ログに入口パスを出力します）", *addr))
		}
	}

	// 7. ARTEX_CALLBACK_ADDR：リバースシェル / トンネルがプラットフォームへ接続するために使用
	// 注意: systemd デプロイでは EnvironmentFile で注入するのが一般的（packaging/artex.service は
	// /etc/artex.env を想定）。doctor をシェルから直接実行するとプロセス環境を読めないため、このファイルをフォールバックとして解析する。
	cb := config.CallbackAddr()
	cbSrc := ""
	if cb == "" {
		if v, ok := readEnvFileValue("/etc/artex.env", "ARTEX_CALLBACK_ADDR"); ok {
			cb, cbSrc = v, "（/etc/artex.env より）"
		}
	}
	if cb != "" {
		check(docOK, "コールバックアドレス", "ARTEX_CALLBACK_ADDR = "+cb+cbSrc)
	} else {
		check(docWarn, "コールバックアドレス", "ARTEX_CALLBACK_ADDR が未設定です。リバースシェル / トンネル（フェーズ 3/5）のプラットフォームへのコールバックは利用できません。フットホールド（webshell）機能には影響しません")
	}

	// 集計：FAIL があれば終了コード 1
	var nOK, nWarn, nFail, nSkip int
	for _, it := range items {
		switch it.status {
		case docOK:
			nOK++
		case docWarn:
			nWarn++
		case docFail:
			nFail++
		case docSkip:
			nSkip++
		}
	}
	fmt.Printf("\n集計：OK %d · WARN %d · FAIL %d · SKIP %d\n", nOK, nWarn, nFail, nSkip)
	if nFail > 0 {
		fmt.Println("FAIL の項目があります。修正してから起動してください。")
		return 1
	}
	return 0
}

// doctorToolNote は 1 つのツールのチェック結果を描画する（doctor レポート用）。
func doctorToolNote(r tools.CheckResult) string {
	switch r.Status {
	case tools.StatusOK:
		return fmt.Sprintf("%s OK（sha256 一致、%s）", r.Entry.Name, r.Path)
	case tools.StatusUnpinned:
		return fmt.Sprintf("%s は存在します（sha256 未固定、%s）", r.Entry.Name, r.Path)
	case tools.StatusMismatch:
		return fmt.Sprintf("%s のハッシュが一致しません（期待値 %s、実際 %s。自己ビルドの可能性があるため警告のみ）", r.Entry.Name, r.Entry.SHA256, r.Actual)
	default:
		return fmt.Sprintf("%s が見つかりません（期待値 %s）", r.Entry.Name, r.Path)
	}
}

// doctorLoopback はリッスンアドレスがループバックのみにバインドされているか判定する（server.isLoopbackAddr と同じ規則。
// あちらは非公開のため、ここに小さなロジックを複製し、事前チェックのために server の公開面を変えないようにしている）。host が空（:8787）
// は全 NIC を意味し、ループバックとはみなさない。
func doctorLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	host = strings.TrimSpace(host)
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// readEnvFileValue は systemd の EnvironmentFile 形式の KEY=VALUE ファイルを解析し、指定キーの値を返す。
// ファイルが存在しないかキーが欠落している場合は ok=false。# で始まる行はコメントとして無視し、行内の
// 引用符の差異は無視する（systemd の意味論の一般的なサブセット。doctor のフォールバック表示専用で、実行時には影響しない）。
func readEnvFileValue(path, key string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(k) != key {
			continue
		}
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		return v, v != ""
	}
	return "", false
}
