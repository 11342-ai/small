// Package config 集中管理 DeepSeek 客户端所需的全部配置，
// 避免 DEEPSEEK_* 环境变量散落在各处的函数调用里。
package config

import (
	"fmt"
	"os"
)

const (
	// EnvModel 模型名环境变量。
	EnvModel = "DEEPSEEK_MODEL"
	// EnvAPIKey API Key 环境变量。
	EnvAPIKey = "DEEPSEEK_API_KEY"
	// defaultModel 未设置 EnvModel 时的默认模型。
	defaultModel = "deepseek-v4-pro"
)

// Config 是 DeepSeek 客户端的集中配置，构造后整体向下游注入。
type Config struct {
	// Model 使用的模型名，如 deepseek-v4-pro / deepseek-v4-flash / deepseek-v4-flash-vision-exp。
	Model string
	// APIKey 鉴权密钥，必填。
	APIKey string
}

// Load 从环境变量加载配置。
//
// 规则：
//   - DEEPSEEK_API_KEY 缺失时报错（无密钥无法鉴权）；
//   - DEEPSEEK_MODEL 缺失时回退到 defaultModel。
func Load() (*Config, error) {
	apiKey := os.Getenv(EnvAPIKey)
	if apiKey == "" {
		return nil, fmt.Errorf("config: environment variable %s is required", EnvAPIKey)
	}
	model := os.Getenv(EnvModel)
	if model == "" {
		model = defaultModel
	}
	return &Config{Model: model, APIKey: apiKey}, nil
}
