# Agent 模块设计文档

> 位置：`internal/agent`
> 状态：**最小可行实现**（多轮循环 + 流式探测降级 + 思考过程），已通过单测（含 `-race`）。

## 1. 定位与职责

Agent 是**领域层**：提供最小化的多轮对话循环。核心约束是**与后端完全解耦**——`agent.go` 只依赖本包定义的领域模型（`Turn`/`Result`）和端口（`Completer`），不感知 provider 的任何 DTO。

依赖方向：

```
main(组合根) → agent → provider（仅 adapter.go 一个文件 import provider）
```

整个 `agent` 包中，只有 `adapter.go` 与 provider DTO 打交道，这是解耦的**隔离点**：换后端 / 加 mock 只动这一处，循环逻辑一行不改。

## 2. 整体轮廓

```
internal/agent/
├── agent.go        # 领域模型：Turn/Result + Completer 端口 + Agent（Run 循环）+ Option
├── adapter.go      # 唯一 import provider 的文件：Turn↔Message 翻译 + 流式探测降级 + WithThinking
├── adapter_test.go # completerOnly mock（非流式路径）+ httptest 真实链路（流式路径）
```

## 3. 核心抽象

### 3.1 领域模型（独立于任何后端）

```text
type Turn struct { Role, Content string }        // 对话消息（user/assistant/system）
type Result struct { Reply, Thinking string }    // 一轮输出：回复正文 + 思考过程
```

### 3.2 端口（消费方定义接口）

```text
type Completer interface {
    Complete(ctx context.Context, turns []Turn) (Result, error)
}
```

- 遵循 Go "accept interfaces, return structs"：接口定义在**使用方**（agent），provider 侧通过适配器满足它。
- 这与 provider 自己的 `Completer/Streamer` 接口**不是同一个**（签名、包、语义层级都不同），同名只是巧合。

### 3.3 Agent 循环

```text
func (a *Agent) Run(ctx, userInput) (Result, error)
```

每轮：追加用户输入 → 把完整历史（含系统提示）交给端口 → 把回复正文追加进历史 → 返回 `Result`。多轮 = 多次 `Run` 累积历史。

- 系统提示经 `WithSystemPrompt` 注入，作为每次请求历史的首条。
- **思考过程不入历史**：`history` 只存 `Reply`，避免把推理过程回传给模型造成对话噪音。

### 3.4 适配器（providerChat）

- `NewProviderChat(client provider.Completer, model string, opts ...AdapterOption)`。
- `WithThinking(bool)`：控制请求是否显式开启 `thinking: enabled`（默认关）。注意 DeepSeek 的思考模式必须显式开启，否则模型不产出 `reasoning_content`。

## 4. 流式探测与降级（统一入口）

```text
if streamer, ok := p.client.(provider.Streamer); ok {
    return p.completeViaStream(ctx, streamer, req)   // 流式：OnThinking→Thinking, OnContent→Reply
}
return p.completeViaNonStream(ctx, req)              // 降级：Content + ReasoningContent
```

- **探测必须在发请求前完成**；一旦进入流中（拿到 2xx），失败**绝不回退**到非流式——避免重复已吐 token（与 provider 层"2xx 后零重试"原则一致）。
- 当前 `*provider.Client` 支持流式，恒真走流式路径；降级分支为"只实现 Completer 的后端"预留，由测试中的 `completerOnly` mock 锁住。

## 5. 重点取舍（决策记录）

| 问题 | 决策 | 理由 |
|---|---|---|
| 解耦方式 | 端口 + 适配器（vs 直接依赖 DTO / 策略注入 / 失败回退） | 核心循环可独立 mock、换后端只动 adapter；失败回退会重复 token，弃用 |
| 适配器注入参数 | `provider.Completer` 接口（vs 具体 `*provider.Client`） | 降级探测要求字段能容纳能力不同的实现（多态载体）；接口会窄化能力是代价，但这里探测就是核心需求 |
| 边界防御 | 空 turns 提前报错；空 choices 报错；role/content 白名单**不做** | 合法角色由领域层保证，白名单塞进适配层会与领域规则纠缠；信任内部契约，不做不可达分支防御 |
| 思考过程返回 | `Result` struct（vs 回调观察者 / 双返回值） | 类型安全、可扩展（以后加 usage 等）、符合"输入输出打包 struct"一贯偏好 |
| 思考开关 | `WithThinking` 默认关 | 保持原行为，需要时显式开启（更慢更贵是开启的代价） |

### 5.1 一个值得记住的反复

`NewProviderChat` 参数曾经历"具体类型 → 接口 → 再讨论"的往返：具体类型直白、能力完整，但**无法承载降级探测**（`*provider.Client` 上断言 `Streamer` 恒真）。结论：**功能需求决定形态**——要探测多态，字段必须是接口；不要为"用接口"而用接口。

## 6. 测试设计与踩坑

### 6.1 测试方法

- **非流式/降级路径**：`completerOnly`（只实现 `provider.Completer` 的 mock）直接注入，轻量精准。
- **流式路径**：真实 `provider.Client` + `httptest.Server`（SSE 响应），全链路验证拼接、`stream=true`、`thinking` 开关。
- 关键用例：`NoFallbackAfterStreamError` 断言"流中非法 chunk → 透传错误且请求数 = 1"（锁死不回退）。

### 6.2 踩坑（与 provider 共用的教训）

provider 流式的空闲超时 bug（请求绑定外层 ctx，取消 `streamCtx` 无效）说明：**context 取消必须作用在 transport 监听的 ctx 上**。adapter 流式路径依赖 provider.Stream 的正确性，因此必须保留流中错误回退测试来兜住这层契约。

## 7. 后续开发标准（工程一致性）

1. **领域与传输分离**：领域层（agent）只碰自己的模型，传输层（provider）只碰 DTO，中间仅 adapter 翻译。
2. **接口定义在使用方**；构造注入 + 组合根，禁止隐式全局依赖。
3. **状态管理收敛**：`Agent` 是唯一持有历史的地方，`History()` 返回拷贝，防外部篡改。
4. **功能选项扩展**：`Option`/`AdapterOption` 只增不改签名。
5. **测试**：`-race`、`errors.Is/As`、确定性优先；降级路径必须有 mock 锁住。

## 8. 已知扩展点

- ~~**历史截断**~~：**已实现**——`WithTokenBudget` 按估算 token 截断历史（`compact.go`），超预算从头部成组丢弃（工具轮次不悬空），截断同步重写会话文件，见 `compaction.md`。
- ~~**工具调用循环**~~：**已实现**——`Turn/Result` 新增工具调用形态，`Agent.Run` 变为多轮循环（执行 + 回灌，`maxToolRounds=8` 兜底），翻译与分片拼接收敛在 adapter，见 `tool.md`。
- **流式输出上推到端口**：当前 adapter 内部拼完整回复再返回，未来可让 `Result` 携带回调或增量通道。
- **`finish_reason` 暴露**：内容被 `max_tokens` 截断时当前静默返回，若要感知截断需扩展端口。
- ~~**Agent 单测**~~：**已补**——`agent_test.go` 覆盖 `Run` 的历史累积、思考不入历史、工具循环、未知工具回灌、框架错误中止、超轮数兜底。
