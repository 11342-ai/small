# Session（会话持久化）模块设计文档

> 位置：`internal/session`
> 状态：**设计稿**——对齐 roadmap 第一梯队第 1 项（会话持久化 + 恢复）。
> 关联：`agent.md`（历史在内存，进程一退全丢）、`路线图.md`（JSONL 落盘，无需 SQLite）、`CLAUDE.md`。
> 决策基调：**不引入 SQLite，零新增依赖（除 config 的 yaml.v3）**；存储用 JSONL，每会话一个文件。

## 1. 定位与职责

session 是 agent 的**存储部件**：把对话历史落盘、跨进程存活、可多会话管理。"续聊"是组合根/agent 用它得到的**能力**，不是独立模块。

- **持久化**：把 `Turn` 序列化落盘（JSONL，每行一条消息）。
- **恢复**：按会话 id 从磁盘读回完整历史，注入新 `Agent` 直接续聊。
- **多会话**：可列出（`List`）、可删除（`Delete`），文件即会话。

### 核心判据（为什么是"部件"而不是独立外壳）

沿用 `Config → provider`、`tool → agent` 的组合先例：**session 被整个装进 `agent.New` 作为显式参数**，agent 是主人、部件是仆人。由此依赖方向为：

```
main → agent → session
          ↘ tool
          ↘ provider → config
```

**关键约束（循环依赖陷阱）**：`agent → session` 之后，session **不得再 import agent**（否则 `agent → session → agent` 编译报错）。因此 session 必须定义**自己的数据模型** `Message`，agent 侧做 `Turn ↔ Message` 翻译（领域内翻译，收敛在 agent 包的 `persist.go`）。

## 2. 整体轮廓

```
internal/session/
├── session.go        # Store 模块对象：New/List/Load/Append/Delete + Meta + Message/ToolCall
├── session_test.go   # round-trip 保真、坏行容错、稳定排序、文件隔离、-race
internal/agent/
├── agent.go          # New 签名加 store 参数 + WithSession + persisted 计数 + Reset 语义
├── persist.go        # 唯一做 Turn↔session.Message 翻译的文件 + 自动持久化时机
├── persist_test.go   # 集成：Run→落盘→新 agent 恢复→续聊引用前文
internal/config/
└── config.go         # Load() = 环境变量优先 + 可选 config.yml 覆盖；新增 SessionDir
```

## 3. 核心抽象

### 3.1 Store（模块对象，不接口化）

```go
func New(dir string) (*Store, error)        // 建目录（不存在则创建），返回模块对象
func (s *Store) List() ([]Meta, error)      // 按 id 升序稳定（扫目录 *.jsonl）
func (s *Store) Load(id string) ([]Message, error) // 逐行读回；坏行跳过；文件不存在=空历史
func (s *Store) Append(id string, msgs []Message) error // O_APPEND 追加，一次写一行
func (s *Store) Delete(id string) error     // 删除会话文件；不存在返回 nil
```

- **不接口化**：与 Registry 同理（tool.md 4.2）——唯一实现、无第二形态可替换，接口是空抽象。未来要换存储引擎（如 SQLite）时再评估。
- **并发**：内部 `sync.Mutex` 锁追加/删除，保证 `-race` 干净。多进程写同一会话**明确不支持**（CLI 单进程不存在该场景，注释注明）。
- **id 形态**：组合根决定（flag 传入；缺省生成时间戳，如 `20260823-153045`）。id 即文件名，**禁止包含路径分隔符**（Load/Append 前校验，防路径注入）。

### 3.2 Message（session 自己的数据模型，不 import agent）

```go
type Message struct {
    Role       string     `json:"role"`
    Content    string     `json:"content,omitempty"`
    ToolCallID string     `json:"tool_call_id,omitempty"`
    ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
}
type ToolCall struct {
    ID   string `json:"id"`
    Name string `json:"name"`
    Args string `json:"args"`
}
```

