// socks5.go は極めて簡素な SOCKS5 クライアント（認証なし）で、トンネル内の
// 決定的な検証にだけ使う：プラットフォームのループバック上のリバース socks 入口
// 経由で、目標側から到達できるアドレスに実際に CONNECT する。メッセージの組み立て
// と応答の解析は純関数なので単体テストできる。
package tunnel

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"
)

// BuildSocks5Greeting はハンドシェイクメッセージ [VER=5, NMETHODS=1, NOAUTH] を組み立てる。純関数。
func BuildSocks5Greeting() []byte {
	return []byte{0x05, 0x01, 0x00}
}

// BuildSocks5Request は CONNECT リクエスト（ドメイン名または IPv4）を組み立てる。純関数。
func BuildSocks5Request(host string, port int) ([]byte, error) {
	if port <= 0 || port > 65535 {
		return nil, fmt.Errorf("不正なポート %d", port)
	}
	out := []byte{0x05, 0x01, 0x00}
	if ip := net.ParseIP(host); ip != nil {
		if v4 := ip.To4(); v4 != nil {
			out = append(out, 0x01)
			out = append(out, v4...)
		} else {
			out = append(out, 0x04)
			out = append(out, ip.To16()...)
		}
	} else {
		if len(host) > 255 {
			return nil, fmt.Errorf("ドメイン名が長すぎます %d", len(host))
		}
		out = append(out, 0x03, byte(len(host)))
		out = append(out, host...)
	}
	var p [2]byte
	binary.BigEndian.PutUint16(p[:], uint16(port))
	return append(out, p[:]...), nil
}

// ParseSocks5Reply は CONNECT 応答を解析し、成功可否とステータスコードを返す。純関数。
func ParseSocks5Reply(b []byte) (bool, byte) {
	if len(b) < 2 || b[0] != 0x05 {
		return false, 0xff
	}
	return b[1] == 0x00, b[1]
}

// BuildSocks5GreetingAuth は user/pass 方式を宣言するハンドシェイクメッセージ
// [VER=5, NMETHODS=1, USERPASS=2] を組み立てる。純関数。
func BuildSocks5GreetingAuth() []byte {
	return []byte{0x05, 0x01, 0x02}
}

// BuildSocks5UserPass は RFC 1929 のユーザー名/パスワードサブネゴシエーションメッセージを組み立てる。純関数。
func BuildSocks5UserPass(user, pass string) ([]byte, error) {
	if len(user) == 0 || len(user) > 255 || len(pass) > 255 {
		return nil, fmt.Errorf("socks5 auth のユーザー名/パスワード長が不正です（1-255）")
	}
	out := []byte{0x01, byte(len(user))}
	out = append(out, user...)
	out = append(out, byte(len(pass)))
	return append(out, pass...), nil
}

// ParseSocks5UserPassReply はサブネゴシエーション応答を解析する（VER=1, STATUS=0 が成功）。純関数。
func ParseSocks5UserPassReply(b []byte) bool {
	return len(b) >= 2 && b[0] == 0x01 && b[1] == 0x00
}

// socks5DialAuth は user/pass 認証付きの socks5 入口 addr 経由で target に疎通確認する（suo5 --auth のケース）。
func socks5DialAuth(socksAddr, target, user, pass string, timeout time.Duration) error {
	host, portStr, err := net.SplitHostPort(target)
	if err != nil {
		return fmt.Errorf("検証対象 %q の解析に失敗しました： %w", target, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return fmt.Errorf("検証対象 %q のポートが不正です： %w", target, err)
	}
	conn, err := net.DialTimeout("tcp", socksAddr, timeout)
	if err != nil {
		return fmt.Errorf("socks 入口 %s に接続できません： %w", socksAddr, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(timeout))
	if _, err := conn.Write(BuildSocks5GreetingAuth()); err != nil {
		return err
	}
	buf := make([]byte, 2)
	if _, err := io.ReadFull(conn, buf); err != nil {
		return fmt.Errorf("socks ハンドシェイクの読み取りに失敗しました： %w", err)
	}
	if buf[0] != 0x05 || buf[1] != 0x02 {
		return fmt.Errorf("socks サーバーが user/pass 認証を受け付けません（method=%d）", buf[1])
	}
	auth, err := BuildSocks5UserPass(user, pass)
	if err != nil {
		return err
	}
	if _, err := conn.Write(auth); err != nil {
		return err
	}
	if _, err := io.ReadFull(conn, buf); err != nil {
		return fmt.Errorf("socks auth 応答の読み取りに失敗しました： %w", err)
	}
	if !ParseSocks5UserPassReply(buf) {
		return fmt.Errorf("socks auth が拒否されました（パスワード誤り？）")
	}
	req, err := BuildSocks5Request(host, port)
	if err != nil {
		return err
	}
	if _, err := conn.Write(req); err != nil {
		return err
	}
	reply := make([]byte, 4)
	if _, err := io.ReadFull(conn, reply); err != nil {
		return fmt.Errorf("socks 応答の読み取りに失敗しました： %w", err)
	}
	ok, code := ParseSocks5Reply(reply)
	if !ok {
		return fmt.Errorf("socks CONNECT %s が拒否されました（rep=0x%02x）", target, code)
	}
	return nil
}

// socks5Dial は socks5 入口 addr 経由で target(host:port) に疎通確認する。接続できれば決定的なエビデンス。
func socks5Dial(socksAddr, target string, timeout time.Duration) error {
	host, portStr, err := net.SplitHostPort(target)
	if err != nil {
		return fmt.Errorf("検証対象 %q の解析に失敗しました: %w", target, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return fmt.Errorf("検証対象 %q のポートが不正です: %w", target, err)
	}
	conn, err := net.DialTimeout("tcp", socksAddr, timeout)
	if err != nil {
		return fmt.Errorf("socks 入口 %s に接続できません: %w", socksAddr, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(timeout))
	if _, err := conn.Write(BuildSocks5Greeting()); err != nil {
		return err
	}
	buf := make([]byte, 2)
	if _, err := io.ReadFull(conn, buf); err != nil {
		return fmt.Errorf("socks ハンドシェイクの読み取りに失敗しました: %w", err)
	}
	if buf[0] != 0x05 || buf[1] != 0x00 {
		return fmt.Errorf("socks ハンドシェイクが拒否されました（method=%d）", buf[1])
	}
	req, err := BuildSocks5Request(host, port)
	if err != nil {
		return err
	}
	if _, err := conn.Write(req); err != nil {
		return err
	}
	reply := make([]byte, 4)
	if _, err := io.ReadFull(conn, reply); err != nil {
		return fmt.Errorf("socks 応答の読み取りに失敗しました: %w", err)
	}
	ok, code := ParseSocks5Reply(reply)
	if !ok {
		return fmt.Errorf("socks CONNECT %s が拒否されました（rep=0x%02x）", target, code)
	}
	return nil
}
