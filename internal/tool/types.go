// Package tool 承载"给模型使用的工具"能力的三要素：声明（Spec）、执行（Execute）、
// 注册（Registry）。
//
// 边界约定：
//   - tool 包不感知任何传输层 DTO（provider）；声明的序列化翻译由消费方
//     （agent 的 adapter）负责，tool 包本身不 import provider；
//   - 参数统一以原始 JSON 传入（json.RawMessage），由各工具自行解码为强类型，
//     解码失败属系统边界，应作为业务失败返回而非中断调用方循环；
//   - 业务失败以 Result.IsError 标记返回，框架级错误才返回 error（见 Result）。
package tool

import "encoding/json"

// Spec 工具的静态声明，是"模型看到的契约"：名称、描述、参数说明三件套。
// 三件套永远一起被序列化（进 tools 数组就是一次打包），故聚合为一个整体，
// 而非拆成三个取值方法；未来扩展字段（如 strict 严格模式）只需扩结构体。
type Spec struct {
	// Name 唯一标识，注册时用作 Registry 的键。
	Name string
	// Description 给模型的说明，决定模型何时选用该工具。
	Description string
	// Parameters 参数说明，JSON Schema 格式。
	// OpenAI（parameters）、Claude（input_schema）、MCP（inputSchema）等
	// 各家的底层标准都是 JSON Schema，跨提供商通用。
	Parameters json.RawMessage
}

// Result 一次执行的输出，回灌模型供下一轮推理。
type Result struct {
	// Data 回灌文本（JSON 或纯文本）。
	Data string
	// IsError 标记业务失败：工具执行"失败"（如 API 404、参数非法）作为
	// 结果正常返回，由模型自行决定重试或换策略；仅框架级错误
	// （panic、配置错误、契约破坏）才以 error 向上抛并中止整个调用循环。
	IsError bool
}
