// Package agent 提供最小化的多轮对话循环。
//
// 解耦设计：Agent 核心逻辑只依赖本包定义的 Turn 与 Completer 端口，
// 完全不感知 provider 的 DTO（ChatRequest/Message 等）。与 provider 的
// 翻译收敛在 adapter.go 中（唯一 import provider 的文件），因此换后端、
// 做 mock 测试都不需要改动循环本身。
package agent

import (
	"context"
)

// Turn 一轮对话消息（领域模型，独立于任何后端）。
type Turn struct {
	// Role 取值 user / assistant / system。
	Role string
	// Content 消息正文。
	Content string
}

// Completer 是 agent 依赖的最小能力端口。
// 遵循 Go "accept interfaces, return structs" 惯例：接口定义在使用方。
type Completer interface {
	// Complete 给定完整对话历史，返回助手回复结果。
	Complete(ctx context.Context, turns []Turn) (Result, error)
}

// Result 一轮对话的输出，将回复正文与思考过程打包，便于调用方分别展示。
type Result struct {
	// Reply 回复正文。
	Reply string
	// Thinking 思考过程（reasoning_content），可能为空。
	Thinking string
}

// 编译期断言：确保适配器在编译期满足端口（实现见 adapter.go）。
var _ Completer = (*providerChat)(nil)

// Agent 持有对话历史并驱动多轮循环：每调用一次 Run 完成一轮 user→assistant。
type Agent struct {
	chat    Completer
	system  string
	history []Turn
}

// Option 以函数式选项配置 Agent。
type Option func(*Agent)

// WithSystemPrompt 设置初始系统提示，作为每次请求历史的首条消息。
func WithSystemPrompt(p string) Option {
	return func(a *Agent) { a.system = p }
}

// New 构造 Agent。chat 以构造注入传入，便于测试时替换为 mock。
func New(chat Completer, opts ...Option) *Agent {
	a := &Agent{chat: chat}
	for _, o := range opts {
		o(a)
	}
	return a
}

// Run 推进一轮对话：
//  1. 将用户输入追加进历史；
//  2. 把完整历史（含系统提示）交给 Completer 得到结果；
//  3. 将助手回复正文追加进历史（思考过程不回传模型，只作当轮输出）；
//  4. 返回结果。
//
// 多轮对话即多次调用 Run，历史在调用间持续累积。
func (a *Agent) Run(ctx context.Context, userInput string) (Result, error) {
	a.history = append(a.history, Turn{Role: "user", Content: userInput})

	result, err := a.chat.Complete(ctx, a.allTurns())
	if err != nil {
		return Result{}, err
	}

	a.history = append(a.history, Turn{Role: "assistant", Content: result.Reply})
	return result, nil
}

// History 返回当前对话历史（拷贝，防止外部篡改内部状态）。
func (a *Agent) History() []Turn {
	out := make([]Turn, len(a.history))
	copy(out, a.history)
	return out
}

// Reset 清空对话历史（系统提示保留）。
func (a *Agent) Reset() {
	a.history = a.history[:0]
}

// allTurns 返回包含系统提示在内的完整历史（拷贝）。
func (a *Agent) allTurns() []Turn {
	turns := make([]Turn, 0, len(a.history)+1)
	if a.system != "" {
		turns = append(turns, Turn{Role: "system", Content: a.system})
	}
	return append(turns, a.history...)
}
