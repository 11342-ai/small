# Memory（长期记忆）模块设计文档

> 位置：`internal/memory`
> 状态：**设计稿（已实现 + 记忆写入增强）**——对齐 roadmap 第二梯队第 3 项（长期记忆，简化版）。
> 关联：`tool.md`（复用工具三件套抽象）、`session.md`（存储部件同构）、`compaction.md`（会话内预算截断；记忆是跨会话的）、`CLAUDE.md`。
> 决策基调：**零新增依赖**；存储用 Markdown 文件，检索用关键词（bigram + 子串兜底，不上向量）；**写入双层**——常驻层 `MEMORY.md` 人手维护，归档层 `memory/*.md` 可由 `memory_save` 受控追加（仅用户显式要求时，锁死只写归档层）。

## 1. 定位与职责

memory 是 agent 的**跨会话存储部件**（session 管"会话内历史"，memory 管"会话间长期事实"）：把磁盘上的 Markdown 记忆文件变成可检索的能力，暴露给模型按需查询。

- **记忆 = 磁盘上的 Markdown 文件（唯一事实来源）**。与 OpenClaw 同款理念："模型只记住落盘的内容，不存在隐藏状态"。
- **本模块做四件事**：扫描文件 → 分块建索引 → 关键词检索 → 受控追加（`memory_save` 只写归档层）。
- **明确不做**（本阶段）：向量/语义检索、自动蒸馏、模型自主写入（仅显式要求时受控写入）、auto-recall 每轮注入、会话 JSONL 索引——规模不匹配，见 §2、§9。

### 依赖方向（叶子包）

```
main → agent → tool
        ↘ builtin → memory（叶子，只依赖 stdlib）
```

- `memory` 不 import 任何内部包（同 `session` 的定位），故不产生环。
- 记忆工具（`memory_search`/`memory_get`/`memory_save`）是 `builtin` 的一员，构造时注入 `*memory.Store`——结构依赖显式入参。

## 2. 为什么是"文件 + 关键词"，而不是向量/数据库

### 2.1 行业四派（记忆如何进上下文）

| 派别 | 代表 | 优点 | 缺点 |
|---|---|---|---|
| 启动注入（bootstrap） | Claude Code `CLAUDE.md`、Codex `MEMORY.md`、OpenClaw `MEMORY.md` | 零依赖、人可读、模型必见 | 有大小预算、每轮常驻 token |
| 工具按需检索（pull） | OpenClaw `memory_search/get`、Codex `memory search` | token 只在需要时花、可索引海量文件 | 依赖模型自觉调用（需强 prompt） |
| 自动注入（push） | OpenClaw hybrid 插件、Mem0 | 模型无需自觉 | 每轮嵌入成本、上下文膨胀、误判污染 |
| 会话日志即记忆 | DeepSeek Harness、Codex rollout | 零额外设计 | 只能 grep 回看，无提炼 |

### 2.2 检索技术谱系

| 手段 | 优点 | 缺点 |
|---|---|---|
| 关键词/全文检索（BM25、FTS5） | 离线、零成本、精确匹配标识符/报错串/配置键、可解释 | 无语义（同义改写召回差）；**中文必须分词** |
| 向量检索 | 语义召回 | 需嵌入模型/API、索引维护、不可解释 |
| 混合 + RRF 融合 | 语义+精确兼顾（OpenClaw 默认） | 复杂度最高、依赖最多 |
| 图记忆（知识图谱） | 实体关系推理（Mem0/Zep） | 严重 over-engineering |

### 2.3 本项目取舍

**选"工具按需检索（pull）+ 启动注入（bootstrap）"双轨**：

- 双轨是 OpenClaw 同款双层：`MEMORY.md` 启动注入兜"高频事实必见"（模型不会自觉想起检索），`memory_search` 按需深挖"低频细节"。
- 检索技术选**纯关键词**：语料量级（几百个 md、几 MB）下毫秒级，且关键词对"代码标识符、报错串、专有名词"这类记忆恰恰是**最准**的——语义检索的优势（同义改写）在当前语料量级用不上。
- **分界线**：要"同义改写/模糊语义召回" → 向量；要"精确命中" → 关键词。本项目当前是后者，向量等语料大了再评估（路线图已注明）。
- **砍掉的部分**（对照 OpenClaw 全量能力）：向量/混合检索、memoryFlush（压缩前强制写盘）、后台蒸馏、会话记忆搜索、模型自主写入——保留受控写入 `memory_save`（只写归档层、仅显式要求时触发），规避"模型自主判断写入"的噪声风险。

## 3. 文件布局与数据模型

