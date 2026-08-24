# Persona（人格）模块设计文档

> 位置：`internal/persona`
> 状态：**设计稿（未实现）**——对话多样性需求，创建对话时固定风格。
> 关联：`session.md`（头行 meta 记录人格）、`compaction.md`（system 加长与预算）、`agent.md`（零改动）、`CLAUDE.md`、`路线图.md`。
> 决策基调：**一个对话 = 一个人格**（创建时固定，存续期内不切换）；缓存顾虑**不构成产品约束**（本项目规模收益可忽略，见 §2.1 纠偏记录）；人格是代码资产 → `go:embed` 编译期快照；**`agent` 零改动**（system prompt 本就是组合根注入的字符串）。

## 1. 定位与职责

persona 是 agent 的**提示词源部件**：把"人格/风格"从硬编码的 system prompt 中拆出，变成可命名的、编译期内置的一组模板，供组合根在**创建会话时**挑选并注入。

- **一个对话 = 一个人格**：persona 在会话创建时定型，存续期内不切换（产品上不提供，非技术禁止）。
- **本模块做三件事**：`go:embed` 内置人格文件 → frontmatter 解析 → 按名查找/列举/默认兜底。
- **明确不做**（本阶段）：中途切换、人格组合/继承、开场白/示例对话字段、运行时目录加载——见 §3、§13 扩展方向。

### 依赖方向（叶子包）

```
main(组合根) → persona（叶子：stdlib + yaml.v3，不 import 任何内部包）
```

- `persona` 不被 `agent`/`session`/`provider` 引用——agent 只收 `WithSystemPrompt(string)`，人格是字符串的来源，不是 agent 的协作对象（对照 `tool`：工具是 agent 的协作对象所以被依赖；persona 不是）。
- 这与 `memory` 的叶子地位同构，只是**内容来源不同**：memory 是运行时读用户可编辑的 Markdown 文件；persona 是编译期嵌入开发者维护的文件。两者正好对照（§4）。

## 2. 决策记录（重点取舍）

| 问题 | 决策 | 理由 |
|---|---|---|
| 换人格语义 | 创建时固定，不提供中途切换 | 旧人格的 assistant 回复留在历史里会让模型串味（二次元 vs 严谨科学家反差尤甚）；"换人格=新起点"是**产品语义**而非缓存约束 |
| 缓存约束 | 不当硬约束 | 本地 CLI 单用户、低频、短对话，一次全 miss 成本毫秒级；改 system 无论何时都全前缀 miss，"长度 0 才能改"是过度推论（见 §2.1） |
| 人格存储 | `personas/*.md` + `go:embed` | 人格是代码资产（开发者维护），编译期快照零运行时失败、单二进制分发 |
| 文件格式 | frontmatter（yaml.v3）+ 正文 | 结构化字段可扩展（description，未来 first_message）；复用 config 既有依赖，**零新增依赖** |
| 接口化 | 不抽 interface，具体 `Manager` struct | agent 不消费 persona（只收字符串）、main 不需要多态；第二来源出现才在**使用方**抽接口 + 编译期断言 |
| 指定入口 | `--persona` flag + 默认人格兜底，不做斜杠命令 | 创建时指定即够；斜杠命令与 flag 同义，省输入循环改动（命令解析不进历史是另一摊事） |
| 恢复优先级 | 会话 meta > `--persona` > 默认 | 会话已定型，恢复不改人格；flag 仅对新建会话生效 |
| 默认人格 | `personas/default.md`（现 main.go 硬编码"简洁助手"提取为模板） | 不指定人格时行为与现状完全一致，零回归 |

### 2.1 缓存纠偏（本设计的论证基石，防止将来被错误回卷）

