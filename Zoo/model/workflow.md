# 工作流分支机制（workflow）——PDF 解析分支

> 位置：`internal/workflow`（包 + embed 资产）、`workflows/pdf.md`（PDF 分支实例）、`main.go`（装配/命令）、`main_commands.go`（/pdf 命令）
> 状态：设计（2026-09-01，待用户确认后实现）
> 关联：`internal/persona`（embed + frontmatter 成熟模式，本机制对齐）、`Zoo/model/pdf-workflow.md`（PDF 整理工作流内容）、`Zoo/temp/lit/SKILL.md`（doc_* 工具使用指南）、`Zoo/model/cli.md`（命令系统）

## 1. 结论先行

目标：把"PDF 解析整理"这类任务沉淀为稳定的工作流分支（skill/workflow），模型收到此类任务时自动进入分支、按既定步骤执行、明确停止条件——提升任务处理稳定性，而非每次靠临场发挥。

形态（2026-09-01 用户拍板）：
1. 提示词数据层：workflow 定义为 go:embed 的 md 资产（frontmatter 写元数据，正文写步骤），对齐 persona 模式；注入 base 提示词作为"可用工作流分支"清单。零运行时改动、模块化天然（每 workflow 一个文件）、可单测。
2. 触发双通道：提示词自动判断（模型识别任务命中触发条件 → 声明进入分支）+ /pdf 命令显式进入（不依赖模型判断时）。
3. 停止条件：正常结束 = 产物产出 + 向用户汇报摘要，等待确认/新指令即结束；异常兜底 = 工具失败、用户中断、轮次超限即停。
4. 落点：新建 internal/workflow 包（叶子，不 import 内部包）+ workflows/pdf.md 实例 + /pdf 命令。

## 2. 需求理解

用户（2026-09-01）：
- 为 PDF 解析过程专门添加 skill/workflow，提升处理稳定性。
- 严格框定描述、输入输出；规划步骤（各步做什么）；明确何时停止。
- 用户提出 PDF 解析类任务（提示词或命令）时自动转到该分支。
- workflow 模块化，后期可扩展新 workflow（如 Word 处理、网页整理）。

## 3. workflow 包设计

