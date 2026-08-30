package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withIsolatedConfig 把配置文件指到临时目录，避免测试受真实 ~/.small/config.yml 干扰。
func withIsolatedConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yml")
	t.Setenv(EnvConfigFile, path)
	return path
}

func TestLoad_RequiresAPIKey(t *testing.T) {
	t.Setenv(EnvConfigFile, filepath.Join(t.TempDir(), "none.yml"))
	t.Setenv(EnvAPIKey, "")
	if _, err := Load(); err == nil {
		t.Fatal("want error when API key missing")
	}
}

func TestLoad_DefaultsWhenOnlyAPIKey(t *testing.T) {
	withIsolatedConfig(t)
	t.Setenv(EnvAPIKey, "k")
	t.Setenv(EnvModel, "")
	t.Setenv(EnvSessionDir, "")
	t.Setenv(EnvMaxTokens, "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Model != defaultModel {
		t.Errorf("model = %q, want default %q", cfg.Model, defaultModel)
	}
	home, _ := os.UserHomeDir()
	if cfg.SessionDir != filepath.Join(home, ".small", "sessions") {
		t.Errorf("session dir = %q, want ~/.small/sessions", cfg.SessionDir)
	}
	if cfg.MemoryDir != filepath.Join(home, ".small", "memory") {
		t.Errorf("memory dir = %q, want ~/.small/memory", cfg.MemoryDir)
	}
	if cfg.KbDir != filepath.Join(home, ".small", "kb") {
		t.Errorf("kb dir = %q, want ~/.small/kb", cfg.KbDir)
	}
	if cfg.MaxTokens != defaultMaxTokens {
		t.Errorf("max tokens = %d, want default %d", cfg.MaxTokens, defaultMaxTokens)
	}
}

func TestLoad_MaxTokensEnvOverridesFile(t *testing.T) {
	path := withIsolatedConfig(t)
	if err := os.WriteFile(path, []byte("max_tokens: 500\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv(EnvAPIKey, "k")
	t.Setenv(EnvMaxTokens, "1234")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MaxTokens != 1234 {
		t.Errorf("max tokens = %d, want env 1234", cfg.MaxTokens)
	}
}

func TestLoad_MaxTokensFromFileAndZeroDisables(t *testing.T) {
	path := withIsolatedConfig(t)
	if err := os.WriteFile(path, []byte("max_tokens: 0\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv(EnvAPIKey, "k")
	t.Setenv(EnvMaxTokens, "")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// 显式 0 = 禁用截断（区别于未设置时用默认值）。
	if cfg.MaxTokens != 0 {
		t.Errorf("max tokens = %d, want explicit 0 (disabled)", cfg.MaxTokens)
	}
}

func TestLoad_MaxTokensInvalidEnvErrors(t *testing.T) {
	withIsolatedConfig(t)
	t.Setenv(EnvAPIKey, "k")
	t.Setenv(EnvMaxTokens, "abc")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "not a number") {
		t.Fatalf("want invalid-number error, got %v", err)
	}
}

func TestLoad_EnvOverridesFile(t *testing.T) {
	path := withIsolatedConfig(t)
	if err := os.WriteFile(path, []byte("model: from-file\nsession_dir: /file/dir\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv(EnvAPIKey, "k")
	t.Setenv(EnvModel, "from-env")
	t.Setenv(EnvSessionDir, "/env/dir")
	t.Setenv(EnvMemoryDir, "/env/mem")
	t.Setenv(EnvKbDir, "/env/kb")
	t.Setenv(EnvMaxTokens, "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Model != "from-env" || cfg.SessionDir != "/env/dir" {
		t.Errorf("got model=%q dir=%q, want env values", cfg.Model, cfg.SessionDir)
	}
	if cfg.MemoryDir != "/env/mem" {
		t.Errorf("memory dir = %q, want env /env/mem", cfg.MemoryDir)
	}
	if cfg.KbDir != "/env/kb" {
		t.Errorf("kb dir = %q, want env /env/kb", cfg.KbDir)
	}
}

func TestLoad_FileOverridesDefault(t *testing.T) {
	path := withIsolatedConfig(t)
	if err := os.WriteFile(path, []byte("model: from-file\nsession_dir: /file/dir\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv(EnvAPIKey, "k")
	t.Setenv(EnvModel, "")
	t.Setenv(EnvSessionDir, "")
	t.Setenv(EnvMaxTokens, "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Model != "from-file" || cfg.SessionDir != "/file/dir" {
		t.Errorf("got model=%q dir=%q, want file values", cfg.Model, cfg.SessionDir)
	}
}

func TestLoad_MissingConfigFileIgnored(t *testing.T) {
	withIsolatedConfig(t) // 指向不存在的文件
	t.Setenv(EnvAPIKey, "k")
	t.Setenv(EnvMaxTokens, "")
	if _, err := Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
}

func TestLoad_BrokenConfigFileErrors(t *testing.T) {
	path := withIsolatedConfig(t)
	if err := os.WriteFile(path, []byte("a: [unclosed\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv(EnvAPIKey, "k")
	t.Setenv(EnvMaxTokens, "")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "parse") {
		t.Fatalf("want parse error, got %v", err)
	}
}

func TestExpandHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("home: %v", err)
	}
	cases := []struct{ in, want string }{
		{"~", home},
		{"~/x/y", filepath.Join(home, "x", "y")},
		{"/abs/path", "/abs/path"},
		{"rel/path", "rel/path"},
	}
	for _, c := range cases {
		got, err := expandHome(c.in)
		if err != nil {
			t.Fatalf("expandHome(%q): %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("expandHome(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