- **前提事实**：DeepSeek 自动磁盘缓存按"请求前缀"匹配；system prompt 位于每次请求最前（agent.go `allTurns()` 首位），是前缀的锚；命中部分输入 token 价格约 1/10（具体粒度/价格以官方文档为准）。
- **纠正**：改 system prompt，无论对话长度是 0 还是 100，当次请求都是**全前缀 miss**（token 0 起就不同）。"长度 0 时改"唯一差别是"没有已建缓存可浪费"，而浪费是一次性的、毫秒级、下一轮请求自动重建缓存。
- **结论**：真正的优化原则是"切换频率"（换来换去才持续 miss）而非"切换时机"。本项目规模下缓存收益可忽略，不值得作为产品约束。若将来真要省 token，正确姿势是"保持 system 前缀稳定"，而非锁死切换时机。

## 3. 行业做法与取舍

| 手段 | 代表 | 优点 | 缺点 |
|---|---|---|---|
| **新会话即人格**（启动参数/创建时选择器） | ChatGPT GPTs、Dify/FastGPT 应用切换、**本项目** | 语义干净、实现最简、缓存天然无顾虑 | 会话内无法换（产品上本就不需要） |
| 角色卡（Character Card） | SillyTavern/Chub（CCv2 规范） | 可分享、开场白/示例对话提升一致性 | 格式规范重、示例占 token |
| 中途热切换 | Slack 按 channel 绑人格、SillyTavern 中途换卡 | 灵活 | 历史串味、一次全 miss |
| 分层前缀（base 固定 + 变体靠后） | Anthropic `cache_control` 断点、Claude Code `CLAUDE.md` 注入 | 前缀共享缓存 | 复杂度上升；变块长度变化使后续历史错位 |

**取舍**：选第一行（新会话即人格 + `--persona`）。理由：语义与"一个对话一个人格"天然契合、实现面最小（agent 零改动）、缓存顾虑从根上消失。角色卡格式与分层前缀留作扩展方向（§13），等"用户要自己写/分享人格"或"缓存成本真到不可忽视"再评估。

## 4. 文件布局与数据模型

```
internal/persona/
├── persona.go        # Persona/Manager + go:embed + frontmatter 解析
├── personas/         # 人格文件（编译期嵌入，仅开发者维护）
│   ├── default.md        # 默认人格（现 main.go 硬编码"简洁助手"→ 模板化）
│   ├── catton.md         # 二次元元气少女
│   ├── scientist.md      # 严谨科学家/架构师/程序员
│   ├── onee.md           # 御姐
│   └── interviewer.md    # 面试官
└── persona_test.go
```

frontmatter 格式：

```markdown
---
name: catton                          # 可选，缺省取文件名 stem
description: 二次元元气少女，说话带颜文字和萌系语气词   # 可选，供 --persona 帮助/错误提示
---

你是猫瞳（catton），一个元气满满的二次元少女……
（正文即 SystemPrompt，建议自包含："你是…语气…"，不引用工具/记忆——关注点分离）
```

- **解析**：按首个 `---` 行切出 frontmatter 与正文；frontmatter 用 `yaml.v3`（复用 config 既有依赖，不新增）；正文 trim 后作为 `SystemPrompt`。
- **约束**：正文非空；name 冲突视为格式错误（组合根 fail fast）；**`default.md` 必须存在且正文非空**——它是默认兜底，缺失属开发错误，Load 时校验并报错（fail fast 早暴露，与"配置缺失即启动失败"的项目惯例一致）。
- **budget 守门**：system 在截断逻辑中保留，persona 加长会挤占历史预算——人格正文建议 ≤1500 字符（对齐 memory 的 2KB 上限思路），测试守门。

## 5. 对外 API（模块对象，不接口化）

```go
type Persona struct {
    Name         string
    Description  string
    SystemPrompt string
}

func Load() (*Manager, error)                       // 从 embed.FS 扫描 personas/*.md（稳定排序）并解析
// 格式错误（坏 frontmatter/空正文/name 冲突/缺 default.md）= 开发错误，返回 error（组合根 fail fast）。

func (m *Manager) Get(name string) (Persona, error) // 按名查找；未命中返回业务错误，附可用列表提示
func (m *Manager) List() []Persona                  // 按文件名稳定排序，供 --persona 帮助/错误提示
func (m *Manager) Default() Persona                 // 返回 default.md 对应人格（Load 已保证存在）
```

