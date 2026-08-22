// Package provider 封装 DeepSeek 兼容接口的 Chat Completions 调用，
// 提供非流式（Completer）与流式（Streamer）两种能力。
//
// 依赖注入约定：New 以 *config.Config 构造注入，依赖方向为
//
//	main（组合根）→ config → provider
//
// 不产生任何反向或循环依赖。
package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"small/internal/config"
	"small/internal/provider/retry"
)

const (
	// defaultBaseURL DeepSeek 兼容接口的基址。
	defaultBaseURL = "https://api.deepseek.com"
	// chatCompletionsPath 补全接口路径。
	chatCompletionsPath = "/chat/completions"
	// defaultRequestTimeout 非流式单次请求的整请求超时。
	defaultRequestTimeout = 60 * time.Second
	// defaultIdleTimeout 流式场景下两次读取之间的空闲超时。
	defaultIdleTimeout = 30 * time.Second
)

// Completer 是最小能力：非流式补全。消费方只依赖它即可获得完整答案。
type Completer interface {
	// Complete 发起一次非流式补全，返回完整响应。
	Complete(ctx context.Context, req *ChatRequest) (*ChatResponse, error)
}

// Streamer 在 Completer 之上扩展流式能力，构成能力嵌套的"降级"关系：
// 消费方可用类型断言 (s, ok := p.(provider.Streamer)) 探测实现是否支持流式，
// 不支持时优雅降级到 Completer。
type Streamer interface {
	Completer
	// Stream 发起一次流式补全，增量内容经 cbs 回调输出。
	Stream(ctx context.Context, req *ChatRequest, cbs StreamCallbacks) error
}

// 编译期接口断言（"虚实现"）：用 nil 指针占位验证 Client 完整实现了
// 两个能力接口，签名一旦漂移在编译期即报错，而非留到运行时类型断言。
var _ Completer = (*Client)(nil)
var _ Streamer = (*Client)(nil)

// Client 是 DeepSeek 补全接口的具体实现，同时满足 Completer 与 Streamer。
type Client struct {
	cfg         *config.Config
	baseURL     string
	httpClient  *http.Client
	retryPolicy *retry.Policy
	idleTimeout time.Duration
}

// Option 以函数式选项修改 Client 的构造参数。
type Option func(*Client)

// WithBaseURL 覆盖默认接口基址（默认 https://api.deepseek.com）。
func WithBaseURL(baseURL string) Option {
	return func(c *Client) { c.baseURL = strings.TrimRight(baseURL, "/") }
}

// WithHTTPClient 注入自定义 http.Client（如测试用的 httptest.Server、自定义 Transport）。
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) { c.httpClient = hc }
}

// WithRetryPolicy 覆盖默认重试策略。
func WithRetryPolicy(p *retry.Policy) Option {
	return func(c *Client) { c.retryPolicy = p }
}

// WithRequestTimeout 设置非流式请求的整请求超时（默认 60s）。
func WithRequestTimeout(d time.Duration) Option {
	return func(c *Client) { c.httpClient.Timeout = d }
}

// WithIdleTimeout 设置流式场景的每读空闲超时（默认 30s）。
func WithIdleTimeout(d time.Duration) Option {
	return func(c *Client) { c.idleTimeout = d }
}

// New 构造一个 Client。cfg 以构造注入传入，保证依赖可见、可测。
func New(cfg *config.Config, opts ...Option) *Client {
	c := &Client{
		cfg:         cfg,
		baseURL:     defaultBaseURL,
		httpClient:  &http.Client{Timeout: defaultRequestTimeout},
		retryPolicy: retry.Default(),
		idleTimeout: defaultIdleTimeout,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// cloneClientWithoutTimeout 复制 http.Client 的可携带字段并将 Timeout 置 0。
// 流式场景刻意去掉整请求超时，避免长生成被误杀。
func cloneClientWithoutTimeout(c *http.Client) *http.Client {
	return &http.Client{
		Transport:     c.Transport,
		CheckRedirect: c.CheckRedirect,
		Jar:           c.Jar,
		Timeout:       0,
	}
}

// Complete 实现 Completer。重试覆盖整个非流式请求（建连 + 读完整响应体）。
func (c *Client) Complete(ctx context.Context, req *ChatRequest) (*ChatResponse, error) {
	body, err := json.Marshal(chatPayload{ChatRequest: *req, Stream: false})
	if err != nil {
		return nil, fmt.Errorf("provider: marshal request: %w", err)
	}

	resp, err := c.retryPolicy.Do(ctx, func() (*http.Response, error) {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+chatCompletionsPath, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		c.setHeaders(httpReq)
		return c.httpClient.Do(httpReq)
	})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, parseAPIError(resp)
	}

	var out ChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("provider: decode response: %w", err)
	}
	return &out, nil
}

func (c *Client) setHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
}

// parseAPIError 从非 2xx 响应中提取错误信息。
func parseAPIError(resp *http.Response) error {
	// OpenAI 兼容错误格式：{"error": {"message": "...", "type": "...", "code": ...}}
	var body struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	if err := json.Unmarshal(raw, &body); err != nil || body.Error.Message == "" {
		return &APIError{Status: resp.StatusCode, Message: strings.TrimSpace(string(raw))}
	}
	return &APIError{Status: resp.StatusCode, Message: body.Error.Message}
}