```
<memoryDir>/                       # 默认 ~/.small/memory，由 config.MemoryDir 提供
├── MEMORY.md                      # 长期记忆：精炼事实/偏好/决策，启动时注入 system prompt
└── memory/*.md                    # 主题/日期分文件：详细笔记、背景上下文，仅索引可检索
```

- **双层语义**：`MEMORY.md` 是"常驻层"（小、精、启动必见，人手维护）；`memory/*.md` 是"归档层"（大、杂、按需检索，人写 + `memory_save` 受控追加）。二者分工清晰，与 OpenClaw 一致。
- **双层写入模型**：常驻层**只许人手写**（保护高频上下文不被污染）；归档层可由 `memory_save` 追加——**锁死在 `memory` 包层**：`Append` 只写 `memory/*.md` 子目录，工具层无从选择写入常驻层。模型生成内容的去重/事实性问题由"只写归档层 + 仅显式要求触发"两重约束缓释（详见 §6）。
- **分块（chunk）**：索引的最小单元。按 `##` 标题段或固定行数切块，每块带 `Ref`（文件相对路径 + 块序号），供定位与 `memory_get` 精读。
- **惰性刷新**：每次 `Search` 前按文件 mtime 比对，仅重新索引改动文件（最简实现可全量重扫——语料小，毫秒级，无需复杂缓存）。`Append` 落盘后下一次 `Search` 自动可见，写入与检索天然解耦。

## 4. 检索算法：bigram + 子串兜底 + 轻量 BM25

### 4.1 为什么 bigram

中文没有空格，朴素"按空白分词"对中文几乎全失效（一句"我们决定用 JSONL"会被切成一整个词）。**bigram（二元组）** 把每个块切成相邻两个字符的滑动窗口组合（"我们""们决""决定"…），零依赖、免维护、无需词表，对中文检索是"够用"的最简解。

### 4.2 索引结构

```
扫描 → 分块 → 每块做 bigram 切分 → 倒排表 term → [chunk...]
```

- 倒排表：`map[string][]chunkID`（term → 包含它的块）。
- 查询同样切成 bigram，取各 term 命中块的并集，按分排序取 top-N。

### 4.3 评分

- 主评分：轻量 **BM25 变体**（TF × IDF，无文档长度归一化的简化版即可）——让"出现多次、语料中稀有"的词贡献更高分。
- **子串兜底**：查询整体（及按空格/标点粗切出的短语）做包含匹配，专门兜住 bigram 切不中的场景——英文单词/代码标识符、长专有名词（如 `memory_search`、`DEEPSEEK_API_KEY`）。这类"精确串"恰恰是记忆检索的高频需求。
- 排序稳定：同分按 Ref 字典序，保证重复调用结果确定（同 session.List 的惯例）。

### 4.4 明确不做（本阶段）

同义改写、拼写纠错、停用词表、embedding——全部留给未来的向量层（若上，见 §9"升级路径"）。

## 5. 对外 API（模块对象，不接口化）

```go
func New(dir string) (*Store, error)              // 建目录（不存在则创建），返回模块对象

func (s *Store) Search(query string, limit int) ([]Hit, error)
// 关键词检索：bigram 倒排 + 子串兜底，按分降序取前 limit（>0），同分按 Ref 稳定排序。
// query 为空返回业务失败（参数错误），不返回框架错误。

func (s *Store) Get(ref string) (string, error)   // 按 Ref 读回整块原文（memory_get 用）
// ref 非法/块不存在返回业务失败；目录不可读等系统边界才返回 error。

func (s *Store) Append(name, content string) error // 追加到归档层 <dir>/memory/<name>.md（memory_save 用）
// name 须为纯文件名（不含路径分隔符/扩展名，防路径注入），content 原样追加为一段。
// 只写归档层是设计约束：常驻层 MEMORY.md 保持人手维护，本方法无任何写入路径可达它。

type Hit struct {
    Ref     string // 文件相对路径 + 块序号，如 "memory/20260823.md#2"
    Score   float64
    Snippet string // 块内命中片段（上下文窗口），供模型快速判断相关性
}
```

- **不接口化**：与 `session.Store`/`Registry` 同理由（tool.md §4.2）——唯一实现、无第二形态可替换，接口是空抽象；将来换 SQLite FTS5/向量时再评估。
- **并发**：单进程 CLI，Search 前惰性刷新即可；内部 `sync.Mutex` 保护索引重建，保证 `-race` 干净。
- **错误分层**：无结果、参数非法、ref 非法 = **业务失败**（`Result.IsError`，由模型自行换词重试）；目录损坏、不可读 = **框架错误**（error 透传中止）——沿用 tool 包边界约定。

