# Trace 模块设计文档

> 位置：`internal/trace`
> 状态：**设计稿**（待评审后实现）
> 关联：`agent.md` 工具观测扩展点（`WithToolObserver`）；`tool.md` 工具三要素；`路线图.md` 第二梯队"工具轨迹"

## 1. 定位与职责

记录工具调用的**元数据**（耗时、顺序、失败标记、审计信息），独立成文件供调试与审计。它不改变对话循环的任何行为——模型看到的输入、回灌内容、会话历史全部不受影响。

一句话：**session 管"回灌模型的对话历史"，trace 管"工具调用的观测元数据"，两者永不混写。**

依赖方向：

```
main(组合根) → trace(叶子包，只 import 标准库)
agent ──WithToolObserver── main 包装 ──→ trace.Store.Append
```

- trace 是叶子包，不 import 任何内部包（对齐 memory 的叶子地位）。
- agent 完全不感知 trace：通过现有 `WithToolObserver` 回调解耦，组合根把写盘动作包装成 observer 注入。

## 2. 边界划分

### 2.1 与 session 的边界（核心红线）

- session 语义 = **回灌模型的对话历史**。混入耗时/审计会污染回灌内容（模型会把 "12ms" 当事实学习）。
- trace 独立文件，语义 = **人读可查的观测日志**。
- 对照市面：Harness trajectory / Codex rollout 均与对话消息分离存储，本项目同哲学。

### 2.2 失败语义：观测数据是次要失败

- 对话持久化失败必须透传（盘上数据与内存不一致，不能静默）——session 属契约数据。
- trace 写失败属次要失败：组合根包装层记 stderr 后丢弃，**绝不打断对话**。观测缺失不影响正确性。

### 2.3 只记真实执行

- 模型请求未注册工具的 case 不观测（维持 `ToolObserver` 现状语义：仅实际执行成功后触发）。
- 理由：该失败已以 `"未注册的工具: xxx"` 工具结果回灌并落盘 session，trace 重复记录无增量价值。

### 2.4 只增不删（审计语义）

- `Reset` 删会话文件时 trace 文件保留——观测日志天然只增不删，符合审计定位；残留的孤立 trace 文件无副作用。

## 3. 数据形态

- 文件：`<sid>.trace.jsonl`，与会话文件同目录（`SMALL_SESSION_DIR`）、同生命周期创建。
- 格式：JSONL，每行一条 Entry，人读可查（对齐 session 的落盘哲学）。
- 字段（对齐 `ToolCallEvent` + 补充元数据）：

| 字段 | 类型 | 含义 |
|---|---|---|
| ts | string | RFC3339 时间戳 |
| session | string | 会话 id（跨会话聚合 grep 用） |
| round | int | 工具循环第几轮（0 起），定位多轮轨迹/卡死 |
| name | string | 工具名 |
| args | string | 模型传入的原始参数 JSON |
| data | string | 执行结果文本（含业务失败） |
| isError | bool | 业务失败标记 |
| durationMs | int64 | 执行耗时毫秒（转 ms 保人读，不用 time.Duration 纳秒） |
| type | string | 事件类型：缺省 `tool`（工具调用）；plan 变更记 `plan`（plan.md §6）。omitempty，旧文件缺失按 `tool` 处理，向后兼容 |

## 4. 接口设计

```go
// Package trace 记录工具调用元数据（JSONL 追加），叶子包，不依赖任何内部包。
package trace

// Entry 一条工具调用轨迹记录。
type Entry struct {
	TS         time.Time
	Session    string
	Round      int
	Name       string
	Args       string
	Data       string
	IsError    bool
	DurationMs int64
}

// Store 是 trace 模块对象：惰性打开文件、追加写入 JSONL。
type Store struct {
	path string
	f    *os.File // 首次 Append 惰性打开
}

func New(path string) *Store
func (s *Store) Append(e Entry) error
```

- 构造注入完整文件路径（组合根用 `filepath.Join(sessionDir, sid+".trace.jsonl")` 计算），trace 不感知 session 目录语义。
- 惰性开文件：不 Append 则零副作用（纯对话/无会话时无 trace 文件产生）。
- 无并发锁：单会话单写者（一个 Agent 实例独享）；多 agent 写同一 trace 文件不在本期范围。

## 5. agent 侧改动（最小，纯加法）

- `ToolCallEvent` 增加 `Duration time.Duration` 字段（加法，不破坏现有构造与测试）。
- Run 循环内 `start := time.Now()` 计时，执行成功后 observe 携带 Duration。
- 不改 observer 触发时机（仍只在实际执行成功后触发，见 2.3）。

## 6. 组合根装配

```go
tr := trace.New(filepath.Join(sessionDir, sid+".trace.jsonl"))
obs := func(ev agent.ToolCallEvent) {
	entry := trace.Entry{
		TS: time.Now(), Session: sid, Round: ev.Round,
		Name: ev.Name, Args: ev.Args, Data: ev.Result.Data,
		IsError: ev.Result.IsError, DurationMs: ev.Duration.Milliseconds(),
	}
	if err := tr.Append(entry); err != nil {
		log.Printf("trace: append: %v", err) // 次要失败，不打断对话
	}
}
agent.New(chat, reg, store, agent.WithToolObserver(obs), ...)
```

- 默认开启：跟随会话自动写（文件小、成本低；观测数据"有数据才有价值"，opt-in 等于没人开）。
- 配置：复用 `SMALL_SESSION_DIR`，不新增环境变量。

## 7. 行业实践对比

| 项目 | 做法 | 本项目取舍 |
|---|---|---|
| DeepSeek Harness | trajectory 逐帧记录 agent 全部动作、可回放调试 | 只做工具维度（当前唯一高价值事件源）；逐帧回放成本高，暂缓 |
| OpenAI Codex | rollout 元数据与对话消息分离存储 | 同哲学：trace 与 session 分离 |
| OpenClaw | 内置观测与日志 | 对齐 |

## 8. 落地路径

**阶段一（本模块）**：`internal/trace`（Store + JSONL + 单测：写文件/追加/格式/错误路径）+ agent 加 Duration + 组合根接线。
**阶段二（可选）**：CLI `--trace <sid>` 查看、跨会话失败率统计、耗时报表。

## 9. 待决问题

- 文件轮转/大小上限：YAGNI，暂不做；真出现再评估。
- 是否记录模型完整推理（thinking）：不记——thinking 已在会话输出展示，trace 聚焦工具执行。
- `round` 字段：`ToolCallEvent` 需补轮次信息（agent 循环内 `round` 在作用域内，顺手传入）。
