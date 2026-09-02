// Package config 集中管理客户端所需的全部配置，
// 避免环境变量与配置文件散落在各处的函数调用里。
// 读取只发生在 Load() 一处，随后以 struct 整体注入下游（组合根红线）。
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	// EnvModel 模型名环境变量。
	EnvModel = "DEEPSEEK_MODEL"
	// EnvAPIKey API Key 环境变量。
	EnvAPIKey = "DEEPSEEK_API_KEY"
	// defaultModel 未设置时的默认模型。
	defaultModel = "deepseek-v4-pro"
	// defaultConfigFile 配置文件路径（写死，不做环境变量覆盖；对齐微信等把路径定死的做法）。
	defaultConfigFile = "~/.small/config.yml"
	// defaultSessionDir 缺省会话存储目录。
	defaultSessionDir = "~/.small/sessions"
	// defaultMemoryDir 缺省长期记忆目录。
	defaultMemoryDir = "~/.small/memory"
	// defaultKbDir 缺省知识库目录（Zoo/model/kb.md：与 memory 并列的存储部件）。
	defaultKbDir = "~/.small/kb"
	// defaultCacheDir 缺省文档解析缓存目录（tool-lit.md：doc_parse 产物落盘处，
	// 受控目录，解析只读+写缓存 = Pass 权限）。
	defaultCacheDir = "~/.small/cache"
	// defaultMaxTokens 缺省上下文预算：32k token，正好是 64k 上下文的一半，
	// 预留一半余量防溢出（含回复输出与工具声明）；本地对话足够。
	defaultMaxTokens = 32768
	// defaultMaxToolRounds 缺省单次 Run 工具调用轮次上限（pdf-workflow.md §5）：
	// 20 轮，覆盖 PDF 全链路（parse→clean→read→整理→write，大文件分批）；
	// 显式 0 = 不限（agent 层用兜底上限防死循环）。
	defaultMaxToolRounds = 20
	// defaultGUIAddr 缺省 GUI 监听地址（gui.md §4.5：仅本机服务，配置来源单一 =
	// config.yml 优先、内置默认兜底；删 --gui-addr flag 对齐删环境变量决策）。
	defaultGUIAddr = "127.0.0.1:8090"
)

// Config 是客户端的集中配置，构造后整体向下游注入。
type Config struct {
	// Model 使用的模型名。
	Model string
	// APIKey 鉴权密钥，必填（只从环境变量读取，不进配置文件）。
	APIKey string
	// SessionDir 会话持久化目录（session.New 用它建仓库）。
	SessionDir string
	// MemoryDir 长期记忆目录（memory.New 用它建仓库）。
	MemoryDir string
	// KbDir 知识库目录（kb.New 用它建索引；md 文件树，文件夹纯归置）。
	KbDir string
	// CacheDir 文档解析缓存目录（doc_parse 产物落盘处，受控目录）。
	CacheDir string
	// MaxToolRounds 单次 Run 工具调用轮次上限（agent.WithMaxToolRounds）；<=0 表示不限。
	MaxToolRounds int
	// GUIAddr GUI 监听地址（gui.md §4.5；--gui 启动时用，仅本机服务）。
	GUIAddr string
	// MaxTokens 上下文预算（agent.WithTokenBudget）；<=0 表示不启用截断。
	MaxTokens int
}

// fileConfig 配置文件的可选字段。APIKey 不在此列：机密走环境变量更安全。
// 目录/预算等以配置文件为准（文件 > 内置默认值）；模型例外，DEEPSEEK_MODEL 仍可覆盖文件。
type fileConfig struct {
	Model      string `yaml:"model"`
	SessionDir string `yaml:"session_dir"`
	MemoryDir  string `yaml:"memory_dir"`
	KbDir      string `yaml:"kb_dir"`
	CacheDir   string `yaml:"cache_dir"`
	GUIAddr    string `yaml:"gui_addr"`
	// 指针区分"未设置"与"显式 0"（显式 0 = 不限，未设置 = 用默认值；对齐 max_tokens）。
	MaxTokens     *int `yaml:"max_tokens"`
	MaxToolRounds *int `yaml:"max_tool_rounds"`
}

// Load 加载配置：APIKey 必填（只走环境变量）；目录/预算等字段以配置文件为准，
// 文件未配置时用内置默认值（不再读环境变量）。模型名例外：DEEPSEEK_MODEL 仍可覆盖文件。
// 配置文件路径写死 ~/.small/config.yml（不做环境变量覆盖）；
// 文件不存在则忽略（纯默认值行为），存在但解析失败则报错（配置损坏应暴露而非静默）。
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
	sessionDir, err := expandHome(firstNonEmpty(file.SessionDir, defaultSessionDir))
	if err != nil {
		return nil, err
	}
	memoryDir, err := expandHome(firstNonEmpty(file.MemoryDir, defaultMemoryDir))
	if err != nil {
		return nil, err
	}
	kbDir, err := expandHome(firstNonEmpty(file.KbDir, defaultKbDir))
	if err != nil {
		return nil, err
	}
	cacheDir, err := expandHome(firstNonEmpty(file.CacheDir, defaultCacheDir))
	if err != nil {
		return nil, err
	}
	maxTokens := defaultMaxTokens
	if file.MaxTokens != nil {
		// 指针区分"未设置"与"显式 0"：显式 0 = 禁用截断。
		maxTokens = *file.MaxTokens
	}
	maxRounds := defaultMaxToolRounds
	if file.MaxToolRounds != nil {
		// 显式 0 = 不限（agent 层用兜底上限防死循环）。
		maxRounds = *file.MaxToolRounds
	}
	guiAddr := firstNonEmpty(file.GUIAddr, defaultGUIAddr)

	return &Config{Model: model, APIKey: apiKey, SessionDir: sessionDir, MemoryDir: memoryDir, KbDir: kbDir, CacheDir: cacheDir, MaxTokens: maxTokens, MaxToolRounds: maxRounds, GUIAddr: guiAddr}, nil
}

// loadFile 读取默认配置文件（路径写死 ~/.small/config.yml）；不存在时返回零值（等价于未配置）。
func loadFile() (fileConfig, error) {
	expanded, err := expandHome(defaultConfigFile)
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

// firstNonEmpty 返回第一个非空值（合并优先级：文件 > 默认；模型为 环境变量 > 文件 > 默认）。
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