## 6. 工具暴露：memory_search / memory_get

### 6.1 工具声明（JSON Schema）

```json
{
  "name": "memory_search",
  "description": "在长期记忆文件（MEMORY.md + memory/*.md）中按关键词检索相关片段，返回 top-N 命中（含文件位置）。回答涉及先前决策/偏好/待办/项目事实前应优先调用。",
  "parameters": {
    "type": "object",
    "properties": {
      "query":  {"type": "string", "description": "检索关键词，可用空格分隔多个词"},
      "limit":  {"type": "integer", "description": "最多返回条数，默认 5"}
    },
    "required": ["query"]
  }
}

{
  "name": "memory_get",
  "description": "读取 memory_search 返回的完整块原文（按 Ref），用于需要完整上下文的场景。",
  "parameters": {
    "type": "object",
    "properties": {
      "ref": {"type": "string", "description": "memory_search 返回的 Ref"}
    },
    "required": ["ref"]
  }
}

{
  "name": "memory_save",
  "description": "把一段内容追加进长期记忆的归档层（memory/YYYY-MM-DD.md，可被后续 memory_search 检索，不注入常驻上下文）。仅在用户明确要求记住某事时调用；写入内容应为用户原话或已确认的事实。",
  "parameters": {
    "type": "object",
    "properties": {
      "topic":  {"type": "string", "description": "本条记忆的标题（自动转成 ## 标题，缺省'备忘'）"},
      "content": {"type": "string", "description": "要记住的内容"}
    },
    "required": ["content"]
  }
}
```

### 6.2 放置与注册

- `internal/tool/builtin/memory.go` 提供三个独立构造函数（与 `Echo()` 同款"一工具一构造函数"）：`MemorySearch(mem)` / `MemoryGet(mem)` / `MemorySave(mem)`，`RegisterBuiltins(reg, mem)` 显式聚合。
- `RegisterBuiltins` 签名：`RegisterBuiltins(reg *tool.Registry, mem *memory.Store) error`——**结构协作对象显式入参**；`mem` 为 nil 则不注册记忆工具（退化，纯对话不受影响，同 `tools`/`store` 传 nil 的惯例）。
- 实现走 `tool.NewFunc`，零新抽象；解码失败/无结果按 `Result{IsError: true}` 回灌。
- **写入触发约束**（防模型自作主张写垃圾）：`memory_save` 的 Description + system prompt 双重限定"仅在用户明确要求记住时调用"，配合归档层隔离，模型写得再烂也不污染常驻上下文。

## 7. 双轨注入（启动注入 + 工具检索）

- **启动注入**：`main.go` 启动时读取 `<memoryDir>/MEMORY.md`，拼进 `agent.WithSystemPrompt`——**agent 零改动**（system prompt 本就是组合根传入的字符串）。
- **budget 上限**：`MEMORY.md` 建议上限 2KB，超出则截断并在注入内容末尾注明（完整内容仍可通过 `memory_search` 检索）——防常驻 token 膨胀。
- **按需检索**：模型侧靠工具声明 + system prompt 一句"回答涉及先前决策/偏好/待办前先 memory_search"驱动（模型自觉调用，pull 模式）。
- 为什么必须双轨：只做工具检索，模型会"想不起来查"；只做启动注入，记忆容量受常驻预算锁死。二者互补。

## 8. config 集成

- 新增环境变量 `SMALL_MEMORY_DIR`（缺省 `~/.small/memory`），`Config` 加 `MemoryDir string` 字段。
- 沿用红线：环境变量只在 `Load()` 一处读取，随后 struct 整体构造注入；`expandHome` 处理 `~`。

```yaml
# ~/.small/config.yml（可选追加）
memory_dir: ~/.small/memory
```

## 9. 影响面与依赖检查

| 模块 | 影响 | 依据 |
|---|---|---|
| `internal/memory`（新） | 纯新增，零影响 | 叶子包，只依赖 stdlib；与 session 同构 |
| `internal/tool/builtin` | **主要改动面**：新增 `memory.go`；`RegisterBuiltins` 加 `mem` 参数；`register_test.go` 同步改 | 工具需访问存储实例，"结构依赖显式入参"红线 |
| `internal/config` | 加 `MemoryDir` + `SMALL_MEMORY_DIR`，补测试 | 环境变量只在组合根读一次 |
| `main.go` | 装配 `memory.New` → 注入 `RegisterBuiltins`；读 `MEMORY.md` 拼 system prompt | 组合根职责 |
| `agent` / `session` / `provider` | **零改动** | 工具循环已实现（agent.go `maxToolRounds` + 回灌）；memory 不被这三个包引用 |

