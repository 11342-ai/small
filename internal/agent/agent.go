// Package agent 提供最小化的多轮对话循环，支持工具调用（function calling）。
//
// 解耦设计：Agent 核心逻辑只依赖本包定义的 Turn 与 Completer 端口，以及 tool
// 包的工具契约（声明/执行），完全不感知 provider 的 DTO（ChatRequest/Message 等）。
// 与 provider 的翻译收敛在 adapter.go 中（唯一 import provider 的文件），因此换后端、
// 做 mock 测试都不需要改动循环本身。
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"small/internal/tool"
)

// Turn 一轮对话消息（领域模型，独立于任何后端）。
type Turn struct {
	// Role 取值 user / assistant / system / tool。
	Role string
	// Content 消息正文。
	Content string
	// ToolCallID 仅 Role == "tool" 时有效：指向被执行的调用，供模型关联结果。
	ToolCallID string
	// ToolCalls 仅 Role == "assistant" 时有效：模型请求的工具调用列表（Content 可为空）。
	ToolCalls []ToolCall
}

// ToolCall 模型请求的一次工具调用。
type ToolCall struct {
	// ID 调用的唯一标识，结果回灌时必须原样带回。
	ID string
	// Name 工具名，Registry 的键。
	Name string
	// Args 参数 JSON 文本（来自模型，原始形态）。
	Args string
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
	// ToolCalls 模型请求的工具调用；非空表示本轮回灌前需要执行工具，
	// 由 Agent 循环负责执行并回灌结果后继续下一轮。
	ToolCalls []ToolCall
}

// 编译期断言：确保适配器在编译期满足端口（实现见 adapter.go）。
var _ Completer = (*providerChat)(nil)

// maxToolRounds 单次 Run 内工具调用的最大轮数，防御模型无限循环调用工具。
const maxToolRounds = 8

// Agent 持有对话历史并驱动多轮循环：每调用一次 Run 完成一轮 user→assistant。
type Agent struct {
	chat    Completer
	system  string
	history []Turn
	tools   *tool.Registry // 为 nil 时工具调用不可用，退化为纯对话
}

// Option 以函数式选项配置 Agent。
type Option func(*Agent)

// WithSystemPrompt 设置初始系统提示，作为每次请求历史的首条消息。
func WithSystemPrompt(p string) Option {
	return func(a *Agent) { a.system = p }
}

// New 构造 Agent。结构协作对象以显式参数注入（chat、tools），便于测试时替换 mock；
// tools 为 nil 时退化为纯对话（模型不会收到工具声明）。行为开关走 Option。
func New(chat Completer, tools *tool.Registry, opts ...Option) *Agent {
	a := &Agent{chat: chat, tools: tools}
	for _, o := range opts {
		o(a)
	}
	return a
}

// Run 推进一轮对话：
//  1. 将用户输入追加进历史；
//  2. 把完整历史（含系统提示）交给 Completer 得到结果；
//  3. 若结果请求了工具调用，则执行每个调用并把结果回灌进历史，再回到步骤 2，
//     直到模型不再请求工具（最多 maxToolRounds 轮，防死循环）；
//  4. 将最终助手回复正文追加进历史（思考过程不回传模型，只作当轮输出）；
//  5. 返回结果。
//
// 多轮对话即多次调用 Run，历史在调用间持续累积。
func (a *Agent) Run(ctx context.Context, userInput string) (Result, error) {
	a.history = append(a.history, Turn{Role: "user", Content: userInput})

	for round := 0; round < maxToolRounds; round++ {
		result, err := a.chat.Complete(ctx, a.allTurns())
		if err != nil {
			return Result{}, err
		}

		if len(result.ToolCalls) == 0 {
			a.history = append(a.history, Turn{Role: "assistant", Content: result.Reply})
			return result, nil
		}
		if a.tools == nil {
			return Result{}, errors.New("agent: model requested tools but no tools registered")
		}

		// 把模型请求的调用原样保留进历史（供 provider 关联 tool 结果），再执行回灌。
		a.history = append(a.history, Turn{Role: "assistant", Content: result.Reply, ToolCalls: result.ToolCalls})
		for _, call := range result.ToolCalls {
			t, ok := a.tools.Get(call.Name)
			if !ok {
				// 模型请求了未注册的工具：按"业务失败"回灌，让模型自行修正，而非中止循环。
				a.history = append(a.history, Turn{
					Role: "tool", ToolCallID: call.ID, Content: "未注册的工具: " + call.Name,
				})
				continue
			}
			res, err := t.Execute(ctx, json.RawMessage(call.Args))
			if err != nil {
				// 框架级错误（契约破坏等）：中止循环并透传。
				return Result{}, fmt.Errorf("agent: execute tool %q: %w", call.Name, err)
			}
			a.history = append(a.history, Turn{Role: "tool", ToolCallID: call.ID, Content: res.Data})
		}
	}
	return Result{}, fmt.Errorf("agent: exceeded %d tool rounds", maxToolRounds)
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
