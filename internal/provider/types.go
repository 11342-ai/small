package provider

import "fmt"

// ChatRequest 一次补全请求的参数，字段与 DeepSeek 兼容接口的
// POST /chat/completions 请求体一一对应。
type ChatRequest struct {
	Model           string          `json:"model"`
	Messages        []Message       `json:"messages"`
	Thinking        *Thinking       `json:"thinking,omitempty"`
	ReasoningEffort string          `json:"reasoning_effort,omitempty"`
	MaxTokens       int             `json:"max_tokens,omitempty"`
	ResponseFormat  *ResponseFormat `json:"response_format,omitempty"`
	Stop            []string        `json:"stop,omitempty"`
	Temperature     *float64        `json:"temperature,omitempty"`
	TopP            *float64        `json:"top_p,omitempty"`
	Logprobs        bool            `json:"logprobs,omitempty"`
	TopLogprobs     int             `json:"top_logprobs,omitempty"`
}

// Message 一条对话消息。
type Message struct {
	Role             string `json:"role"`
	Content          string `json:"content"`
	ReasoningContent string `json:"reasoning_content,omitempty"`
}

// Thinking 控制深度思考模式（deepseek-v4 等模型支持）。
type Thinking struct {
	Type string `json:"type"` // enabled / disabled
}

// ThinkingEnabled 返回开启深度思考的配置。
func ThinkingEnabled() *Thinking { return &Thinking{Type: "enabled"} }

// ThinkingDisabled 返回关闭深度思考的配置。
func ThinkingDisabled() *Thinking { return &Thinking{Type: "disabled"} }

// ResponseFormat 约束响应格式。
type ResponseFormat struct {
	Type string `json:"type"` // text / json_object
}

// ChatResponse 非流式补全的完整响应。
type ChatResponse struct {
	ID                string   `json:"id"`
	Object            string   `json:"object"`
	Created           int64    `json:"created"`
	Model             string   `json:"model"`
	Choices           []Choice `json:"choices"`
	Usage             *Usage   `json:"usage,omitempty"`
	SystemFingerprint string   `json:"system_fingerprint,omitempty"`
}

// Choice 一个候选答案。
type Choice struct {
	Index        int     `json:"index"`
	Message      Message `json:"message"`
	FinishReason string  `json:"finish_reason"`
}

// Usage token 用量统计。
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// APIError 服务端返回的带状态码的错误。
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("deepseek api: status=%d message=%q", e.Status, e.Message)
	}
	return fmt.Sprintf("deepseek api: status=%d", e.Status)
}

// chatPayload 是实际发送到 /chat/completions 的请求体。
// ChatRequest 保持纯净（不含 stream），发送时在此补上 stream 开关。
type chatPayload struct {
	ChatRequest
	Stream bool `json:"stream"`
}
