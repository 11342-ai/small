# 项目约定与规范

> 本项目的自建规范（与通用 Go 风格不同处已标注）。核心原则：**依赖单向、解耦到隔离点、功能需求决定形态**。

## 核心目录结构

```
small/
├── main.go                 # 组合根：装配全部依赖 + 多轮对话 demo
├── internal/
│   ├── config/             # 配置：环境变量优先 + 可选 config.yml，构造注入下游
│   ├── provider/           # 传输层：DeepSeek HTTP/SSE 调用、重试、超时（含 retry 子包）
│   ├── agent/              # 领域层：多轮对话循环/历史/自动持久化/预算截断；adapter.go 翻译 provider
│   ├── session/            # 会话持久化：JSONL 每会话一文件，agent 的存储部件
│   ├── memory/             # 长期记忆：Markdown 文件 + bigram 关键词索引 + 归档层受控追加，存储部件
│   ├── kb/                 # 知识库：goldmark 解析 md 文件树 → 节点/边/反链 + 无环校验，存储部件（见 Zoo/model/kb.md）
│   ├── persona/            # 人格：go:embed 内置人格文件（frontmatter + 正文），提示词源部件
│   ├── policy/             # 权限：Pass/Ask 类型（命令与工具共享判定，安全护栏地基，见 cli.md §5）
│   └── tool/               # 工具：声明/执行/注册（Registry）+ 内置工具（builtin）
└── Zoo/                    # 设计文档、约定、踩坑记录
```

## 常用命令

```bash
go build ./...                      # 编译
go vet ./...                        # 静态检查
go test ./... -count=1 -race        # 测试（必须过 race）
gofmt -l .                          # 格式检查（无输出为干净）
```

### CLI 会话对话（多轮，自动持久化）

语法：`go run . [--session <id>] [--persona <name>]`

```text
--persona <name>
  - default / catton / scientist / onee / interviewer
```

| 参数 | 含义 | 缺省 |
|---|---|---|
| `--session <id>` | 会话 ID：恢复/续聊该会话，不存在则新建 | 时间戳新会话（如 `20260823-153045`） |
| `--persona <name>` | 对话人格：仅对新建会话生效；恢复会话时以会话 meta 为准 | `default` |

| 环境变量 | 含义 | 缺省 |
|---|---|---|
| `DEEPSEEK_API_KEY` | 鉴权密钥（必填） | 无 |
| `DEEPSEEK_MODEL` | 模型名 | `deepseek-v4-pro` |
| `SMALL_SESSION_DIR` | 会话存储目录 | `~/.small/sessions` |
| `SMALL_MEMORY_DIR` | 长期记忆目录（`MEMORY.md` + `memory/*.md`） | `~/.small/memory` |
| `SMALL_KB_DIR` | 知识库目录（md 文件树，见 `Zoo/model/kb.md`） | `~/.small/kb` |
| `SMALL_CONFIG` | 配置文件路径 | `~/.small/config.yml` |
| `SMALL_MAX_TOKENS` | 上下文预算（估算 token，显式 `0` 禁用截断） | `8000` |

示例：

```bash
go run . --session mychat          # 开始/续聊会话 mychat（聊几句后输入 exit 退出）
go run . --session mychat          # 再次进入，上下文还在
cat ~/.small/sessions/mychat.jsonl # 落盘文件人读可查（JSONL，一行一条消息）

cloc . --not-match-f='_test\.go$'
cloc . --not-match-f='_test\.go$' --not-match-d='^\.'      # 同时排除隐藏目录（如 .git/.idea）
cloc . --include-lang=Go --not-match-f='_test\.go$'         # 只看 Go，忽略 md/yml 等
cloc . --by-file --not-match-f='_test\.go$'                 # 逐文件明细
```

## 代码风格与约定

