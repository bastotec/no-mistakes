package config

import (
	"testing"
	"time"
)

func TestLoadGlobal_PushQuietWindow(t *testing.T) {
	for _, tc := range []struct {
		name string
		yaml string
		want time.Duration
	}{
		{"default", "log_level: info\n", DefaultPushQuietWindow},
		{"configured", "push_quiet_window: \"30s\"\n", 30 * time.Second},
		{"disabled", "push_quiet_window: \"0s\"\n", 0},
		{"negative disables", "push_quiet_window: \"-5s\"\n", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := LoadGlobalFromBytes([]byte(tc.yaml))
			if err != nil {
				t.Fatal(err)
			}
			if cfg.PushQuietWindow != tc.want {
				t.Fatalf("PushQuietWindow = %s, want %s", cfg.PushQuietWindow, tc.want)
			}
		})
	}
	if _, err := LoadGlobalFromBytes([]byte("push_quiet_window: soon\n")); err == nil {
		t.Fatal("an unparseable push_quiet_window must fail the config")
	}
}

func TestDefaultConfigYAML_PushQuietWindowMatchesDefault(t *testing.T) {
	cfg, err := LoadGlobalFromBytes([]byte(defaultConfigYAML))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PushQuietWindow != DefaultPushQuietWindow {
		t.Fatalf("template push_quiet_window = %s, want %s", cfg.PushQuietWindow, DefaultPushQuietWindow)
	}
}
