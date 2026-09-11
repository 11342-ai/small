package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withIsolatedConfig 把 HOME 指到临时目录，让写死的 ~/.small/config.yml 落在临时目录，
// 避免测试受真实用户配置干扰（配置文件路径已写死，无环境变量可覆盖）。
// Linux 下 os.UserHomeDir 读 $HOME，因此 t.Setenv 即可生效。
func withIsolatedConfig(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".small")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	return filepath.Join(dir, "config.yml")
}

func TestLoad_RequiresAPIKey(t *testing.T) {
	t.Setenv(EnvAPIKey, "")
	if _, err := Load(); err == nil {
		t.Fatal("want error when API key missing")
	}
}

// TestLoad_DefaultsWhenOnlyAPIKey 无配置文件（文件缺项/缺失）→ 全部落到内置默认值。
func TestLoad_DefaultsWhenOnlyAPIKey(t *testing.T) {
	withIsolatedConfig(t)
	t.Setenv(EnvAPIKey, "k")
	t.Setenv(EnvModel, "")

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
	if cfg.CacheDir != filepath.Join(home, ".small", "cache") {
		t.Errorf("cache dir = %q, want ~/.small/cache", cfg.CacheDir)
	}
	if cfg.MaxTokens != defaultMaxTokens {
		t.Errorf("max tokens = %d, want default %d", cfg.MaxTokens, defaultMaxTokens)
	}
	if cfg.MaxToolRounds != defaultMaxToolRounds {
		t.Errorf("max tool rounds = %d, want default %d", cfg.MaxToolRounds, defaultMaxToolRounds)
	}
	if cfg.GUIAddr != defaultGUIAddr {
		t.Errorf("gui addr = %q, want default %q", cfg.GUIAddr, defaultGUIAddr)
	}
}

// TestLoad_KubeContextFromFile 验证 kube_context 从 config.yml 读取（多集群切换）；
// 未配置时留空——空串的语义是"用 kubeconfig 的 current-context"，不能填默认值。
func TestLoad_KubeContextFromFile(t *testing.T) {
	t.Setenv(EnvAPIKey, "k")
	path := withIsolatedConfig(t)
	if err := os.WriteFile(path, []byte("kube_config: /tmp/kubeconfig\nkube_context: prod\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.KubeContext != "prod" {
		t.Errorf("kube context = %q, want prod", cfg.KubeContext)
	}
	if cfg.KubeConfig != "/tmp/kubeconfig" {
		t.Errorf("kube config = %q, want /tmp/kubeconfig（绝对路径不该被改写）", cfg.KubeConfig)
	}

	// 换一个干净 HOME（无配置文件）：context 留空而不是落某个默认值。
	withIsolatedConfig(t)
	cfg2, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg2.KubeContext != "" {
		t.Errorf("未配置 kube_context 时应留空（= current-context），实际 %q", cfg2.KubeContext)
	}
}

func TestLoad_MaxTokensFromFileAndZeroDisables(t *testing.T) {
	path := withIsolatedConfig(t)
	if err := os.WriteFile(path, []byte("max_tokens: 0\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv(EnvAPIKey, "k")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// 显式 0 = 禁用截断（区别于未设置时用默认值）。
	if cfg.MaxTokens != 0 {
		t.Errorf("max tokens = %d, want explicit 0 (disabled)", cfg.MaxTokens)
	}
}

// TestLoad_Priority 验证完整优先级链：DEEPSEEK_MODEL（环境变量）> config.yml > 内置默认值；
// 目录/预算只认 config.yml，环境变量不再参与。
func TestLoad_Priority(t *testing.T) {
	path := withIsolatedConfig(t)
	if err := os.WriteFile(path, []byte("model: from-file\nsession_dir: /file/dir\nmemory_dir: /file/mem\nkb_dir: /file/kb\ncache_dir: /file/cache\nmax_tokens: 500\nmax_tool_rounds: 50\ngui_addr: 0.0.0.0:9999\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv(EnvAPIKey, "k")

	// 阶段一：不设模型环境变量 → 全部以 config.yml 为准（文件 > 默认）。
	t.Setenv(EnvModel, "")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Model != "from-file" {
		t.Errorf("model = %q, want file from-file", cfg.Model)
	}
	if cfg.SessionDir != "/file/dir" || cfg.MemoryDir != "/file/mem" || cfg.KbDir != "/file/kb" || cfg.CacheDir != "/file/cache" {
		t.Errorf("dirs = %q %q %q %q, want file values", cfg.SessionDir, cfg.MemoryDir, cfg.KbDir, cfg.CacheDir)
	}
	if cfg.MaxTokens != 500 {
		t.Errorf("max tokens = %d, want file 500", cfg.MaxTokens)
	}
	if cfg.MaxToolRounds != 50 {
		t.Errorf("max tool rounds = %d, want file 50", cfg.MaxToolRounds)
	}
	if cfg.GUIAddr != "0.0.0.0:9999" {
		t.Errorf("gui addr = %q, want file 0.0.0.0:9999", cfg.GUIAddr)
	}

	// 阶段二：设置模型环境变量 → 仅模型被覆盖（环境变量 > 文件），目录/预算不变。
	t.Setenv(EnvModel, "from-env")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Model != "from-env" {
		t.Errorf("model = %q, want env from-env", cfg.Model)
	}
	if cfg.SessionDir != "/file/dir" || cfg.MemoryDir != "/file/mem" || cfg.KbDir != "/file/kb" || cfg.CacheDir != "/file/cache" {
		t.Errorf("dirs = %q %q %q %q, want file values", cfg.SessionDir, cfg.MemoryDir, cfg.KbDir, cfg.CacheDir)
	}
	if cfg.MaxTokens != 500 {
		t.Errorf("max tokens = %d, want file 500", cfg.MaxTokens)
	}
	if cfg.MaxToolRounds != 50 {
		t.Errorf("max tool rounds = %d, want file 50", cfg.MaxToolRounds)
	}
	if cfg.GUIAddr != "0.0.0.0:9999" {
		t.Errorf("gui addr = %q, want file 0.0.0.0:9999", cfg.GUIAddr)
	}
}

// TestLoad_MaxToolRoundsZeroUnlimited 显式 0 = 不限（区别于未设置用默认值）。
func TestLoad_MaxToolRoundsZeroUnlimited(t *testing.T) {
	path := withIsolatedConfig(t)
	if err := os.WriteFile(path, []byte("max_tool_rounds: 0\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv(EnvAPIKey, "k")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MaxToolRounds != 0 {
		t.Errorf("max tool rounds = %d, want explicit 0 (unlimited)", cfg.MaxToolRounds)
	}
}

func TestLoad_BrokenConfigFileErrors(t *testing.T) {
	path := withIsolatedConfig(t)
	if err := os.WriteFile(path, []byte("a: [unclosed\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv(EnvAPIKey, "k")
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