对齐 persona（internal/persona）模式：go:embed workflows/*.md，frontmatter（yaml.v3）定义元数据，正文定义步骤。

### 3.1 类型与 API

```go
// Workflow 一个任务工作流分支：元数据 + 步骤正文。
type Workflow struct {
	Name        string // 唯一名（如 pdf），触发判断与命令的标识
	Description string // 一句话说明（分支清单展示）
	Trigger     string // 触发条件描述（模型判断依据）
	Input       string // 输入说明（用户需提供什么）
	Output      string // 输出说明（产物形态）
	Stop        string // 停止条件
	Steps       string // 步骤正文（markdown，渲染进分支清单）
}

type Manager struct { workflows map[string]Workflow; order []string }

func Load() (*Manager, error)  // embed workflows/*.md 解析，坏文件/重名 = 开发错误 fail fast
func (m *Manager) Get(name string) (Workflow, error)
func (m *Manager) List() []Workflow
func (m *Manager) RenderBranch() string // "可用工作流分支"提示词段（注入 base）
```

### 3.2 文件格式（workflows/pdf.md）

```markdown
---
name: pdf
description: 解析 PDF/Word 等文档并整理
trigger: 用户要求解析/阅读/提取/整理 PDF、Word、PPT 等文档文件（含 /pdf 命令显式进入）
input: 文档路径（必填）；可选：页范围（pages）、OCR（ocr）、输出格式、整理要求
output: 清洗后的缓存文本（doc_parse 产物）+ 整理后文档（用户确认后落盘）+ 内容摘要
stop: 产出并向用户汇报摘要后，等待用户确认/新指令即结束；工具失败、用户中断、轮次超限立即停止并说明
---
（正文 = 步骤，markdown 有序列表）
1. 解析：doc_parse <路径>（大文件按 pages 分片，每片独立缓存）。
2. 清洗：doc_clean 清噪声（分页符/页码/水印/CJK 字间空格/横线页码）。
3. 细读：doc_read 窗口化读产物，控制上下文成本。
4. 整理：按 SKILL 清单修复语义问题（段落合并/词间距/换行粘连/数字空格/双栏重排）。
5. 落盘：先询问用户原位整理 or 新开文件，确认后 file_write/propose_file_write。
6. 汇报：输出内容摘要与产物路径，等待用户确认或新指令（结束）。
```

### 3.3 渲染与装配

- RenderBranch 输出固定格式（分支清单），追加进 main.go 的 base 提示词（契约层，人格无关）。
- base 提示词同时加触发约束句："用户任务命中某分支的触发条件时，第一句回复声明进入该分支（如：进入 pdf 分支），严格按步骤执行，产出后汇报并等待确认。"
- 可见性声明（2026-09-01，纯提示词 + 固定格式）：RenderBranch 引言内嵌声明要求——进入分支时第一句以 `[进入分支:<名>]` 开头（如 `[进入分支:pdf]`），完成汇报时须包含 `[分支完成:<名>]`。用户对话输出可直接核实是否用了 skill、用了哪个。不引入运行时观测（命令 /pdf 天然可见，提示词触发靠固定标记）。

## 4. 触发与停止（明确表述）

### 4.1 触发双通道

- 提示词通道：模型收到"解析/阅读/提取/整理某文档"类任务 → 命中 pdf 分支 trigger → 声明进入分支并按步骤执行（自动）。
- 命令通道：/pdf <文档路径> → 命令校验参数后注入一条用户消息（"请按 pdf 工作流处理文档：<路径>，用户已显式进入 pdf 分支"），模型同样按分支执行（显式）。
- 命令注入机制（command 包零改动）：/pdf 的 Run 成功时返回注入消息 + 哨兵错误 errInject（对齐 errExit 模式）；main 主循环检测到 errInject 时把该消息当作本轮用户输入送 agent，走正常 Run 路径（含 plan/proposals ctx 与持久化）。

### 4.2 停止条件

- 正常：分支末步"汇报摘要等待确认"——用户确认/给出新指令即 workflow 结束，回归普通对话。
- 异常兜底（同样写入 stop 字段，模型须遵守）：工具连续失败、用户主动打断（如"停"）、工具轮次超限（agent 层 max_tool_rounds）→ 停止并说明原因与已产出部分。

## 5. 实现落点

| 改动 | 落点 |
|---|---|
| workflow 包 | internal/workflow/workflow.go（embed + 解析 + Manager + RenderBranch） |
| PDF 分支实例 | internal/workflow/workflows/pdf.md |
| 装配 | main.go：workflow.Load → base 提示词追加 RenderBranch + 触发约束句 |
| /pdf 命令 | main_commands.go：cmdPdf（参数校验 + 注入消息 + errInject 哨兵）；main.go 注册 + 主循环 errInject 分支 |
| 文档 | Zoo/model/workflow.md（本文件） |

## 6. 测试计划

- workflow 包：Load 解析（frontmatter 元数据/正文/缺 name 取文件名）、坏 frontmatter/空正文/重名报错、Get/List/RenderBranch 输出格式。
- /pdf 命令：无参数报错（提示用法）；有参数返回注入消息 + errInject。
- 主循环 errInject 分支：main 包无测试，靠手动验证（文档说明）。

## 7. 后期扩展

新增 workflow 三步：加 workflows/<name>.md（frontmatter + 步骤正文）→ base 提示词自动带出分支清单（Load 自动收录）→ 如需命令入口，加对应 /<name> 命令。无需改 workflow 包。

## 8. 决策记录

1. 提示词数据层形态（2026-09-01 用户确认）：对齐 persona 的 embed + frontmatter 模式，不做运行时引擎。
2. 触发双通道（2026-09-01 用户确认）：提示词自动判断 + /pdf 命令显式进入。
3. 停止条件（2026-09-01 用户确认）：产出+汇报+异常兜底。
4. 落点（2026-09-01 用户确认）：workflow 包 + PDF 实例 + /pdf 命令 + 本设计文档。
5. 可见性声明（2026-09-01 用户确认）：纯提示词 + 固定格式标记（`[进入分支:<名>]` / `[分支完成:<名>]`），仅对话声明、不引入运行时观测。
