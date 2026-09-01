# 文档解析工具（doc_parse / doc_read / doc_clean）

> 位置：`internal/tool/builtin`（落点 lit.go 等）
> 状态：设计（2026-09-01，待用户确认后实现）
> 关联：`model/tool.md`（工具模块本体）；`model/tool-fs.md`（文件工具窗口化/路径约束）；`model/policy.md`（权限 Pass/Ask）；`Zoo/temp/lit/`（lit 遗留 SKILL 与真实样例）

## 1. 结论先行

目标：把本地文档解析 CLI `lit`（LlamaIndex LiteParse）的能力封装为内置工具，agent 不记 lit 命令即可解析 PDF/DOCX 等文档；同时对解析产物的噪声做确定性清洗，语义类问题交给 AI 在整理阶段修复。

形态：三个内置工具 `doc_parse`（解析落盘）、`doc_read`（读缓存产物）、`doc_clean`（确定性清洗）。比最初设想（两个工具）多一个 `doc_read`，理由见 §4——`file_read` 被工作区路径约束挡在缓存目录外，读缓存产物必须专用工具（对齐 memory_get 读 memory 目录的模式）。

已确认决策（2026-09-01 用户拍板）：
1. 按职责拆工具，独立单测、可组合。
2. 修复分工：规则管 `----` 分页符/水印重复/页码行/空行折叠；AI 管换行粘连（异常点1）、列表丢行（异常点2）、数字空格拆分（异常点4）、双栏重排（改进点）。
3. 解析产物落系统缓存目录（`~/.small/cache`），doc_parse/doc_read/doc_clean 全 Pass 权限（写系统缓存类比 memory_save 写归档层，受控写入非用户工作区）。
4. source 允许任意绝对路径（PDF 可能在任意位置，解析只读+落缓存目录，风险可控）。

## 2. 需求与问题清单

需求（来自用户 2026-09-01）：
- 把 lit 解析能力做成工具（封装常用命令模板为固定函数）+ 配套 skill 说明（改造 LlamaIndex 遗留 SKILL，写清工具用法与陷阱）。
- 降低重复劳动：同一文件重复解析应复用（parse once 的工具化落地）。
- 降低水印干扰：解析产物里水印重复字眼（如"人民法院案例库 人民法院案例库"）确定性去除。
- 降低使用门槛：agent 用工具而非裸 lit 命令。
- 智能整理：解析出的 md 按用户要求自定义整理；由用户决定在原文件直接整理还是新开文件（走既有 propose_file_write / file_write 确认交互）。

解析质量问题分类（规则可修 vs AI 修）：

| 问题 | 现象 | 归属 |
|---|---|---|
| 分页符 | PDF 分页产生的 `----` 横线独立成行 | 规则 |
| 页码残留 | "第 1 页"独立成行 | 规则 |
| 水印重复 | 同一行内词组重复（异常点3）；相邻行完全重复 | 规则 |
| 空行堆积 | 连续多空行 | 规则 |
| 换行粘连 | 列表项挤成一行、语义断行丢失（异常点1） | AI |
| 列表丢行 | 列表项之间被插空行/换行错位（异常点2） | AI |
| 数字空格拆分 | "4.4亿"→"4.4 亿"、"2024年7月10日"→"2024 7 10"（异常点4） | AI |
| 双栏大空白 | 知网双栏中间大空白划开（改进点） | AI |

规则保守原则：宁可少改不可误伤正常文本。每条规则独立函数、独立单测、可开关（本期默认全开，函数级粒度天然可关）。

## 3. 工具设计

> v2 增强（2026-09-01）：doc_clean 新增 CJK 字间空格压缩、横线页码残留删除（规则 6/7）；
> AI 整理工作流（段落合并/词间距/大 PDF 分批）见 Zoo/model/pdf-workflow.md 与 SKILL。

### 3.1 doc_parse（解析落盘）

