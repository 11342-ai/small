package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"small/internal/memory"
	"small/internal/tool"
)

// 记忆工具与 Echo() 同款构造风格：一工具一构造函数（tool.Tool），有状态依赖
// （记忆仓库）显式入参，由 RegisterBuiltins 装配。不做"集合构造函数"——
// 保持与既有内置工具一致的形态，且让 register.go 的内置清单一眼可见。
//
// 执行逻辑外置为具名函数（runMemorySearch / runMemoryGet）：NewFunc 的 run 签名
// 固定为 func(ctx, args)，不含 mem，只能靠闭包捕获仓库实例——所以 NewFunc 内
// 只留一行转发闭包，实质逻辑放在可独立单测的包级函数里。
//
// 失败语义：执行失败（参数错误、无结果、存储不可读）统一按业务失败（IsError）
// 回灌，由模型自行决定换词重试或直接作答。记忆检索失败对对话循环不致命，
// 不属于契约破坏，故不中止循环（tool 包边界约定：框架级错误才向上抛）。

// MemorySearch 构造长期记忆关键词检索工具（memory_search）。
func MemorySearch(mem *memory.Store) tool.Tool {
	return tool.NewFunc(
		tool.Spec{
			Name:        "memory_search",
			Description: "在长期记忆文件（MEMORY.md 与 memory/*.md）中按关键词检索相关片段，返回 top-N 命中（含文件位置与片段）。回答涉及先前决策、偏好、待办或项目事实前，应优先调用本工具。",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"query": {"type": "string", "description": "检索关键词，可用空格分隔多个词，支持中文"},
					"limit": {"type": "integer", "description": "最多返回条数，默认 5"}
				},
				"required": ["query"]
			}`),
		},
		func(_ context.Context, args json.RawMessage) (tool.Result, error) {
			return runMemorySearch(mem, args)
		},
	)
}

// runMemorySearch 记忆检索的执行逻辑：解码参数 → 关键词检索 → 格式化命中。
// 独立成具名函数（而非内联闭包）：逻辑可脱离工具壳直接单测。
func runMemorySearch(mem *memory.Store, args json.RawMessage) (tool.Result, error) {
	var in struct {
		Query string `json:"query"`
		Limit *int   `json:"limit"` // 指针区分"未传"与"显式 0"
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return tool.Result{Data: "参数错误: " + err.Error(), IsError: true}, nil
	}
	limit := 5
	if in.Limit != nil && *in.Limit > 0 {
		limit = *in.Limit
	}
	hits, err := mem.Search(in.Query, limit)
	if err != nil {
		// 参数/存储错误统一按业务失败回灌（见文件头注释）。
		return tool.Result{Data: "检索失败: " + err.Error(), IsError: true}, nil
	}
	if len(hits) == 0 {
		return tool.Result{Data: "未找到相关记忆", IsError: true}, nil
	}
	var b strings.Builder
	for i, h := range hits {
		fmt.Fprintf(&b, "[%d] %s (score=%.3f)\n%s\n", i+1, h.Ref, h.Score, h.Snippet)
	}
	return tool.Result{Data: b.String()}, nil
}

// MemoryGet 构造记忆精读工具（memory_get）：search 的片段不够时按 Ref 读整块。
func MemoryGet(mem *memory.Store) tool.Tool {
	return tool.NewFunc(
		tool.Spec{
			Name:        "memory_get",
			Description: "读取 memory_search 返回的完整块原文（按 Ref），用于需要完整上下文而非片段时。",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"ref": {"type": "string", "description": "memory_search 返回的 Ref，如 memory/20260823.md#2"}
				},
				"required": ["ref"]
			}`),
		},
		func(_ context.Context, args json.RawMessage) (tool.Result, error) {
			return runMemoryGet(mem, args)
		},
	)
}

// runMemoryGet 记忆精读的执行逻辑：解码参数 → 按 Ref 读整块。
func runMemoryGet(mem *memory.Store, args json.RawMessage) (tool.Result, error) {
	var in struct {
		Ref string `json:"ref"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return tool.Result{Data: "参数错误: " + err.Error(), IsError: true}, nil
	}
	content, err := mem.Get(in.Ref)
	if err != nil {
		return tool.Result{Data: "读取失败: " + err.Error(), IsError: true}, nil
	}
	return tool.Result{Data: content}, nil
}

// MemorySave 构造记忆受控写入工具（memory_save）。
// 触发约束：Description 声明"仅在用户明确要求记住时调用"，配合 system prompt 双重限定，
// 防模型自作主张写垃圾；写入只进归档层（Append 锁死），不污染常驻上下文。
func MemorySave(mem *memory.Store) tool.Tool {
	return tool.NewFunc(
		tool.Spec{
			Name:        "memory_save",
			Description: "把一段内容追加进长期记忆的归档层（memory/YYYY-MM-DD.md，可被后续 memory_search 检索，不注入常驻上下文）。仅在用户明确要求记住某事时调用；写入内容应为用户原话或已确认的事实。",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"topic": {"type": "string", "description": "本条记忆的标题（自动转成 ## 标题，缺省'备忘'）"},
					"content": {"type": "string", "description": "要记住的内容"}
				},
				"required": ["content"]
			}`),
		},
		func(_ context.Context, args json.RawMessage) (tool.Result, error) {
			return runMemorySave(mem, args)
		},
	)
}

// runMemorySave 记忆受控写入的执行逻辑：解码 → 拼 ## 标题块 → 追加到按日期命名的归档文件。
func runMemorySave(mem *memory.Store, args json.RawMessage) (tool.Result, error) {
	var in struct {
		Topic   string `json:"topic"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return tool.Result{Data: "参数错误: " + err.Error(), IsError: true}, nil
	}
	topic := strings.TrimSpace(in.Topic)
	if topic == "" {
		topic = "备忘"
	}
	content := strings.TrimSpace(in.Content)
	if content == "" {
		return tool.Result{Data: "参数错误: content 为空", IsError: true}, nil
	}
	// 按日期归档（OpenClaw 每日日志同款）：同一天追加到同一文件，以 ## 标题分块检索。
	name := time.Now().Format("20060102")
	block := "## " + topic + "\n" + content
	if err := mem.Append(name, block); err != nil {
		return tool.Result{Data: "写入失败: " + err.Error(), IsError: true}, nil
	}
	return tool.Result{Data: "已记住（memory/" + name + ".md）"}, nil
}
