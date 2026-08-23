package provider

// ToolCallDelta 流式工具调用的增量分片。
// 同一 Index 的 ID / Name 通常只在首个分片出现，Arguments 分片需按 Index 拼接；
// 消费方按 Index 累积后还原完整调用。
type ToolCallDelta struct {
	Index     int
	ID        string
	Name      string
	Arguments string
}

// StreamCallbacks 流式输出的回调集合，全部字段可为 nil（nil-safe）。
//
// 语义约定：
//   - OnThinking / OnContent / OnToolCall / OnDone 返回非 nil error 会立即中止读取流，
//     该 error 将作为 Stream 方法的返回值传播；
//   - 流中错误（连接中断、解析失败、空闲超时等）只通过 Stream 方法的
//     返回值传播，不做回调，因此这里不设 OnError 字段。
type StreamCallbacks struct {
	// OnThinking 收到一段思考内容（reasoning_content）时触发。
	OnThinking func(segment string) error
	// OnContent 收到一段正式回答内容（content）时触发。
	OnContent func(segment string) error
	// OnToolCall 收到一段工具调用增量（delta.tool_calls）时触发。
	OnToolCall func(delta ToolCallDelta) error
	// OnDone 流正常结束时触发（收到 [DONE] 或服务端优雅关闭）。
	OnDone func() error
}