作用：调 `lit parse` 解析文档（PDF/DOCX/PPTX/XLSX/图片均支持，lit 自动转换），产物落缓存目录。

参数：
- source（必填）：源文档绝对路径（允许任意路径，不做工作区约束）。
- pages（可选）：`--target-pages`，如 "1-5,10"，缺省全文档。
- ocr（可选，bool 默认 false）：false 传 `--no-ocr`（born-digital 默认，快且同质）；true 去掉（扫描件/图片）。
- format（可选，默认 text）：text / json。json 仅当需要版式/坐标时用（产物更大，仍按文件读不整灌）。

行为：
1. `exec.LookPath("lit")` 检查依赖：缺失返回安装指引（`npm i -g @llamaindex/liteparse`；Office 需 LibreOffice、图片需 ImageMagick），不自动安装。
2. 缓存命中判断：对 source 做 sha256（前 12 位）拼入产物名，命中直接返回（parse once 落地）。
3. 未命中：`lit parse <source> --format <text|json> [--no-ocr] [--target-pages N] -o <缓存路径>`，exec.CommandContext，默认超时 5 分钟（扫描件慢）。
4. 产物命名 `<basename>.<hash12>.<txt|md|json>`。

返回：缓存文件绝对路径 + 行数/字符数 + 摘要（前 40 行，模型先判断相关性）+ 已知噪声提示（"产物可能含水印/分页符残留，可用 doc_clean 清洗"）。

权限：Pass。

### 3.2 doc_read（读缓存产物）

作用：读 doc_parse 产物（窗口化，对齐 file_read 语义）。`file_read` 受工作区约束读不了 `~/.small/cache`，必须专用工具。

参数：path（必填，缓存目录内的产物路径）、offset（1-based 起始行，缺省 1）、limit（行数，缺省到文件尾）。

行为：路径必须位于缓存目录内（前缀校验，防读任意文件）；带行号渲染；截断提示续读（对齐 file_read 的"（继续可 offset=N）"）。

权限：Pass。

### 3.3 doc_clean（确定性清洗）

作用：对缓存产物做规则清洗，原地改写，返回清洗报告（删了几类噪声）。只接受缓存目录内的文件（doc_parse 产物），防误改用户文件。

清洗规则（每规则独立函数 + 单测）：
1. 分页符行：整行仅由 3 个以上 `-`（及可选空白）构成 → 删除。
2. 页码行：整行匹配 `第\s*\d+\s*页`（可有前导空白）→ 删除。
3. 行内重复折叠：同一行内紧邻重复词组折叠为一次（如 "人民法院案例库 人民法院案例库" → "人民法院案例库"；以空格分隔的相同 token 段 ≥2 次连续出现）。
4. 相邻行去重：相邻两行 trim 后完全相同 → 保留一行。
5. 空行折叠：连续 ≥2 空行折叠为 1 个。

返回：报告文本（各规则删除了多少行/处；未命中则说明"无噪声"）。

权限：Pass。

## 4. 缓存与复用

- 目录：config 新增 `cache_dir`，缺省 `~/.small/cache`，config.yml 可配（对齐 session_dir/memory_dir/kb_dir 同一来源与展开逻辑）。
- 命名：`<basename>.<source sha256 前 12 位>.<ext>`，同源同内容重复解析直接命中，不再调 lit（降低重复劳动的直接落地）。
- 多一个 doc_read 的理由：file_read 的 resolveInRoot 只放行工作区内文件，缓存目录在工作区外；doc_read 是读取口（对齐 memory_get 读 memory 目录的部件内读模式）。
- 清理：本期不做自动清理（受控目录，量小）。若膨胀后续按时间戳/大小清理，不做进本期。

## 5. 依赖与安全

