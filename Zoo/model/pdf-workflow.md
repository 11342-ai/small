# PDF 处理链路增强（v2）：段落修复 / 大文件分批 / 轮次配置 / 白名单

> 位置：`internal/tool/builtin/lit.go`（clean 规则）、`internal/agent/agent.go`（轮次）、`internal/config/config.go`（配置）、`main.go`（装配/白名单）、`Zoo/temp/lit/SKILL.md`（整理工作流）
> 状态：设计（2026-09-01，待用户确认后实现）
> 关联：`model/tool-lit.md`（doc_* 工具 v1）、`model/agent.md`（工具循环）、`Zoo/temp/目前发现的问题.md#L110-130`（改进点1）

## 1. 结论先行

v1（doc_parse/doc_read/doc_clean + 读路径放宽）已落地。本次针对 PDF 处理链路的 4 个增强（2026-09-01 用户拍板）：

1. 段落修复（规则+AI 分工）：doc_clean 新增两条确定性规则（CJK 字间空格压缩、分页页码残留删除）；段落合并/词间距恢复/重排走 AI 整理工作流（SKILL 强化）。
2. 大 PDF 分页切片批处理：doc_parse pages 分片 + 每片独立缓存 + AI 分批整理后拼接（doc_parse 已有 pages 能力，本次补工作流指引）。
3. 轮次上限配置化：maxToolRounds 硬编码 8 → config.yml `max_tool_rounds`（缺省 20，显式 0 禁用），agent Option 注入。
4. exec 白名单：加入 find（只读查找）、cp（复制，Ask 确认兜底）。

## 2. 问题与现状

- 改进点1（`Zoo/temp/目前发现的问题.md#L110-130`）：中文 PDF 解析产物逐字拆分（"第 一 百 四 十 一 条"），分页残留（`－70－` 独立成行）未清除，段落未合并。预期为规范中文段落（"第一百四十一条 生产、销售假药的，处三年以下有期徒刑……"）。
- `agent: exceeded 8 tool rounds`：PDF 全链路（parse→clean→read→整理→write）调用多，大文件分批后翻倍，8 轮硬上限不够。
- exec 白名单缺 find/cp：文件查找、复制等 PDF 管理动作无法执行（exec 为 Ask，确认仍兜底）。

## 3. doc_clean 规则增强（规则+AI 分工）

新增两条规则（沿用保守原则：宁可少改不误伤；独立函数 + 单测，tool-lit.md §3.3 同款）：

- 规则 6 CJK 字间空格压缩：删除 CJK 字符（含全角标点）之间的空格。效果："第 一 百 四 十 一 条" → "第一百四十一条"；"处 三 年 以 下" → "处三年以下"。实现按 rune 扫描，空格两侧均为 CJK/全角标点才删（Go regexp `\p{Han}` + 全角标点集）；数字/拉丁/英文之间的空格不动（防误伤 "4.4 亿"、"200 个行业" 类需 AI 处理，SKILL 指引）。
  - 边界：产物整段逐字空格时删光后为连续中文（无词间距），词间距恢复属 AI 整理任务（§4），规则不越界。
- 规则 7 分页页码残留删除：独立成行的 `－70－` 类页码（全角/半角横线 + 数字 + 横线，含空白）→ 删行。正则 `^[－\-—]\s*\d+\s*[－\-—]$`（覆盖 "－70－"、"- 70 -"、"——12——" 等变体）。

段落合并/重排不写规则（段落边界、列表/标题保护难以确定，误伤风险高），归 AI 整理（§4）。

## 4. AI 整理工作流（SKILL 强化）

SKILL.md 的"AI 整理阶段需手动修复"一节增强为完整整理流程：

1. 预处理（确定性已做）：doc_clean 删 CJK 字间空格、分页残留、页码、水印。
2. 段落合并（AI）：断行成段的规则——行尾无句读（。！？；）且语义连续 → 与下行合并；被分页符/页码打断的段落拼接；逐字空格已清，重点恢复断行与词间距。
3. 词间距恢复（AI）：连续中文按语义恢复自然短语间距（"第一百四十一条 生产、销售假药的"），标点后不加空格（中文习惯）。
4. 大 PDF 分批（AI 流程）：doc_parse pages 分片（每片 ~50 页，独立缓存文件）→ 每片 doc_clean + doc_read 细读 + 片段整理 → 全部整理完按序拼接输出。
5. 输出：先询问用户原位整理 or 新开文件（已有确认交互）；分批时先整出各片片段，最后拼接成完整文档。

## 5. max_tool_rounds 配置化

- agent.go：删 `const maxToolRounds = 8`；Agent 增 `maxRounds int`（缺省 20）；新增 Option `WithMaxToolRounds(n int)`（n<=0 视为不限，防死循环改用极大上限兜底）；Run 循环上限改用该字段；错误信息保持 `agent: exceeded N tool rounds` 文案（测试断言含 N）。
- config.go：`MaxToolRounds *int` yaml `max_tool_rounds`；缺省 20；显式 0 = 不限（对齐 max_tokens 显式 0 语义）。
- main.go：装配 `agent.WithMaxToolRounds(cfg.MaxToolRounds)`。

## 6. exec 白名单

- main.go `execAllow` 追加 `find`、`cp`（exec 为 Ask，每步确认兜底；find 只读，cp 有确认门）。
- 不追加 mkdir/rm/mv 等（用户未要求，写/破坏性操作保持缺省拒绝）。

## 7. 实现落点

| 改动 | 落点 |
|---|---|
| clean 规则 6/7 | builtin/lit.go（docFoldCJKSpace、docPageNumDashedRe、cleanDocLines 增分支 + counts 扩字段） |
| 整理工作流 | Zoo/temp/lit/SKILL.md（§4 内容：段落合并/词间距/分批流程） |
| 轮次配置化 | agent.go（Option + 字段 + 循环）、config.go（字段 + yaml + 默认）、main.go（装配） |
| 白名单 | main.go execAllow 追加 find/cp |

## 8. 测试计划

- clean 规则：CJK 字间空格压缩（逐字拆分样例 → 连续中文；数字/英文间空格不动）；分页页码残留（"－70－"/"- 70 -"删行；正文行不误删）；规则 6/7 与既有规则回归（tool-lit.md 样例）。
- 轮次：WithMaxToolRounds 生效（n=2 超限报错、n=0 不限）；config 缺省 20 / 显式 0 / 文件覆盖。
- 白名单：execAllowed 对 find/cp 放行（builtin 单测参数化即可覆盖，main 无测试）。

## 9. 决策记录

1. 段落修复规则+AI 分工（2026-09-01 用户确认）：doc_clean 只做确定性（CJK 空格/分页残留），段落合并/词间距/重排归 AI 整理。
2. 大 PDF 分页切片批处理（2026-09-01 用户确认）：doc_parse pages 分片 + AI 分批整理 + 拼接。
3. 轮次配置化缺省 20、显式 0 不限（2026-09-01 用户确认）。
4. exec 白名单加 find/cp（用户要求，2026-09-01）。