字段与 `agent.Turn` 镜像——因为 Turn 是 agent 的领域模型，session 要独立就得复制一份结构。翻译在 agent 侧，成本是字段级拷贝（见 5）。

### 3.3 Meta（List 的元数据）

```go
type Meta struct {
    ID        string
    TurnCount int
    UpdatedAt time.Time
}
```

来源：`List` 读每个文件首行（首个消息带时间戳）与总行数。为最小实现，`List` 只保证 **id + 行数** 稳定返回；`UpdatedAt` 若取不到则跳过（首行为空文件等边界），不阻塞 List。

## 4. 存储形态：JSONL，每会话一个文件

- **文件**：`<dir>/<id>.jsonl`。文件即会话，`List` 即扫目录，天然隔离。
- **行**：一行 = 一条 `Message`（`json.Marshal` 压缩无换行）。
- **追加**：`os.OpenFile(O_APPEND|O_CREATE|O_WRONLY)`，单次 `Write` 一行。小数据单行写入在 `O_APPEND` 下是原子追加（POSIX）。
- **崩溃语义**：写到一半被杀最多留下**半行坏数据**；恢复时跳过坏行，**已写完的完整行不丢**——这是 JSONL 相比"全量覆写"的核心优势。

### 坏行容错策略

Load 遇解析失败的行：**跳过，不报错**。理由：坏行 = 崩溃痕迹，是常态而非异常；"续聊"场景下静默跳过最顺滑。硬错误（目录不存在、文件不可读）才返回 error。若未来要审计坏行，再加统计返回。

## 5. agent 接入（结构依赖显式入参 + 行为开关 Option）

### 5.1 构造函数与选项

```go
func New(chat Completer, tools *tool.Registry, store *session.Store, opts ...Option) *Agent
// store 传 nil = 不持久化（纯对话），与 tools 传 nil 退化惯例一致。

func WithSession(id string) Option
// 绑定会话 id，启用自动持久化；store 为 nil 时忽略（纯对话不受影响）。

func WithHistory(turns []Turn) Option
// 注入初始历史（启动恢复用），此后 Run 只追加新增部分；未启用持久化同样有效。
```

- `Agent` 新增字段：`store *session.Store`、`sid string`、`persisted int`（已落盘条数）。
- **恢复不在 New 内做**：`New` 不返回 error，而文件读取是系统边界，错误应由组合根处理。组合根 `store.Load(id)` → `agent.FromSession(msgs)` → `WithHistory` 注入。这也让"恢复"在装配阶段显式可见（fail fast）。

### 5.2 自动持久化时机（用户决策：Run 结束时内部 append）

`Run` **成功返回**时：把 `history[persisted:]`（本轮新增的全部 turn）翻译成 `[]session.Message`，`Append` 进文件，`persisted` 前进到 `len(history)`。

- **失败不落盘**：与红线 4"失败只能透传错误，由调用方重试整个对话"一致——错误时调用方重跑整轮，避免盘上出现半轮状态。
- **只落内存历史，不落 Thinking**：`history` 只存 `Reply`（agent.go 既有约定）；DeepSeek/OpenAI/Anthropic 的 reasoning 都不回传模型，落盘数据的唯一用途是"恢复后回灌模型"，故不存（参考市面取舍，见 7）。

### 5.3 Reset 语义调整

`Reset()` 改为返回 `error`：清空内存历史、`persisted` 归零；若启用持久化则 `Delete(sid)` 删文件（否则下次 Append 会与盘上旧数据重复）。无外部调用方（已 grep 确认），签名可安全变更。

## 6. config 集成（引入 yaml.v3）

- 引入 `gopkg.in/yaml.v3`（纯 Go、零 CGO、成熟稳定）。
- `Load()` 职责扩为：**环境变量优先 + 可选配置文件覆盖**。配置文件路径：`SMALL_CONFIG` 指定，缺省 `~/.small/config.yml`；文件不存在则忽略（纯环境变量行为不变）。
- `Config` 新增 `SessionDir string`（默认 `~/.small/sessions`）。
- **红线保持**：文件/环境变量仍只在 `Load()` 一处读取，随后整个 struct 构造注入——不产生新隐式读取。