- lit 依赖：工具只做检查与指引，不自动安装（无网络安装动作）；错误信息附安装命令。
- 超时：doc_parse 默认 5 分钟（ContextTimeout，可被 ctx 取消）。
- 路径：source 任意绝对路径（用户决策）；产物与清洗只发生在缓存目录内；doc_clean 输入限定缓存目录。
- 权限登记：doc_parse/doc_read/doc_clean 均 Pass（只读 + 写受控缓存目录，类比 memory_save 写归档层；登记进 ToolPermissions 表，漏登记由 validateToolPermissions 注册期暴露）。
- builtin 生产代码不新增反向依赖（仅 tool + 标准库，imports_test.go 固化）。

## 6. 实现落点（对齐 CLAUDE.md 新增内置工具五步）

1. 构造函数：`builtin/lit.go` 写 `LitParse(cfg *LitConfig)`、`LitRead(cfg *LitConfig)`、`LitClean(cfg *LitConfig)`；执行逻辑外置 `runLitParse/runLitRead/runLitClean` + clean 规则函数 `cleanRules`；`LitConfig{Root string}`（缓存根）。
2. 注册：`register.go` Deps 扩 `Cache *LitConfig` 字段（nil 退化），`RegisterBuiltins` 追加三工具。
3. 权限：`tool_permissions.go` 登记三工具 Pass。
4. 提示词：`main.go` base 字符串补工具说明（用途 + 触发约束 + doc_clean 后处理提醒）。
5. 配置：`config.go` 加 `CacheDir` 字段 + `cache_dir` yaml + `defaultCacheDir = "~/.small/cache"`；`main.go` 装配 `builtin.Deps{Cache: &builtin.LitConfig{Root: cfg.CacheDir}}`。

## 7. SKILL 改造

- 落点：就地改造 `Zoo/temp/lit/SKILL.md`（保留 LiteParse 原文意图，适配本项目工具形态；工作区内，模型可 doc_search/file_read 访问）。
- 改造内容：
  - 工具用法替代裸 lit 命令：doc_parse（解析）→ doc_clean（清洗）→ doc_read（读窗口）。
  - 陷阱清单：异常点 1/2/3/4 的具体现象 + 修复指引（哪些规则已自动处理，哪些需 AI 整理时修复）。
  - 整理工作流：parse → clean → read → 询问用户原位整理 or 新开文件（propose_file_write / file_write 确认）→ 落盘。
  - AI 修复清单：换行粘连（异常点1）、列表丢行（异常点2）、数字/日期空格拆分（异常点4）、双栏重排（改进点）的识别与改写示例。
  - 保留的纪律：parse once（缓存复用）、按窗口读不整灌、json 格式只按需。
- base 提示词只写一句话工具说明，完整纪律在 SKILL（模型需要时可 doc_read 读取；SKILL 本身不进常驻提示词）。

## 8. 测试计划

- clean 规则单测：用 `Zoo/temp/lit/来某虐待、王某诉雷某变更抚养关系纠纷等案.md`（真实水印/页码/换行样例）与 `基于小程序的高校场地预约平台设计_田睿芬.md`（数字空格/双栏样例）构造用例，验证各规则命中与不误伤。
- 缓存命中：同源二次解析不触发 lit（lit 缺失时也能命中返回）。
- lit 缺失分支：LookPath 失败返回安装指引。
- doc_read 越界：缓存目录外路径拒绝。
- imports_test.go 不反向依赖。
- 手动验证：lit 安装后解析真实 PDF 全链路（本机当前未装 lit，装好后验证）。

## 9. 决策记录

1. 工具命名 doc_ 前缀（2026-09-01 用户确认）：lit 实际支持 pdf/docx/pptx/xlsx/图片，doc_ 更准确。
2. 三工具 doc_parse/doc_read/doc_clean（2026-09-01 用户确认）：doc_read 因 file_read 工作区约束而必要。
3. SKILL 改造就地落点 temp/lit/SKILL.md（定稿）。
4. doc_parse 默认超时 5 分钟（定稿；大扫描件可后续调大或可配置化）。