- **不接口化**：唯一实现、无第二形态，接口是空抽象（同 `session.Store`/`memory.Store` 理由，tool.md §4.2）。将来加运行时目录覆盖时，在 **main（使用方）** 定义 `Provider` 接口并配编译期断言 `var _ Provider = (*Manager)(nil)`。
- **并发**：Load 后只读、无状态，无需锁（`-race` 干净）。
- **错误分层**：Get 未命中 = 业务错误（参数问题，main 报错并列出可用人格）；Load 失败 = 框架错误（开发错误，fail fast）。

## 6. 组合根装配（main.go）

```go
mgr, err := persona.Load()                      // 开发错误，fail fast
p := mgr.Default()
if *personaFlag != "" {
    p, err = mgr.Get(*personaFlag)              // 未命中：报错并列出可用人格（List）
}

// 恢复会话：meta.Persona 优先于 flag（会话已定型），冲突时以 meta 为准并打印提示
base := "你是一个简洁的助手…" + 工具说明 + 记忆规则   // 现有硬编码段，人格无关
prompt := base + "\n\n" + p.SystemPrompt
if boot := loadBootstrapMemory(...); boot != "" {
    prompt += "\n\n<memory>\n" + boot + "\n</memory>"
}
agent.New(..., agent.WithSystemPrompt(prompt), ...)
```

- **优先级**：会话 meta > `--persona` > 默认；`--persona` 仅对新建会话生效。
- **拼装顺序**：base（行为/工具契约）→ persona（角色/语气）→ memory（常驻事实）。base 保持现状不动，persona 插在中段，最小 diff。
- **新建会话**：`store.Load` 返回 nil（文件不存在）时，main 先 `store.WriteMeta(id, {Persona: p.Name})` 再开始对话（见 §7）。

## 7. session 集成（头行 meta）

JSONL 首行记录会话人格，消息行格式不变：

```json
{"meta":{"persona":"catton"}}
```

- **写入**：新增 `Store.WriteMeta(id, header)`，仅 main 在**新建会话**时调用（Load 返回 nil 才写），先于首次 Run。
- **读取**：`Store.Load` 对每行先按 Header 解析，`meta.persona` 非空视为头行跳过，否则按 Message 解析——**兼容旧文件**（无头行 → 行为与改前完全一致，这是主风险点，必须有回归测试）。新增 `Store.Meta(id)` 只读首行。
- **Rewrite 保头**：compaction 的全量重写会丢头 → `Rewrite` 重写前从原文件读回头行，在新文件首行重放（Store 内部自持，签名不变）。
- **countLines 修正**：头行计入行数会让 `List` 的 `TurnCount` 多 1 → 跳过首行头行。
- **agent 零改动**：`persist.go` 只调 `Append`；`Append` 用 `O_APPEND|O_CREATE` 不截断，头行天然保留。

## 8. config 集成

**零改动，不新增配置项**：人格来源是 `go:embed`（无目录可配）；默认人格 = `personas/default.md` 内容（改默认人格 = 改文件，不需要配置项）。将来若加运行时目录，再补 `SMALL_PERSONA_DIR`。

## 9. 影响面与依赖检查

| 模块 | 影响 | 依据 |
|---|---|---|
| `internal/persona`（新） | 纯新增 | 叶子包：stdlib + yaml.v3（既有依赖），不 import 任何内部包 |
| `main.go` | 中等：`--persona` flag、Load、优先级选择、system 拼装、新建会话 WriteMeta | 组合根职责（装配逻辑本就集中于此） |
| `internal/session` | 中等：Header 解析、Load 跳头行、`Meta()`/`WriteMeta()`、Rewrite 保头、countLines 修正 + 兼容回归测试 | 头行是存储格式扩展；**主风险在旧会话文件兼容** |
| `internal/agent` | **零改动** | system 是构造期字符串；persist.go 只调 Append |
| `internal/agent/compact.go` | 不改代码；注意 persona 加长 system 挤占预算 | §4 长度守门 |
| `internal/config` | **零改动** | §8 |
| `CLAUDE.md` / `路线图.md` | 目录结构 + 决策表追加；勾选项 | 项目惯例 |

