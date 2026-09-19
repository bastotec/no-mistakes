package config

import (
	"strings"
	"testing"
	"time"

	jevpkg "github.com/kunchenguid/no-mistakes/internal/jev"
)

// The advisory Jev signal is global-only operator configuration: it spends
// this operator's gateway quota and names a credential reference, so these
// tests pin the parse surface an operator drives - defaults, overrides,
// validation, and the fact that no repository copy can steer it.

func TestLoadGlobal_JevDefaults(t *testing.T) {
	cfg, err := LoadGlobalFromBytes([]byte("agent: claude\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := Jev{
		Enabled:       false,
		GatewayKeyEnv: jevpkg.DefaultKeyEnv,
		SecretsFile:   jevpkg.DefaultSecretsFile,
		Model:         jevpkg.DefaultModel,
		Timeout:       jevpkg.DefaultTimeout,
		MaxDiffBytes:  DefaultJevMaxDiffBytes,
		Threshold:     DefaultJevThreshold,
	}
	if cfg.Jev != want {
		t.Fatalf("jev defaults = %+v, want %+v", cfg.Jev, want)
	}
	if DefaultJevMaxDiffBytes != jevpkg.MaxStateBytes {
		t.Fatalf("DefaultJevMaxDiffBytes = %d, want the single owner jev.MaxStateBytes = %d", DefaultJevMaxDiffBytes, jevpkg.MaxStateBytes)
	}
}

func TestLoadGlobal_JevOverrides(t *testing.T) {
	cfg, err := LoadGlobalFromBytes([]byte(`
jev:
  enabled: true
  gateway_key_env: MY_JEV_KEY
  secrets_file: /etc/jev/secrets
  model: other-org/jev-fork
  timeout: 45s
  max_diff_bytes: 4096
  threshold: 0.75
`))
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.Jev
	if !got.Enabled || got.GatewayKeyEnv != "MY_JEV_KEY" || got.SecretsFile != "/etc/jev/secrets" ||
		got.Model != "other-org/jev-fork" || got.Timeout != 45*time.Second ||
		got.MaxDiffBytes != 4096 || got.Threshold != 0.75 {
		t.Fatalf("jev overrides not applied: %+v", got)
	}
}

func TestLoadGlobal_JevInvalidValues(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want string
	}{
		{"threshold zero", "jev:\n  threshold: 0\n", "jev.threshold"},
		{"threshold one", "jev:\n  threshold: 1\n", "jev.threshold"},
		{"threshold negative", "jev:\n  threshold: -0.5\n", "jev.threshold"},
		{"empty gateway_key_env", "jev:\n  gateway_key_env: \"\"\n", "jev.gateway_key_env"},
		{"empty secrets_file", "jev:\n  secrets_file: \"\"\n", "jev.secrets_file"},
		{"empty model", "jev:\n  model: \"\"\n", "jev.model"},
		{"zero max_diff_bytes", "jev:\n  max_diff_bytes: 0\n", "jev.max_diff_bytes"},
		{"negative max_diff_bytes", "jev:\n  max_diff_bytes: -1\n", "jev.max_diff_bytes"},
		{"non-positive timeout", "jev:\n  timeout: 0s\n", "jev.timeout"},
		{"unparsable timeout", "jev:\n  timeout: soon\n", "jev.timeout"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadGlobalFromBytes([]byte(tc.yaml))
			if err == nil {
				t.Fatalf("LoadGlobalFromBytes accepted %q", tc.yaml)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not name %q", err, tc.want)
			}
		})
	}
}

// TestMerge_JevIsGlobalOnly pins the trust boundary: a repository config
// (trusted or pushed) has no jev surface at all, so a `jev:` block in a
// .no-mistakes.yaml cannot enable, disable, or resize the operator's signal.
func TestMerge_JevIsGlobalOnly(t *testing.T) {
	global, err := LoadGlobalFromBytes([]byte("agent: claude\n"))
	if err != nil {
		t.Fatal(err)
	}
	// A repository that tries to enable and steer the signal from its own
	// config: every key here must be inert because RepoConfig has no such
	// fields and Merge copies the global block straight through.
	repo, err := LoadRepoFromBytes([]byte(`
jev:
  enabled: true
  model: evil-org/steer
  gateway_key_env: LEAKED
`))
	if err != nil {
		t.Fatalf("a repo jev block should parse as unknown keys, got %v", err)
	}
	merged := Merge(global, repo)
	if merged.Jev.Enabled {
		t.Fatal("a repository config enabled the operator's jev signal")
	}
	if merged.Jev.Model != jevpkg.DefaultModel || merged.Jev.GatewayKeyEnv != jevpkg.DefaultKeyEnv {
		t.Fatalf("a repository config steered jev: %+v", merged.Jev)
	}
	if merged.Jev != global.Jev {
		t.Fatalf("Merge did not copy the global jev block verbatim: %+v vs %+v", merged.Jev, global.Jev)
	}
}
