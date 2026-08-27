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
	"time"

	"small/internal/session"
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
	// PromptTokens 本轮请求的服务端真实输入 token 数（usage.prompt_tokens），
	// 0 表示未提供（如某些降级路径）。Agent 用它校准上下文预算（见 compact.go）。
	PromptTokens int
}

// ToolCallEvent 一次工具调用的观测事件，供展示层实时渲染与轨迹记录
// （工具名/入参/结果/耗时/轮次）。
type ToolCallEvent struct {
	// Name 工具名。
	Name string
	// Args 参数原始 JSON（模型传参，原样呈现）。
	Args string
	// Result 执行结果（Data + IsError），含业务失败标记。
	Result tool.Result
	// Duration 工具执行耗时（从开始执行到返回结果），供轨迹记录。
	Duration time.Duration
	// Round 工具循环第几轮（0 起），供轨迹定位多轮顺序。
	Round int
}

// ToolObserver 工具调用观察者：Run 在每次工具实际执行后回调。
// 可注入展示层（实时打印）或打点器；nil 时不触发——调用方不注入则完全无感知。
type ToolObserver func(ToolCallEvent)

// 编译期断言：确保适配器在编译期满足端口（实现见 adapter.go）。
var _ Completer = (*providerChat)(nil)

// maxToolRounds 单次 Run 内工具调用的最大轮数，防御模型无限循环调用工具。
const maxToolRounds = 8

// Agent 持有对话历史并驱动多轮循环：每调用一次 Run 完成一轮 user→assistant。
// store/sid 非空时启用会话持久化：Run 成功结束自动把本轮新增历史追加落盘（见 persist.go）。
// budget > 0 时启用上下文预算：每轮追加输入后按估算 token 截断历史（见 compact.go）。
type Agent struct {
	chat        Completer
	system      string
	history     []Turn
	tools       *tool.Registry // 为 nil 时工具调用不可用，退化为纯对话
	store       *session.Store // 为 nil 时不做持久化（纯对话）
	sid         string         // 当前会话 id；仅 store 非 nil 且 sid 非空时生效
	persisted   int            // 已落盘的历史条数（恢复/注入初始历史后=len(history)）
	budget      int            // 估算 token 预算；<=0 不启用截断
	baseline    int            // 最近一次 Complete 的真实 prompt_tokens（usage）；0 表示无基线
	baselineLen int            // 基线对应的历史长度（Complete 返回瞬间 len(history)）
	observe     ToolObserver   // 工具调用观察者；nil 时不触发（nil-safe 回调约定）
}

// Option 以函数式选项配置 Agent。
type Option func(*Agent)

// WithSystemPrompt 设置初始系统提示，作为每次请求历史的首条消息。
func WithSystemPrompt(p string) Option {
	return func(a *Agent) { a.system = p }
}

// WithSession 启用会话持久化并绑定会话 id：Run 成功后自动把新增历史追加到 store。
// store 为 nil 时忽略（纯对话不受影响）。恢复历史由组合根 Load 后经 WithHistory 注入，
// 因为 New 不返回 error，而文件读取是系统边界，错误应在组合根处理。
func WithSession(id string) Option {
	return func(a *Agent) { a.sid = id }
}

// WithHistory 注入初始历史（启动恢复用），此后 Run 只追加新增部分。
// 传入切片会拷贝，防止外部篡改内部状态；未启用持久化时同样有效（纯内存恢复）。
func WithHistory(turns []Turn) Option {
	return func(a *Agent) {
		a.history = append([]Turn(nil), turns...)
		a.persisted = len(a.history)
	}
}

// WithTokenBudget 启用上下文预算（按估算 token，字符近似）：每轮追加输入后
// 超预算则从头部截断历史（system 与最近一轮保留）。<=0 表示不启用（默认）。
func WithTokenBudget(maxTokens int) Option {
	return func(a *Agent) { a.budget = maxTokens }
}

// WithToolObserver 注入工具调用观察者：每次工具实际执行后回调
// （名称/参数/结果，供展示层实时渲染）。传 nil 则等同不注入（默认行为不变）。
func WithToolObserver(obs ToolObserver) Option {
	return func(a *Agent) { a.observe = obs }
}

// New 构造 Agent。结构协作对象以显式参数注入（chat、tools、store），便于测试时替换 mock；
// tools 为 nil 时退化为纯对话，store 为 nil 时不做持久化。行为开关走 Option。
func New(chat Completer, tools *tool.Registry, store *session.Store, opts ...Option) *Agent {
	a := &Agent{chat: chat, tools: tools, store: store}
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
	if err := a.enforceBudget(); err != nil {
		// 截断后同步落盘失败必须透传：盘上旧数据与内存不一致，不能静默。
		return Result{}, err
	}

	for round := 0; round < maxToolRounds; round++ {
		result, err := a.chat.Complete(ctx, a.allTurns())
		if err != nil {
			return Result{}, err
		}
		// 记录真实用量基线：prompt_tokens 对应"发送时的全部输入"，
		// baselineLen 取 Complete 返回瞬间的历史长度，供下轮预算判断（见 compact.go）。
		if result.PromptTokens > 0 {
			a.baseline = result.PromptTokens
			a.baselineLen = len(a.history)
		}

		if len(result.ToolCalls) == 0 {
			a.history = append(a.history, Turn{Role: "assistant", Content: result.Reply})
			if err := a.persistRun(); err != nil {
				// 持久化失败必须透传：调用方以为已保存，实际未落盘，不能静默。
				return Result{}, err
			}
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
			// 执行计时：耗时随观测事件流出，供轨迹记录（trace 不感知执行过程，
			// 只消费事件；执行成功才观测——未注册工具等失败已在历史里回灌可见）。
			start := time.Now()
			res, err := t.Execute(ctx, json.RawMessage(call.Args))
			if err != nil {
				// 框架级错误（契约破坏等）：中止循环并透传。
				return Result{}, fmt.Errorf("agent: execute tool %q: %w", call.Name, err)
			}
			// 执行成功才观测：事件含名称/入参/结果/耗时/轮次，展示层据此实时渲染（nil-safe）。
			if a.observe != nil {
				a.observe(ToolCallEvent{
					Name: call.Name, Args: call.Args, Result: res,
					Duration: time.Since(start), Round: round,
				})
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

// Reset 清空对话历史（系统提示保留）。启用持久化时同步删除会话文件，
// 否则下次追加会与盘上旧数据重复。
func (a *Agent) Reset() error {
	a.history = a.history[:0]
	a.persisted = 0
	if a.store != nil && a.sid != "" {
		return a.store.Delete(a.sid)
	}
	return nil
}

// SetSystemPrompt 运行时替换系统提示（如 /persona 命令重定人格）。
// 系统提示不进历史（allTurns 每次现拼），替换不影响历史完整性；
// 语义上仅应在"无消息会话"时调用才安全（/persona 窗口已保证，见 Zoo/model/cli.md §6）。
func (a *Agent) SetSystemPrompt(p string) { a.system = p }

// allTurns 返回包含系统提示在内的完整历史（拷贝）。
func (a *Agent) allTurns() []Turn {
	turns := make([]Turn, 0, len(a.history)+1)
	if a.system != "" {
		turns = append(turns, Turn{Role: "system", Content: a.system})
	}
	return append(turns, a.history...)
}
