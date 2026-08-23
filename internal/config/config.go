// Package config 集中管理客户端所需的全部配置，
// 避免环境变量与配置文件散落在各处的函数调用里。
// 读取只发生在 Load() 一处，随后以 struct 整体注入下游（组合根红线）。
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	// EnvModel 模型名环境变量。
	EnvModel = "DEEPSEEK_MODEL"
	// EnvAPIKey API Key 环境变量。
	EnvAPIKey = "DEEPSEEK_API_KEY"
	// EnvSessionDir 会话存储目录环境变量。
	EnvSessionDir = "SMALL_SESSION_DIR"
	// EnvConfigFile 配置文件路径覆盖（缺省 ~/.small/config.yml）。
	EnvConfigFile = "SMALL_CONFIG"
	// EnvMaxTokens 上下文预算（估算 token，字符近似）环境变量。
	EnvMaxTokens = "SMALL_MAX_TOKENS"
	// defaultModel 未设置时的默认模型。
	defaultModel = "deepseek-v4-pro"
	// defaultConfigFile 缺省配置文件路径。
	defaultConfigFile = "~/.small/config.yml"
	// defaultSessionDir 缺省会话存储目录。
	defaultSessionDir = "~/.small/sessions"
	// defaultMaxTokens 缺省上下文预算：约 8k token，远低于 64k 上下文，
	// 预留余量防溢出；本地对话足够。
	defaultMaxTokens = 8000
)

// Config 是客户端的集中配置，构造后整体向下游注入。
type Config struct {
	// Model 使用的模型名。
	Model string
	// APIKey 鉴权密钥，必填（只从环境变量读取，不进配置文件）。
	APIKey string
	// SessionDir 会话持久化目录（session.New 用它建仓库）。
	SessionDir string
	// MaxTokens 上下文预算（agent.WithTokenBudget）；<=0 表示不启用截断。
	MaxTokens int
}

// fileConfig 配置文件的可选字段。APIKey 不在此列：机密走环境变量更安全。
// 环境变量优先于文件（显式注入覆盖持久化配置），文件优先于内置默认值。
type fileConfig struct {
	Model      string `yaml:"model"`
	SessionDir string `yaml:"session_dir"`
	// 指针区分"未设置"与"显式 0"（显式 0 = 禁用截断，未设置 = 用默认值）。
	MaxTokens *int `yaml:"max_tokens"`
}

// Load 加载配置：APIKey 必填（环境变量），其余字段按
// "环境变量 > 配置文件 > 内置默认值" 的优先级合并。
// 配置文件路径由 SMALL_CONFIG 指定，缺省 ~/.small/config.yml；
// 文件不存在则忽略（纯环境变量行为），存在但解析失败则报错（配置损坏应暴露而非静默）。
func Load() (*Config, error) {
	apiKey := os.Getenv(EnvAPIKey)
	if apiKey == "" {
		return nil, fmt.Errorf("config: environment variable %s is required", EnvAPIKey)
	}

	file, err := loadFile()
	if err != nil {
		return nil, err
	}

	model := firstNonEmpty(os.Getenv(EnvModel), file.Model, defaultModel)
	sessionDir, err := expandHome(firstNonEmpty(os.Getenv(EnvSessionDir), file.SessionDir, defaultSessionDir))
	if err != nil {
		return nil, err
	}
	maxTokens, err := resolveMaxTokens(file)
	if err != nil {
		return nil, err
	}

	return &Config{Model: model, APIKey: apiKey, SessionDir: sessionDir, MaxTokens: maxTokens}, nil
}

// resolveMaxTokens 按"环境变量 > 文件 > 默认"解析预算；环境变量非法时显式报错
// （配置错误应暴露而非静默回退）。
func resolveMaxTokens(file fileConfig) (int, error) {
	if env := os.Getenv(EnvMaxTokens); env != "" {
		n, err := strconv.Atoi(env)
		if err != nil {
			return 0, fmt.Errorf("config: %s=%q is not a number", EnvMaxTokens, env)
		}
		return n, nil
	}
	if file.MaxTokens != nil {
		return *file.MaxTokens, nil
	}
	return defaultMaxTokens, nil
}

// loadFile 读取可选配置文件；不存在时返回零值（等价于未配置）。
func loadFile() (fileConfig, error) {
	path := os.Getenv(EnvConfigFile)
	if path == "" {
		path = defaultConfigFile
	}
	expanded, err := expandHome(path)
	if err != nil {
		return fileConfig{}, err
	}
	data, err := os.ReadFile(expanded)
	if err != nil {
		if os.IsNotExist(err) {
			return fileConfig{}, nil
		}
		return fileConfig{}, fmt.Errorf("config: read %q: %w", expanded, err)
	}
	var fc fileConfig
	if err := yaml.Unmarshal(data, &fc); err != nil {
		return fileConfig{}, fmt.Errorf("config: parse %q: %w", expanded, err)
	}
	return fc, nil
}

// expandHome 展开开头的 "~" 为当前用户主目录；其余路径原样返回。
func expandHome(p string) (string, error) {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("config: resolve home dir: %w", err)
		}
		if p == "~" {
			return home, nil
		}
		return filepath.Join(home, p[2:]), nil
	}
	return p, nil
}

// firstNonEmpty 返回第一个非空值（合并优先级：环境变量 > 文件 > 默认）。
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
