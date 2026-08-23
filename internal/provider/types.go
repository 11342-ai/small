package provider

import (
	"encoding/json"
	"fmt"
)

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
	Tools           []Tool          `json:"tools,omitempty"`
}

// Tool 一次请求中提供给模型的一个工具声明（OpenAI 兼容格式）。
type Tool struct {
	Type     string       `json:"type"` // function
	Function ToolFunction `json:"function"`
}

// ToolFunction 工具声明的函数部分，Parameters 即 JSON Schema。
type ToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// ToolCall 模型在响应中请求的一次工具调用。
type ToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"` // function
	Function ToolCallFunction `json:"function"`
}

// ToolCallFunction 工具调用的函数部分，Arguments 为参数 JSON 文本。
type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Message 一条对话消息。
type Message struct {
	Role             string `json:"role"`
	Content          string `json:"content"`
	ReasoningContent string `json:"reasoning_content,omitempty"`
	// ToolCalls 仅 assistant 消息携带：模型请求的工具调用列表。
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	// ToolCallID 仅 tool 消息携带：指向被执行的调用。
	ToolCallID string `json:"tool_call_id,omitempty"`
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

// StreamOptions 流式专用选项（仅随流式请求注入，非流式无意义）。
type StreamOptions struct {
	// IncludeUsage 为 true 时，服务端在流末尾返回一个携带 usage 的 chunk
	// （choices 为空）。预算校准依赖它拿到真实的 prompt_tokens 计数。
	IncludeUsage bool `json:"include_usage"`
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
// ChatRequest 保持纯净（不含 stream），发送时在此补上流式开关与流式专用选项。
type chatPayload struct {
	ChatRequest
	Stream        bool           `json:"stream"`
	StreamOptions *StreamOptions `json:"stream_options,omitempty"`
}
