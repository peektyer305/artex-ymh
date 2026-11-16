package agent

import (
	"testing"

	"github.com/Autumn-27/artex/honeydetect"
)

func TestHoneypotConfidenceMapping(t *testing.T) {
	// 処置段階の対応: score≥0.9 high、≥0.6 medium。
	cases := map[float64]string{0.99: "high", 0.9: "high", 0.85: "medium", 0.6: "medium"}
	for score, want := range cases {
		if got := honeypotConfidence(score); got != want {
			t.Errorf("honeypotConfidence(%v)=%q want %q", score, got, want)
		}
	}
}

func TestURLPort(t *testing.T) {
	cases := map[string]int{
		"http://a.example":        80,
		"https://a.example/path":  443,
		"http://a.example:8080/x": 8080,
		"not a url ::":            0,
	}
	for raw, want := range cases {
		if got := urlPort(raw); got != want {
			t.Errorf("urlPort(%q)=%d want %d", raw, got, want)
		}
	}
}

func TestEvaluateHoneypotNilStoresNoPanic(t *testing.T) {
	// store を持たない ToolSet（toolcatalog の seed 構築など）がフックを呼んでも panic せず、何もしない。
	ts := &ToolSet{}
	ts.evaluateHoneypot(0, honeydetect.ServiceView{Port: 22})
	ts.evaluateHoneypot(1, honeydetect.ServiceView{Port: 22, Banner: "SSH-2.0-OpenSSH_6.0p1 Debian-4+deb7u2"})
}