**依赖方向验证**：`main → builtin → {tool, memory}`；`memory` 不 import 任何内部包——无环、不违反单向红线、不感知 provider。

**升级路径（量级到了再评估，本期不做）**：
- 语料超千级 → 索引落盘/缓存（避免全量重扫）；
- 需要语义 → 换 SQLite FTS5（零成本 BM25，仍需分词）或上 embedding（本地 ollama/bge-m3 免 API key）；
- 需要混合 → BM25 + 向量 + RRF 融合。

## 10. 测试设计与验收

| 层 | 用例 | 判据 |
|---|---|---|
| 单测 | 分块 | `##` 标题段切分正确，Ref 稳定可定位 |
| 单测 | bigram 切分 | 中文查询命中"我们决定用 JSONL"类句子 |
| 单测 | 子串兜底 | 查询 `memory_search` 命中含该标识符的块（bigram 切不中时） |
| 单测 | 评分排序 | 多词查询按分降序、同分字典序，重复调用结果确定 |
| 单测 | 容错 | 损坏/空文件跳过不报错；目录不可读返回 error |
| 单测 | Append | 追加进归档层 `memory/*.md`；name 含路径分隔符报错（防注入）；写入后 Search 立即可见 |
| 集成 | 工具回灌 | 模型调用 memory_search → 结果以 tool role 回灌 → 下一轮正常 |
| 集成 | memory_save | 模型调用 memory_save 落盘 → 后续 memory_search 能检索到新内容；常驻层 MEMORY.md 不被写 |
| 集成 | 双轨注入 | 启动时 MEMORY.md 出现在请求的 system 消息里；超限截断 |
| 手工 | CLI | 手写 `~/.small/memory/MEMORY.md` → 新会话问"我之前定的 X" → 模型先检索再答 |

合并门槛：`go test -race ./...`、`go vet`、`gofmt` 全绿。

## 11. 落地路径

**阶段一（本模块，不碰 agent）**：`internal/memory` Store + 分块 + bigram 索引 + 检索 + 单测。✅ 已完成。
**阶段二（接入）**：`builtin/memory.go` 工具 + `RegisterBuiltins(reg, mem)` 签名变更 + main 装配 + `MEMORY.md` 启动注入；补 register_test 与集成测试。✅ 已完成。
**阶段三（收尾）**：config 字段与测试；更新 `CLAUDE.md` 目录结构与决策表、`路线图.md` 勾掉该项。✅ 已完成。
**阶段四（记忆写入增强）**：`memory.Append`（锁死只写归档层）+ `memory_save` 工具 + system prompt 触发约束。✅ 已完成。

## 12. 需要补的知识（同频清单）

要和我做出同等判断、以及将来评审实现，建议你补齐以下知识（按优先级）：

1. **BM25 / TF-IDF 检索原理**——理解"关键词检索能做到什么程度"，以及它和 `strings.Contains` 的差距；为什么"词频 × 稀有度"是有效打分。
2. **中文分词（bigram vs 词典分词）**——为什么中文检索必须先分词；bigram 的代价（召回一般）与词典分词（sego/jiebago）的代价（依赖+词表）。
3. **SQLite FTS5**——零依赖升级路径：纯关键词 + BM25 的正式实现，量级到了可无缝替换我们的内存索引。
4. **嵌入检索的基本原理 + 本地嵌入选项**（ollama / bge-m3）——用于判断"何时该上语义"，现在只需知道概念、不选型。
5. **注入派 vs 检索派的手感差异**——亲身体会 OpenClaw（检索为主）、Codex / Claude Code（注入为主）的记忆行为，理解为什么本项目选双轨。
6. **RRF（倒数排名融合）**——仅当决定上混合检索时才需要，现在可跳过。

## 13. 参考资料

- [OpenClaw 记忆文档](https://openclawcn.com/docs/concepts/memory/) / [Memory Search 参考](https://docs.openclaw.ai/fr/concepts/memory-search) / [OpenClaw 永久记忆系统技术解析](https://blog.csdn.net/u013261578/article/details/157968557)
- [openclaw-memory：SQLite FTS5 架构](https://github.com/jacklevin74/openclaw-memory)（纯关键词路线参考）
- [OpenClaw hybrid-memory 运行时流程](https://markus-lassfolk.github.io/openclaw-hybrid-memory/HOW-IT-WORKS)（混合 + RRF 参考，本期不做）
- [LanceDB：OpenClaw 记忆层选型分析](https://www.lancedb.com/blog/openclaw-lancedb-memory-layer)（向量路线选型参考，本期不做）