- **注释中文**，写"为什么"多于"是什么"（决策理由进注释，别只写名词解释）。
- **接口定义在使用方**（accept interfaces, return structs）；构造函数参数按需接接口（本项目 adapter 接 `provider.Completer`，因为降级探测要求多态）。
- **函数式选项扩展**（`Option`/`AdapterOption`）：**结构协作对象显式入参**（如 `agent.New(chat, tools, …)`、adapter 收冻结的声明列表），行为开关走 Option（只增不改签名）。
- **编译期断言**：`var _ Interface = (*Impl)(nil)` 锁住实现，签名漂移编译期暴露。
- **nil-safe 回调**：回调字段可为 nil，调用前判 nil。
- **错误处理**：只在系统边界（HTTP 响应、外部输入）防御；内部契约（如"err 非 nil 时 resp 为 nil"）信任不重复防御；判等用 `errors.Is/As`。
- **DTO 打包**：请求/响应 struct 化；内部字段（如 `stream` 开关）不进对外 DTO（用内嵌 `chatPayload` 注入）。

### 新增内置工具的标准流程（工具声明 + 权限声明）

> 新增工具必须走完整五步，缺一不可（权限声明是硬性一步，防"写工具裸奔"）：

1. **一工具一构造函数**：`builtin/xxx.go` 写 `Xxx(cfg) tool.Tool`（对齐 `Echo()`/`FileRead()` 同款）；执行逻辑超 10 行外置具名函数 `runXxx` + 一行转发闭包（可脱离工具壳单测）。
2. **注册**：`builtin/register.go` 追加注册元素；需要新依赖类型时扩 `Deps` struct 字段（不破签名）。
3. **登记权限**：`builtin/tool_permissions.go` 的 `ToolPermissions` 表登记该工具——**写/执行类工具 = `policy.Ask`，只读 = `policy.Pass`**（全量列举；漏登记由 `RegisterBuiltins` 注册后校验在注册期暴露，fail-fast）。
4. **提示词**：`main.go` base 字符串补工具说明（触发约束一并写清）。
5. **验证**：单测 + 合并门槛（`go test -race ./...` / `go vet` / `gofmt`）全绿。

权限语义（见 `Zoo/model/policy.md`）：Ask 工具的确认由 agent 权限层统一提供（`WithToolConfirm`，组合根注入），**工具内部不再写确认逻辑**——新增 Ask 工具只需登记权限表，确认交互零代码。

## 架构与模块边界

- **依赖严格单向**：`main → agent → provider → config`，`agent → session`，`agent → tool`，`agent → policy`，`tool/builtin → policy`，`provider → retry`（子包），`tool/builtin → memory`（叶子）。禁止反向/循环依赖。
- **职责边界**：
  - `provider`（传输层）：只做 HTTP 语义，不感知业务。
  - `agent`（领域层）：只管循环/历史/自动持久化，不感知 provider DTO。
  - `adapter.go`（隔离点）：**全项目唯一** import provider 的 agent 文件，翻译 `Turn↔Message`、探测流式。
  - `session`（存储部件）：不 import agent（避免循环），自持 `Message` 模型，`Turn↔Message` 翻译在 agent 的 `persist.go`。
  - `memory`（存储部件）：bigram 关键词检索 Markdown 记忆文件，不 import 任何内部包；写入双层——`MEMORY.md` 人手维护、归档层 `memory/*.md` 可经 `Append` 受控追加（锁死只写归档层）。
  - `config`：集中配置，构造注入。
- **internal/ 语义**：应用非库，内部实现不对外导出；`retry` 扁平放在 `provider/retry`（不套 internal 嵌套）。

## 红线与硬性规则

1. 禁止包级可变全局状态；禁止隐式 `os.Getenv`（环境变量只在组合根读一次，注入下游）。
2. 禁止引入 DI 框架（uber/fx、wire 等）——构造注入 + 组合根足够。
3. **流式"拿到 2xx 后绝不重试"**——否则重复已吐出的 token。
4. **流中失败不回退非流式**（adapter 层同样）——失败只能透传错误，由调用方重试整个对话。
5. API key 等机密**绝不入库**（提交前 grep 检查）。
6. `builtin` 生产代码只允许 import `tool`/`memory`/`policy`，**不得反向依赖** `agent`/`session`/`provider`/`config`（`imports_test.go` 固化；测试文件不受限，可自由 import 做集成验证）。
7. 合并门槛：`go test -race ./...`、`go vet`、`gofmt` 全绿。

## 重要决策与取舍

