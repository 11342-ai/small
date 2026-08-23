# 会话总结：Tool 模块（阶段一 + 阶段二）

> 时间：2026-08-22
> 范围：`internal/tool` 模块 + provider DTO 扩展 + agent 工具循环适配 + 组合根接线
> 状态：最小可行实现完成，`gofmt` / `go vet` / `go test -race` / `go build` 全绿
> 关联：`Zoo/tool.md`（模块设计）、`Zoo/agent.md` / `Zoo/provider.md`（架构）、`CLAUDE.md`（约定）

## 1. 本会话解决的问题与边界

### 1.1 问题

为 DeepSeek 多轮对话 CLI 添加**工具调用（function calling）**能力：新建 `tool` 模块承载"声明 / 执行 / 注册"三要素，并把工具调用贯通 provider（传输层）→ adapter（隔离点）→ agent（领域层循环）→ 组合根（装配），全程保持项目既有架构原则：**依赖单向、解耦到隔离点、功能需求决定形态**。

### 1.2 问题边界

| 层 | 职责 | 明确的"不做什么" |
|---|---|---|
| `tool` 包 | 领域层：Spec / Execute / Registry，工具契约 | 不 import provider（不感知任何传输层 DTO） |
| `provider` | 传输层：只加 `tools` / `tool_calls` / `tool_call_id` 等 DTO 字段 | 不感知工具语义（纯数据结构） |
| `agent/adapter.go` | 隔离点：全项目唯一 import provider 的 agent 文件，双向翻译 | 翻译只发生在这里，循环层不碰 DTO |
| `agent` | 领域层工具循环（执行 + 回灌 + 轮数兜底） | 不感知 provider DTO |
| 组合根（main） | 装配 + 调用 `builtin.RegisterBuiltins` 注册入口 | 不逐个枚举具体工具 |

依赖方向：

```
main → agent → provider → config
            ↘ tool（agent 工具循环依赖；工具实现在组合根注册）
```

**明确不做（YAGNI）**：参数运行时 schema 校验、工具分类骨架、MCP 进程边界、插件内核（Cordis 式）、多层策略管道、沙箱。

## 2. 分歧与结论

### 2.1 Registry 是否接口化

- **分歧/困惑**：用户问"不接口化是什么意思？接口不是要声明 Execute 吗？"——把工具能力接口与容器抽象混为一谈。
- **澄清**：接口分两个层级——① 工具能力接口（`Tool`：声明 + 执行），因实现多样（内置 / 未来 MCP 适配器 / 测试桩）**必须接口化**；② 容器抽象（是否把 Registry 再包一层接口），**不必要**。
- **结论**：`Tool` 接口化，`Registry` 具体结构体。
- **决策依据**：接口的价值在于承载"多种实现可替换"；Registry 是唯一容器，无第二实现，接口是空抽象；项目先例——`provider.Completer` 因能力差异（流式/非流式）接口化，而 `retry.Policy`、`config.Config` 都是具体结构体。

### 2.2 工具注入形态：显式参数 vs Option（本会话最大分歧）

- **分歧**：我最初设计用 `WithTools(reg)`（agent）+ `WithToolRegistry(reg)`（adapter）两个 Option 注入。用户质疑：结构依赖藏在 Option 里看不清依赖关系，主张 tools 注册表**直接作为 `agent.New` 的参数**传入；并以 Spring 注入 redis/mysql 为例证。
- **我的判断（含对例子的纠正）**：用户方向正确，但 Spring 例子有常识性错误——Spring 的招牌是**隐式装配**（容器 + 自动注入），用它论证"看清依赖关系"自相矛盾；且 Spring 不做"解包"，是注入整个对象、调其公开 API。真正站得住的论据是**项目自身先例**（`provider.New(cfg)` 把配置显式入参）与**构造器注入原则**。
- **结论**：`agent.New(chat Completer, tools *tool.Registry, opts ...Option)` 显式收注册表；adapter `NewProviderChat(client, model, specs []tool.Spec, opts ...)` 收**冻结的声明列表**（只序列化不执行）；行为开关（thinking / system prompt）走 Option。
- **决策依据**：**结构依赖显式入参、行为开关走 Option**；功能需求决定形态（工具调用改变了循环形态）；依赖在构造契约可见。
- **连带影响**：CLAUDE.md 规则"Option 只增不改签名"修订为"结构协作对象显式入参、行为开关走 Option"——原规则的前提（纯扩展性）因功能变化而失效，属合理修订而非违约。

### 2.3 内置工具分类骨架

- **分歧/关切**：用户想在 `builtin` 下按 files / web 建子目录分类，担心每类都要写 register.go 造成重复样板。
- **结论**：分类是行业通例（LangChain toolkit、Codex handlers、OpenClaw 类别），但"重复样板"可通过**分类包只导出纯函数 `All() []tool.Tool`、注册副作用收敛到顶层一个 register.go** 解决；当前仅 1 个工具**不建**（YAGNI），约 5 个且领域边界清晰时再切分。
- **决策依据**：显式优于隐式（否决 `init()` 自注册，因其违反"禁止包级可变全局状态"红线）；切分是可逆的半小时重构，不值得提前做。

### 2.4 两个 `RegisterAll` 同名

- **分歧/困惑**：`(r *Registry) RegisterAll(ts ...Tool)`（批量原语，方法）与 `builtin.RegisterAll(reg)`（内置入口，包函数）签名不同但同名，易误以为重复。
- **结论**：builtin 侧改名 `RegisterBuiltins`。
- **决策依据**：命名即契约，同一语义场必须消歧。

