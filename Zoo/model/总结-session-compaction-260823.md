# 会话总结：Session 持久化 + Compaction（上下文压缩）模块

> 时间：2026-08-23
> 范围：`internal/session` 存储部件 + agent 自动持久化 + config yaml + compaction（预算截断 + 真实 usage 校准）+ 依赖纪律加固
> 状态：全部最小可行实现完成，`gofmt` / `go vet` / `go test -race` / `go build` 全绿
> 关联：`Zoo/session.md`、`Zoo/compaction.md`、`Zoo/踩坑.md`（#8/#9）、`CLAUDE.md`（约定）、`Zoo/路线图.md`

## 1. 本会话解决的问题与边界

### 1.1 问题

1. **会话持久化 + 续聊**：`Agent` 的历史在内存里，进程一退全丢。目标是跨进程存活、多会话可管理。
2. **上下文压缩**：历史无限增长，迟早撑爆 context window。目标是控制发往模型的 token 总量。
3. **依赖纪律加固**：盘点依赖风险，把"唯一真风险"（隔离点）用测试固化。

### 1.2 问题边界

| 层 | 职责 | 明确的"不做什么" |
|---|---|---|
| `session` | 存储部件：JSONL 每会话一文件（List/Load/Append/Delete/Rewrite） | 不 import agent（避免循环），自持 `Message` 镜像模型 |
| `agent` | 自动持久化 + 预算截断 + 真实 usage 基线 | 不感知 provider DTO；翻译收敛在 adapter.go / persist.go |
| `config` | 环境变量优先 + 可选 `~/.small/config.yml`，唯一读取点 | API key 只走环境变量，不落配置文件 |
| `provider` | 流式 usage 传输（`stream_options.include_usage` + `OnUsage`） | 不感知业务语义 |
| `main` | 组合根：`--session` 续聊 + `WithTokenBudget` 装配 | 不逐个枚举内部逻辑 |

依赖方向：

```
main → agent → session（存储部件，不 import agent）
            ↘ tool ← tool/builtin
            ↘ provider → config
                        ↘ retry
```

**明确不做（YAGNI）**：SQLite、摘要压缩（compaction 阶段二）、分级降级状态机、长期记忆/检索、交互式会话命令（`/list` `/use`）。

## 2. 分歧与结论

### 2.1 session 依赖方向与存储类型（本会话最大分歧）

- **分歧**：我最初设计"session 直接存 `agent.Turn`"（依赖方向 `main → session → agent`，session 是 agent 的上层外壳）；用户主张 session 是**部件**、像 `Config → provider`、`tool → agent` 一样**整个装进 `agent.New`**。
- **结论**：`agent → session`；session 不 import agent（否则循环），自持 `Message` 镜像，`Turn↔Message` 翻译收敛在 agent 的 `persist.go`。
- **决策依据**：功能需求决定形态 + 组合直觉（部件模式）；连带产生踩坑 #8（依赖方向 = import 方向，"把 X 塞进 Y" ⇔ "Y 依赖 X"）。
- **代价与补偿**：代价是 Message 字段镜像 + 一层翻译；补偿是 session 独立性（未来 CLI 工具 / 评测回放不必拖 agent）。已记录"Turn 上提 `internal/domain`"为扩展点（第二个消费方出现时触发）。

### 2.2 config 是否现在引入 yaml

- **分歧**：我建议本阶段不引入（配置项 ≤ 3，纯环境变量足够）；用户主张现在引入，为后续配置项铺路。
- **结论**：引入 `yaml.v3`（纯 Go、零 CGO）；`Load()` 合并"环境变量 > 配置文件 > 默认值"；**API key 仍只走环境变量**（机密红线不变）。
- **决策依据**：用户明确意愿 + 配置项增长可预期；`Load()` 仍是唯一读取点，构造注入形态不变。

### 2.3 Thinking 是否入盘

- **分歧/疑问**：用户问"deepseek 是不是需要把 thinking 发回去"。
- **结论**：**不回传**——OpenAI o 系 / DeepSeek / Anthropic 均不回传 reasoning 给模型；落盘内容 = 内存历史（Reply + ToolCalls）。Thinking 是当轮展示物，不存。
- **决策依据**：落盘数据的唯一用途是"恢复后回灌模型"，thinking 不回灌，存了即冗余。

### 2.4 会话恢复位置（实现期修正，非用户分歧）

- **设计稿**：`WithSession(id)` 启动时在 `New` 内 Load 恢复。
- **实现发现**：`New` 不返回 error，而文件读取是系统边界——错误无处传播。
- **修正**：组合根 `store.Load` → `agent.FromSession` → `WithHistory` 注入；`WithSession` 只管持久化。
- **决策依据**：错误应在系统边界处理（组合根 fail fast），领域装配不承担 IO 错误。

### 2.5 compaction 范围与截断-持久化交互

- **范围分歧**：我建议第一版只做"预算估算 + 滑动窗口截断"（摘要引入第二次 LLM 调用 + 摘要入历史/持久化的复杂度）；用户确认。
- **交互方案**：截断修改 `history`，而 session 是 append-only——不同步盘会让被截断数据"复活"。三方案中用户选**同步重写**（session 加 `Rewrite`，tmp+rename 原子替换）。
- **决策依据**：正确性优先于实现省事；"恢复后与内存一致"的语义洁癖值得一次全量写（数据量小）。

### 2.6 真实 usage 校准（用户提出的方向）