```yaml
# ~/.small/config.yml（可选）
model: deepseek-v4-pro
session_dir: ~/.small/sessions
```

## 7. 行业实践对比

| 方案 | 代表 | 优点 | 缺点 | 本项目取舍 |
|---|---|---|---|---|
| SQLite | Codex thread-store、Aider、Continue、OpenClaw | 事务/索引/SQL 查询/并发 | 依赖重，追加型历史用不上 90% 能力 | ❌ 砍掉 |
| bbolt/Badger | 众多小工具 | 单文件、事务、纯 Go | 二进制不可读、比 JSONL 重 | ❌ 暂缓 |
| **JSONL 每会话一文件** | DeepSeek Harness session log、Claude Code 早期 | 零依赖、人读、崩溃只丢半行、追加即日志 | 无查询、同会话并发写不支持 | ✅ **采用** |
| 全量覆写 JSON | — | 实现最简 | 写放大、崩溃丢整轮 | ❌ 不如 append |

**分界线**：要查询/并发 → SQLite；只要续聊 → JSONL。本项目会话量级（十几个）、单进程 CLI、读是全量读回，JSONL 是最优解。

**Thinking 取舍（市面共识）**：OpenAI o 系列 / DeepSeek / Anthropic 均不回传 reasoning 给模型。落盘内容 = 内存历史 = 只含 `Reply`；`Thinking` 是当轮展示物，不存。审计轨迹是未来独立需求（轨迹文件），第一版不做。

## 8. 落地路径

**阶段一（本模块，不碰 agent）**：`internal/session` Store + Message + 单测（round-trip 保真、坏行跳过、稳定排序、文件隔离、id 路径校验）。独立合入，不影响现有对话。

**阶段二（接入）**：
- agent：`New` 加 store 参数、`WithSession`/`WithHistory`、`persist.go` 翻译（内部 `toSessionMsgs` + 导出 `FromSession`）、Run 成功自动 append、`Reset() error` 语义；补 `persist_test.go`（集成续聊、Thinking 不入盘、落盘失败透传）。
- config：yaml.v3 + `SessionDir`（`SMALL_CONFIG` 指定配置文件，缺省 `~/.small/config.yml`）。
- main：组合根接线，`--session <id>` flag（缺省时间戳新会话），`store.Load` → `FromSession` → `WithHistory` 恢复。
- 更新 `CLAUDE.md` 目录结构与决策表、`路线图.md` 勾掉该项。

## 9. 测试设计与验收（完整度评估）

| 层 | 用例 | 判据 |
|---|---|---|
| 单测 | round-trip | 含 ToolCalls/ToolCallID 的 Message 落盘读回 `DeepEqual` 一致 |
| 单测 | 坏行容错 | 文件中间塞一行乱码，Load 跳过坏行、其余完整 |
| 单测 | 稳定排序 | 多会话 List 按 id 升序，重复调用结果确定 |
| 单测 | 文件隔离 | 不同 id 互不干扰；id 含 `/` 报错 |
| 集成 | 续聊 | 跑两轮→新 agent WithSession 恢复→再 Run→断言请求历史含前文 |
| 集成 | Thinking 不入盘 | 落盘文件内容不含 Thinking 字段 |
| 手工 | CLI | `--session x` 对话→exit→再 `--session x` 续聊上下文不丢 |

合并门槛：`go test -race ./...`、`go vet`、`gofmt` 全绿。

## 10. 待决问题

- 会话列表的 CLI 呈现（`/list`、`/use` 交互命令）——第一版仅 `--session` flag，交互命令后续评估。
- 坏行审计（跳过数统计）——第一版静默跳过，有需要再补。
- 自动恢复"最近会话"（不带 flag 时）——第一版缺省新建时间戳会话，自动恢复后续评估。
