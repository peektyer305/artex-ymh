package config

import (
	"net"
	"os"
	"strings"
)

// GateEnabled uses explicit settings first, then the listen address.
func GateEnabled(addr string) (enabled, explicit bool) {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("ARTEX_GATE"))) {
	case "off", "0", "false":
		return false, true
	case "on", "1", "true":
		return true, true
	}
	return !IsLoopbackAddr(addr), false
}

// IsLoopbackAddr preserves the server's existing address classification.
func IsLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