- **用户观点**：provider 返回里有 token 用量，应该用起来；字符估算只是"没有真实数据时"的兜底。
- **关键事实**：非流式 `ChatResponse.Usage` 早已存在，但**项目恒走流式，而流式从未解析 usage**——要拿真实数据，必须 ① 请求注入 `stream_options.include_usage`；② 解析流末尾的 usage chunk。
- **结论**：实现"真实基线（usage.prompt_tokens）+ 新增消息估算"，估算兜底（首轮/截断后/恢复后）。截断后基线失效。
- **决策依据**：用户的判断与业界一致（真实用量优先、估算兜底）；成本是跨 provider/adapter/agent 三层的接线，收益是触发精度。

## 3. 踩坑记录（忽视临界情况的代价）

### 3.1 依赖方向反直觉（踩坑 #8）

- **现象**：`session` 被塞入 `agent.New`，直觉以为依赖是 `session → agent`，实际是 `agent → session`。
- **根因**：依赖方向 = import 方向 = "谁需要谁"；注入（塞入）要求被注入方被 import，故 `agent → session`。把数据流/服务关系误当依赖方向。
- **解决**：记录踩坑 #8（含循环陷阱推论：session 不得再 import agent）。

### 3.2 流式 usage 拿不到（忽视数据路径）

- **现象**：以为"provider 有 usage 直接用"，实际项目恒走流式，流式 `streamChunk` 无 Usage 字段、也不解析。
- **根因**：谈"利用数据"前没先确认"数据在当前路径上是否存在"——流式默认不回 usage（需 `stream_options.include_usage`）。
- **解决**：provider 注入 `stream_options` + `streamChunk.Usage` + `OnUsage` 回调，adapter 翻译，agent 基线校准。

### 3.3 测试断言计数错误（compact 测试多次修正）

- **现象**：`TestCompact_*` 断言历史条数时多次失败——没算上 Run 成功后会追加的 assistant 回复。
- **根因**：断言基于"截断前/注入时"的状态，忘了 Run 是"追加 user → 截断 → Complete → 追加 assistant"的完整状态变迁。
- **解决**：逐项按最终 `History()` 内容断言（而非条数直觉）。

### 3.4 `os.WriteFile` 覆盖而非追加（session 测试）

- **现象**：坏行容错测试用 `os.WriteFile` 追加坏行，实际它**覆盖**整个文件，把合法行也抹掉了。
- **解决**：改用 `os.OpenFile(O_APPEND)` 追加写。

### 3.5 "新建文件处理 provider"撞隔离点（设计层面规避，未踩坑）

- **临界情况**：用户想"在 agent 下新建文件针对 provider 处理，让目录更干净"——这会让第二个文件 import provider，直接破坏刚固化的隔离点纪律。
- **处理**：澄清"目录干净 = 每文件一职责而非文件少"；adapter.go（160 行）未到拆分规模；现状维持。拆分子包的信号（第二个后端适配器）出现时再评估。
- **结果**：设计先行，未踩坑。

## 4. 后续开发的统一与连贯性

以下条目为后续开发硬性对齐项（核心已写入 CLAUDE.md）：

1. **依赖严格单向 + 隔离点**：`adapter.go`（provider）、`persist.go`（session）是翻译收敛点；`session` 不 import agent。
2. **依赖纪律用测试固化**：新增 `imports_test.go`——`agent` 包内只有 `adapter.go` 允许 import provider，软纪律变合并门槛（踩坑 #9：编译期兜底的三个"伪风险"不动代码）。
3. **结构协作对象显式入参、行为开关走 Option**：session 复用同一路线（`agent.New(chat, tools, store, opts...)`）。
4. **存储形态**：JSONL append（崩溃只丢半行）+ `Rewrite`（tmp+rename 原子覆写，截断同步盘）。
5. **真实数据优先、估算兜底**：能拿到服务端真实计数（usage）就优先用，字符估算只在无真实数据时兜底。
6. **config 唯一读取点**：环境变量 + 可选 yaml 都在 `Load()` 合并，机密只走环境变量。
7. **错误在系统边界处理**：`New` 不返回 error 时，IO 错误由组合根处理（Load/WithHistory 模式）。
8. **文档先行 + 节点提交**：每个最小可行实现里程碑即为提交点（本会话 3 次提交）。

## 5. 遗留问题与妥协

| 遗留项 | 类型 | 触发条件 / 后续方向 |
|---|---|---|
| DeepSeek 流式 usage chunk 真实形态 | 待真实 API 验证（用户已确认不紧急） | 下次真实长对话/正式使用顺手验证一次（compaction.md 已标记） |
| 摘要压缩（compaction 阶段二） | 延迟决策 | 截断丢语义成为痛点时：摘要消息形态（role=user 带 `[总结]` 前缀）+ 复用 Completer 端口 |
| 截断对用户可见提示 | 简化妥协 | 建议加一行"上下文已压缩"输出 |
| 交互式会话命令（`/list` `/use`） | 延迟决策 | 会话管理体验需求时评估 |
| 自动恢复最近会话 | 延迟决策 | 缺省新建时间戳会话；需要"接着聊"时评估 |
| `Turn` 上提 `internal/domain` | 扩展点 | 第二个消费 Turn 的模块（memory/trace）出现时触发，消除 Message 镜像 |
| 长期记忆 / 子代理 / 工具轨迹 | roadmap 候选 | 会话持久化与上下文管理已落地，下一模块待选 |

## 6. 参考文档

- `Zoo/session.md`（会话持久化设计：JSONL 形态 / agent 接入 / 恢复 / 验证矩阵）
- `Zoo/compaction.md`（上下文压缩设计：估算 / 截断 / 持久化同步 / 真实 usage 校准 3.4）
- `Zoo/踩坑.md`（#8 依赖方向、#9 真伪风险）
- `Zoo/路线图.md`（会话持久化 + 上下文压缩已勾掉）
- `CLAUDE.md`（项目约定，本会话新增：上下文压缩 / 真实 usage 决策表条目）
