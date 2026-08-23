# Compaction（上下文压缩）模块设计文档

> 位置：第一版不建独立包，作为 `agent` 的预算/截断能力（`WithTokenBudget`）；后续摘要增强再看是否独立。
> 状态：**阶段一已实现**（预算估算 + 滑动窗口截断 + 持久化同步重写 + **真实 usage 校准**），已通过单测（含 `-race`）与 `go vet`、`gofmt`、`go build`。
> 关联：`agent.md`（历史无限累积）、`session.md`（持久化与截断的交互）、`provider.md`（流式 usage 传输）、`CLAUDE.md`。
> 决策基调：**第一版只做"预算估算 + 滑动窗口截断"，不做摘要**（零新增 LLM 调用）；截断与持久化**同步重写**；**真实 usage 优先，字符估算兜底**。

## 1. 定位与职责

compaction 的职责一句话：**控制发往模型的 token 总量**。现状 [agent.go](file:///home/cxr/Program/08_07_GO/small/internal/agent/agent.go#L99-L136) 每次 `Run` 把 `allTurns()`（含 system）全量发给模型，历史无限增长，迟早溢出 context window。

第一版只做两条：
1. **token 预算估算**：字符近似（`len/3` 保守系数），`0 = 不启用`。
2. **滑动窗口截断**：超预算时从头部丢弃最早的历史，**system 永远保留、最近一轮永远保留、工具调用轮次成组保留（不悬空）**。

**不做**（YAGNI）：摘要压缩（第二次 LLM 调用 + 摘要入历史/持久化的复杂度）、分级降级状态机、关键记忆抽取（那是长期记忆，roadmap 第二梯队）。

## 2. 市面方案对比（取舍依据）

| 方案 | 机制 | 代表 | 取舍 |
|---|---|---|---|
| 滑动窗口截断 | 保留最近 N 轮 | 所有 CLI 兜底 | ✅ **第一版采用**：零成本、可预测 |
| 摘要压缩 | 早期历史→模型摘要替换 | Claude auto-compact、Harness | ⏸ 第二步：语义保持好，但多一次 LLM 调用 |
| 分级降级 | 预算分档渐进 | OpenClaw 三级 | ❌ 状态机复杂，两档（截断+摘要）已等价其常用两级 |
| 关键记忆抽取 | 事实清单跨会话检索 | OpenClaw memory_search | ❌ 那是长期记忆，另一个模块 |
| token 预算估算 | 估每条 token 维护预算 | 各家基础设施 | ✅ 采用：触发精度需要它 |

**取舍依据**：单二进制 CLI、个人使用、溢出场景是"几十轮对话"。先让系统"不崩"（截断），再让它"记得牢"（摘要）。token 估算用字符近似而非官方 tokenizer——DeepSeek 无 Go 官方库，近似保守系数足够，宁多估不溢出。

## 3. 设计

### 3.1 估算（agent 包内纯函数）

```go
// estimateTokens 字符近似：约 1 字符 ≈ 1/3 token（中文偏紧、英文偏松），
// 保守取整偏大，宁多估不溢出。附加每条消息的 role 等固定开销。
func estimateTokens(s string) int
```

固定开销：每条消息 +4 token（role/分隔符），system 同样估算。

**估算只是兜底**：有真实 usage（3.4）时用真实计数校准，估算只在"无真实数据"（首轮 / 截断后 / 恢复后）时生效。

### 3.2 触发与截断（agent 行为开关）

```go
func WithTokenBudget(maxTokens int) Option
// maxTokens <= 0 表示不启用（默认）。启用后 Run 每轮在追加用户输入后检查：
// allTurns 估算超预算 → 从头部截断，直到不超。
```

截断规则（保证正确性）：
1. **system 不在 history 里**（`allTurns` 临时拼接），天然保留；
2. **从头部线性丢弃**：`user`/`assistant` 直接丢；`assistant(ToolCalls)` 丢弃时必须**连带丢弃紧随其后的连续 `tool` 结果**——`assistant(ToolCalls)→tool` 是配对，拆开会让模型看到悬空工具结果；
3. 停在不超预算的位置；**至少保留最近一条 user**（否则本轮对话无法继续）。

### 3.3 与持久化的同步（用户决策：同步重写）

截断修改的是 `history`，而 session 是 append-only——若不同步盘，重启恢复会把被截断的旧数据全部"复活"，截断被持久化抵消（这是本需求最绕的交互点）。

解法：**截断发生时若启用持久化，同步重写会话文件**：

```go
// session 新增
func (s *Store) Rewrite(id string, msgs []Message) error
// 全量覆写：临时文件写入 + os.Rename 原子替换，避免 O_TRUNC 崩溃留空文件。
```

agent 侧：截断真正发生时（有消息被丢），`store.Rewrite(sid, toSessionMsgs(history))`，`persisted = len(history)`。未发生截断的轮次仍走 append（无额外开销）。

### 3.4 真实 usage 校准（估算只是兜底）

**动机**：`usage.prompt_tokens` 是服务端对"上一轮请求全部输入"（system + 历史 + tools 声明）的**真实计数**，精度远高于字符估算。预算判断用"真实基线 + 新增估算"，字符估算只在没有真实数据时兜底。

**数据来源**：
- 非流式：`ChatResponse.Usage` 已有；
- **流式（默认路径）**：当前拿不到，需要 ① 请求注入 `stream_options: {include_usage: true}`（服务端在流末尾多发一个 `choices: []` + `usage` 的 chunk）；② `streamChunk` 加 `Usage` 字段并解析，经 `StreamCallbacks.OnUsage` 回调出去。

**语义与基线**（agent 侧）：
- `usage.prompt_tokens` 对应"当时发送的 allTurns"（含 system 与 tools 声明），故记录 `baseline = prompt_tokens`、`baselineLen = len(history)`（Complete 返回瞬间的历史长度）；
- 预算判断：`total = baseline + Σ估算(history[baselineLen:])`——基线精确覆盖旧历史，只对新增消息估算；
- **截断后基线失效**（截断改了历史，真实基线不再对应当前内容）→ 置 0 回到纯估算，下一次 Complete 后重新获得；
- 首轮 / 恢复（WithHistory）无基线 → 纯估算兜底，直到第一次 Complete；
- 截断递减时对基线内消息按 `msgTokens` 近似扣减——只影响"多丢/少丢一条"的边界，无伤大雅。

**Result 扩展**：`Result` 新增 `PromptTokens int`（预算校准所需最小信息），由 adapter 在流式/非流式两条路径翻译。

## 4. 影响模块评估

| 模块 | 影响 | 依据 |
|---|---|---|
| agent | 新增 `WithTokenBudget` + 估算函数 + 截断逻辑（Run 内触发） | 历史只有 agent 持有，截断是它的职责 |
| session | 新增 `Rewrite`（tmp+rename 原子覆写） | 截断需同步盘，append-only 无法"删旧行" |
| config | 新增 `MaxTokens`（环境变量 `SMALL_MAX_TOKENS`，默认 8000） | 预算属集中配置 |
| main | 组合根 `WithTokenBudget(cfg.MaxTokens)` | 装配职责 |
| provider / adapter / tool | 不动 | 传输层/隔离点/工具不感知历史管理 |

## 5. 验证（完整度评估）

| 层 | 用例 | 判据 |
|---|---|---|
| 单测-估算 | 已知长度字符串 | 估算随长度单调，字符近似符合预期 |
| 单测-触发边界 | 恰好不超 / 恰超 1 字符 | 不触发 / 触发（边界精确） |
| 单测-截断 | 超预算历史 | system 保留、最近一轮保留、最早被丢、**无悬空 tool**（配对完整性） |
| 单测-持久化同步 | 触发截断 → Rewrite → 重启恢复 | 恢复历史 == 截断后历史（不复活） |
| 集成-可持续 | mock completer 跑 100 轮 | 每轮发往模型的估算 token ≤ 预算，历史有上限 |
| 手工 | 长对话到溢出 | 不再超限错误，近期上下文仍准确 |

合并门槛：`go test -race ./...`、`go vet`、`gofmt` 全绿。

## 6. 落地路径

**阶段一（本需求）✅ 已实现**：
- session：`Rewrite`（tmp+rename 原子覆写）+ 单测（覆写、空写、Rewrite 后可继续 Append）。
- agent：`WithTokenBudget` + 估算（`compact.go`）+ 截断 + 持久化同步 + 单测（触发边界、成组丢弃、同步不复活、无截断不重写）。
- **真实 usage 校准（3.4）✅**：provider 流式 `stream_options.include_usage` + `OnUsage` 回调 + 流式/非流式 `Result.PromptTokens` 翻译；agent 基线 + 截断失效；单测（流式 usage chunk、回调中止、adapter 翻译、基线记录/失效/兜底）。
- config：`MaxTokens`（`SMALL_MAX_TOKENS`，默认 8000，显式 0 禁用）；main 接线。
- 更新 `CLAUDE.md`（决策表 + 目录说明）、`路线图.md` 勾掉该项。

**阶段二（后续增强，不在此次）**：摘要压缩——触发条件、摘要消息形态（role=user 带 `[总结]` 前缀）、复用 Completer 的端口评估。

## 7. 待决问题

- 摘要压缩的触发阈值与截断如何共存（截断先丢、摘要兜底丢的部分）——阶段二设计。
- 截断是否对用户可见（打印"上下文已压缩"提示）——建议加，一行输出。
- 真实 usage 的校准系数验证：DeepSeek 流式 usage 的实际返回形态（是否稳定返回）——已在 provider 层解析，待真实 API 验证。