| 决策点 | 结论 | 一句话理由 |
|---|---|---|
| 配置获取 | 构造注入（弃全局单例） | 依赖显式、可 mock、无初始化顺序坑 |
| 接口组织 | 双层能力接口 `Completer ⊂ Streamer` | 消费方按最小能力依赖，类型断言降级 |
| 流式形态 | 回调 struct（nil-safe） | 错误传播自然，免 channel 管理 |
| 重试边界 | `retry.Do` 只包建连 | 2xx 后零重试，防重复 token |
| 流式超时 | 去掉整请求 Timeout，改空闲超时 + ctx | 长生成不被误杀 |
| 重试状态码 | 408/429/5xx（529 属 5xx） | 529 是 Cloudflare 私有扩展码 |
| adapter 参数 | `provider.Completer` 接口 | 降级探测要求字段多态（功能决定形态） |
| 工具注入形态 | 结构依赖显式入参（agent 收注册表、adapter 收冻结声明），行为开关走 Option | 构造契约可见协作对象；配置类开关不破签名 |
| 会话持久化 | JSONL 每会话一文件 + Run 成功自动 append（不引 SQLite） | 零依赖、崩溃只丢半行、续聊场景够用；要查询再迁 |
| 会话恢复 | 组合根 `store.Load` + `WithHistory` 注入 | New 不返回 error，文件错误属系统边界，组合根 fail fast |
| 配置来源 | 环境变量优先 + 可选 `~/.small/config.yml`（yaml.v3）；机密只走环境变量 | 配置项增长后可持久化，API key 不落配置文件 |
| 上下文压缩 | 第一版只做"预算截断 + 真实 usage 校准"（`WithTokenBudget`），摘要后续增强 | 真实 usage（流式 `include_usage`）优先、字符估算兜底；截断同步重写（Rewrite）防"复活" |
| 输出形态 | `Result{Reply, Thinking}` struct | 类型安全、可扩展 |
| thinking | `WithThinking` 默认关 | 显式开启才付代价 |
| 记忆存储 | Markdown 文件 + bigram 关键词索引（零依赖） | 中文无空格必须分词，bigram 免词表；语料量级不到，不上向量/SQLite FTS5 |
| 记忆访问 | 双轨：`MEMORY.md` 启动注入 + `memory_search`/`memory_get` 工具按需检索 | 模型不会自觉想起检索，注入兜高频事实；按需检索省常驻 token |
| 记忆写入 | 双层：`MEMORY.md` 人手维护 + `memory_save` 只写归档层（仅用户显式要求时触发） | 模型写的进"搜索池"不污染常驻上下文；去重/事实性风险由触发约束缓释 |
| 工具观测 | `WithToolObserver` 回调（nil-safe），Run 每实际执行一个工具触发一次（名称/入参/结果） | 展示层实时渲染，不改循环逻辑；将来"工具轨迹"可复用同一事件源 |
| 人格来源 | `personas/*.md`（frontmatter + 正文）+ `go:embed` 编译期快照 | 人格是代码资产（对照 memory 是用户可编辑文件）；零运行时失败 |
| 人格切换语义 | 一个对话一个人格（创建时定型，不中途切）；恢复以会话 meta 为准 | 缓存约束不成立（本项目规模收益可忽略）；旧人格回复留历史会串味 |
| 会话 meta | JSONL 首行 `{"meta":{...}}`，旧文件兼容（无头行照常读），Rewrite 保头 | 记录人格绑定且不改消息行格式 |
| 提示词装配 | `persona.Compose(base, p, memory)` 三段式唯一装配入口（契约→人格→记忆） | 字段（first_message/examples）缺省零回归；main 不再手拼 |
| 工具权限 | 静态表 `builtin.ToolPermissions`（按工具名，**不进 `Tool.Spec`**）+ agent 工具循环统一拦截（`WithToolPermissions`/`WithToolConfirm`） | Spec 序列化给模型，内部策略不污染模型视角；Ask 无确认回调即拒绝（fail-closed 由 agent 硬约束） |
| 权限类型 | 只 `Pass`/`Ask`（与命令层共用同一 policy 类型） | 横切不分裂；Deny/动态覆盖留位（cli.md §5） |