**依赖方向验证**：`main → persona`；persona 不 import 任何内部包；agent/session/provider 不感知 persona——无环、不违反单向红线。

## 10. 测试设计与验收

| 层 | 用例 | 判据 |
|---|---|---|
| 单测 | frontmatter 解析 | 合法文件解析出 name/description/正文；缺 name 取文件名 stem；缺 description 为空串 |
| 单测 | 格式错误 | 坏 frontmatter / 空正文 / name 冲突 → Load 返回 error |
| 单测 | embed 全量 | 遍历 `personas/*.md` 全部解析成功（防提交坏文件）；default.md 存在且正文非空 |
| 单测 | Get/List/Default | 命中；未命中返回业务错误且列表提示；List 稳定排序；Default 返回 default.md |
| 单测 | 正文长度 | 所有人格正文 ≤1500 字符（budget 守门） |
| 单测（session） | 兼容回归 | **旧文件（无头行）Load 结果与改前逐条一致**；新文件头行被跳过、Meta 读回正确 |
| 单测（session） | Rewrite 保头 | 截断重写后头行仍在首行，消息不丢 |
| 集成 | 组合根装配 | 新建会话写头；恢复会话 meta 带出人格；meta 与 flag 冲突时 meta 胜 |
| 手工 | CLI | `--persona catton` 开新会话 → 模型以猫瞳口吻回复；退出重进 → 仍是猫瞳；不带 flag → 行为与现状一致 |

合并门槛：`go test -race ./...`、`go vet`、`gofmt` 全绿。

## 11. 落地路径

**阶段一（本模块）**：`internal/persona` + 5 个示例人格 md + 单测（含长度守门、default 校验）。
**阶段二（存储）**：session Header/Meta/WriteMeta/Rewrite 保头 + 兼容回归测试。
**阶段三（装配）**：main.go flag + 优先级选择 + system 拼装 + 新建会话 WriteMeta + 集成测试。
**阶段四（收尾）**：更新 `CLAUDE.md` 目录结构与决策表、`路线图.md` 勾选；手工 CLI 验证。

## 12. 需要补的知识（同频清单）

1. **DeepSeek 官方上下文缓存文档**——验证 §2.1"缓存不构成约束"的判断；将来真要省 token 时知道正确姿势（保持 system 前缀稳定，而非锁死切换时机）。
2. **`go:embed` 语义**——编译期快照、只读 `fs.FS`、`//go:embed` 路径相对源码文件；评审实现时用于判断加载/错误处理是否合理。
3. **角色卡规范**（SillyTavern `character_card_v2`）——未来加 `first_message`/示例对话字段时的字段设计参考。
4. **对比**：Claude Code `CLAUDE.md` / Codex `AGENTS.md` 的用户级配置注入方式——理解"分层/断点"思路，判断将来是否需要。

## 13. 扩展方向（量级/需求到了再评估）

| 方向 | 手段 | 优缺点 | 取舍依据 |
|---|---|---|---|
| 人格文件化增强 | 角色卡字段：first_message、示例对话、禁忌话题 | +开场代入感、few-shot 一致性；−占 token、格式规范重 | 二次元/御姐类人格需要开场白，科学家/面试官不需要 → 有需求再加字段 |
| 人格组合/继承 | base + overlay（"御姐+科学家"） | +组合爆炸小；−每层占 token、一致性难调 | 当前人格数量少，直人格即够 |
| 运行时目录加载 | `SMALL_PERSONA_DIR` 覆盖 embed | +用户可自配；−多一个运行时 IO 失败点、不再单二进制 | 出现"用户要自己写人格"的真实需求再评估 |
| 人格一致性评估 | 固定测试集跑几个人格对比输出 | +可量化一致性；−成本 | 低优先级，CLI 场景收益有限 |
| 人格相关记忆 | 人格各自的长期记忆 | +角色连续性；−复杂 | 通用记忆走现有 memory 系统即可，不做 |
