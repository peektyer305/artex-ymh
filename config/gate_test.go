package config

import "testing"

func TestGateEnabledOverridesAndDefaults(t *testing.T) {
	addresses := []struct {
		addr     string
		loopback bool
	}{
		{"127.0.0.1:8787", true},
		{"127.0.0.2:8787", true},
		{"[::1]:8787", true},
		{"::1", true},
		{"localhost:8787", true},
		{"LOCALHOST", true},
		{":8787", false},
		{"0.0.0.0:8787", false},
		{"[::]:8787", false},
		{"192.0.2.1:8787", false},
		{"invalid-host", false},
		{" localhost ", false},
	}
	settings := []struct {
		value    string
		explicit bool
		enabled  bool
	}{
		{"", false, false},
		{"invalid-value", false, false},
		{"on", true, true},
		{"1", true, true},
		{" TRUE ", true, true},
		{"off", true, false},
		{"0", true, false},
		{" FALSE ", true, false},
	}
	for _, setting := range settings {
		t.Setenv("ARTEX_GATE", setting.value)
		for _, address := range addresses {
			wantEnabled := setting.enabled
			if !setting.explicit {
				wantEnabled = !address.loopback
			}
			enabled, explicit := GateEnabled(address.addr)
			if enabled != wantEnabled || explicit != setting.explicit {
				t.Errorf("GateEnabled(%q), env %q = (%v, %v), want (%v, %v)",
					address.addr, setting.value, enabled, explicit, wantEnabled, setting.explicit)
			}
			if got := IsLoopbackAddr(address.addr); got != address.loopback {
				t.Errorf("IsLoopbackAddr(%q) = %v, want %v", address.addr, got, address.loopback)
			}
		}
	}
}