## 3. 踩坑记录（忽视临界情况的代价）

### 3.1 测试替实现脑补语义（阶段一）

- **现象**：`TestEcho_Execute` 假设 `{}`（缺字段）会报解码错误；实际 `{}` 是合法 JSON，解出空串、`IsError=false`。
- **根因**：echo 不做运行时 schema 校验（YAGNI 决策），缺字段不是错误路径。
- **解决**：修正测试断言为"缺失字段回显空串"，注释锁定语义。
- **教训**：测试别替实现脑补边界语义；明确断言再写。

### 3.2 系统提示不持久化（阶段二测试）

- **现象**：`TestAgent_RunPlainChat` 断言 `History()` 含 system 提示，实际不含。
- **根因**：系统提示只在请求时经 `allTurns` 注入，不存进 history（agent 既有设计）。
- **解决**：修正断言，并顺带补测"思考过程不入历史"。
- **教训**：写测试前先确认领域层真实契约，而非直觉。

### 3.3 误撤销事故（本会话最严重）

- **现象**：用户 IDE 误点撤销，静默回退多个文件——CLAUDE.md（两条规则修订）、`agent_test.go`（整文件回退到中途状态，且出现无法编译的乱码 `y: "two"}}}`）、`adapter_test.go`（多处 `NewProviderChat` 参数退回旧签名）、`tool.md`（"接线形态"条目丢失）。
- **解决**：`git status` 盘点受损面 → 作者文件（`agent_test.go`）整文件重写 → `adapter_test.go` 批量修复调用点 → 重补 CLAUDE.md / tool.md → 全量验证。
- **教训**：
  1. **代码未提交 git 时，IDE 撤销没有保险**——本次全部改动仍未提交，是重大流程缺口；
  2. 撤销可能**静默回退文档**（markdown 不在编译检查范围内），靠编译/测试发现不了，文档恢复需人工逐条核对；
  3. 事故后应先盘点受损面（git status + grep 关键签名），再逐文件修复，而不是盲目重做。

### 3.4 流式 tool_calls 分片（设计层面提前规避，未踩坑）

- **临界情况**：流式调用中 ID/Name 只在首个分片出现、Arguments 分片需按 index 拼接、稀疏 index（跳号）可能出现。
- **处理**：adapter 按 index 累积（`id/name/args`），空累积跳过；用"多 index 交错分片"测试锁住拼接与顺序。
- **结果**：测试先行，未踩坑。

## 4. 后续开发的统一与连贯性

以下条目为后续开发硬性对齐项（核心已写入 CLAUDE.md）：

1. **依赖严格单向**：`main → agent → provider → config`；`agent → tool`；**tool 与 provider 互不感知**。禁止反向/循环依赖。
2. **隔离点**：`adapter.go` 是 agent 包唯一 import provider 的文件，所有 provider DTO 翻译只发生在这里。
3. **结构依赖显式入参、行为开关走 Option**：新增一个"协作对象"（注册表、客户端、配置）就进构造函数签名；新增"行为开关"（thinking、轮数、超时）才走 Option。
4. **接口定义在使用方 + 编译期断言**：`var _ Interface = (*Impl)(nil)` 锁住实现。
5. **中文注释写"为什么"**：决策理由进注释，不只写名词解释。
6. **错误处理**：只在系统边界防御；判等用 `errors.Is/As`；工具**业务失败回灌、框架错误中止**（`Result.IsError` 语义）。
7. **测试**：`-race`、确定性优先、降级路径必须有 mock 锁住、边界路径必测。
8. **文档先行**：新模块 / 新扩展先更新 Zoo 文档再动代码。
9. **新增内置工具流程**：`builtin/` 下加工具文件 → `RegisterBuiltins` 列表追加一个元素 → main 与顶层注册点**不改**。
10. **节点提交 git**：每个"最小可行实现"里程碑即为提交点（本会话教训）。

## 5. 遗留问题与妥协

| 遗留项 | 类型 | 触发条件 / 后续方向 |
|---|---|---|
| 参数运行时 schema 校验 | YAGNI 妥协 | 模型频繁吐非法参数时，在执行前挂可选校验钩子 |
| 工具分类骨架（files/web 子包 + `All()`） | 延迟决策 | 工具约 5 个且领域边界清晰时切分 |
| MCP 适配器 / 动态注册 | 延迟决策 | 需接外部工具时，在 `Tool` 接口下加适配器，接口不变 |
| 历史截断 / `finish_reason` 暴露 | agent 扩展点 | 超长对话防上下文溢出；感知 `max_tokens` 截断 |
| `maxToolRounds=8` 硬编码 | 简化妥协 | 需要时提升为 Option |
| 端到端真实模型验证 | 未做 | httptest 已覆盖全链路，建议用真实 API 冒烟一次（`go run .` 触发 echo） |
| **代码未提交 git** | 流程缺口 | 立即建立节点提交习惯（本次会话最重教训） |

## 6. 参考文档

- `Zoo/tool.md`（模块设计：核心抽象 / 边界约定 / 接口拆分 / 行业对比 / 落地形态）
- `Zoo/agent.md`（领域层设计，含工具循环接入后的扩展点更新）
- `Zoo/provider.md`（传输层设计）
- `CLAUDE.md`（项目约定与规范，本会话修订"结构依赖入参、行为开关走 Option"）
