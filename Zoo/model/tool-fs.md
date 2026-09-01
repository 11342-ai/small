# 文件系统工具族（读增强 + 写族）方案

> 位置：`internal/tool/builtin`（file.go 扩展 + 新增 file_write.go / file_edit.go）
> 状态：**A 档 + B 档全部实现（2026-08-28）**：file_write / file_edit / file_read 窗口化 / 敏感文件防护 / 常量忽略集 / file_tree / gitignore 解析 / propose_file_write/edit；原待决 3 项已定案（§10）；**读工具路径放宽（2026-09-01，§5/§11）**
> 关联：`model/tool-extend.md`（B 档写工具卡审批）；`model/plan.md`（ctx 注入先例）；`model/cli.md`（Permission 种子）；`CLAUDE.md`（红线与约定）
> 对应清单：read_file 窗口化 / write_file / str_replace / propose_* / read_subtree / code_search 语义层 / read_docs

## 1. 结论先行

- 本次落地 5 件事（✅ 已实现 2026-08-28）：
  - ✅ **file_write**（整体覆盖写，原子写）
  - ✅ **file_edit**（多组字符串替换：replacements + 默认唯一匹配 + allowMultiple 开关 + 行尾归一化 + 失败累积）
  - ✅ **file_read 窗口化**（offset/limit + 超界提示 + 续读建议）
  - ✅ **敏感文件防护**（`.env*` 拒绝读取，fail-closed）
  - ✅ **常量忽略集**（file_list/doc_search/file_tree 遍历时跳过 node_modules/dist/*.min.js/.map 等）
- 本次追加落地（B 档转本次）：**file_tree**（目录树预算读）、**gitignore 解析**（遍历遵循 .gitignore）、**propose_file_write / propose_file_edit**（提议暂存 → 组合根确认后落地，权限横切最小形态）。
- 追加落地（2026-09-01）：**读工具路径放宽**——file_read/file_list/file_tree/doc_search 允许访问工作区外绝对路径（用户显式有访问某目录的需求时，如 ~/Pdf），敏感系统目录黑名单 fail-closed（§5）；写工具（file_write/file_edit/propose_*）保持严格工作区约束不变；base 提示词加软确认约束（访问工作区外前先征得用户同意，Codex 式问答）。
- 暂缓 0 件；其余不做项见 §2 C 档。
- 不做（在 §2 给出理由）：**缩进容错**、**去空白兜底**、**referencedBy 符号引用**、**BPE token 估算**、**四级截断/符号分数/内存文件树**、**code_search 语义层**、**read_docs 专用工具**。
- 安全总纲：**写工具 fail-closed 同 exec**——`FileConfig.Confirm` 回调为 nil 则不注册写工具（读工具照常）；每步确认 + 原子写 + 路径约束（复用 resolveInRoot）。
- tool 壳零改动；依赖注入面：`FileConfig` 扩 `Confirm` 字段（不破签名，对齐 tool-extend.md 边界①）。

## 2. 分档总表

| 档 | 工具 | 干什么 | 理由 |
|---|---|---|---|
| A ✅ 已实现 | file_write | 整体覆盖写文件（原子写） | 补"写"核心缺口，工具生态标配 |
| A ✅ 已实现 | file_edit | 多组字符串替换（唯一性 + allowMultiple + 行尾归一化） | 编辑主流形态，省 token、改动精准 |
| A ✅ 已实现 | file_read 窗口化 | offset/limit 行区间读取 + 超界提示/续读建议 | 标配能力，防大文件整读浪费上下文 |
| A ✅ 已实现 | 敏感文件防护 | `.env*` 拒绝读取（fail-closed） | 防机密进模型上下文 |
| A ✅ 已实现 | 常量忽略集 | 遍历跳过 node_modules/dist/*.min.js/*.map 等 | 对齐"跳隐藏目录"既有策略，防无关文件污染搜索/速览 |
| B 本次落地 | file_tree | 目录树 + 每文件头部按预算拼"项目速览" | 快速建立结构感；文件头部摘要 + budget 截断 |
| B 本次落地 | gitignore 解析 | file 工具遍历遵循 .gitignore | 尊重用户既有忽略意图，与忽略集互补 |
| B ✅ 已实现 | propose_file_write/edit | 改动暂存 ProposedStore（ctx 注入），Run 结束后组合根确认落地 | 工具不碰盘，确认交互在用户侧（agent 零改动） |
| C 不做 | 缩进容错 | 严格失败后的整体缩进层级调整 | 与去空白兜底同性质（模糊匹配），已定案砍掉（§2.1 理由③） |
| C 不做 | 去空白兜底 | 剥空白匹配再反推真实位置 | 对代码是危险的模糊替换，已定案砍掉（§2.1 理由①） |
| C 不做 | referencedBy / BPE / 四级截断 / 内存文件树 | 符号索引系能力 | 依赖符号索引，量级不匹配（§2.1 理由②） |
| C 不做 | code_search 语义层 | embedding 向量检索 | roadmap 明确暂缓，bigram 关键词先顶着 |
| C 不做 | read_docs 专用 | 查第三方库文档 | file_read + doc_search + web_fetch 组合已覆盖 |

### 2.1 明确不做项及理由（评审结论）

> 评审来源：补充细节逐条评估（2026-08-28）。以下是明确砍掉的能力与理由，防止未来被"顺手带上"。

- **缩进容错（整体层级调整）**——理由：与去空白兜底同一性质（模糊匹配）。工具"猜缩进"后，命中位置的唯一性与"new_string 按猜测层级重写"的语义都难保证不产生模型没想改的改动；且 file_read 已在手，模型失败后看原文重试的成本极低。**已定案砍掉（2026-08-28 与持有人确认）**。
- **去空白兜底（剥空白匹配 + 反推真实位置）**——理由：剥掉全部空白再匹配本质是**模糊替换**，对代码文件风险高（空白是语法/格式的一部分，命中位置可能不是模型想要的）；"反推真实字节偏移"算法复杂且与"替换后保留原空白"的语义叠加，难验证正确性。本项目宁可"严格匹配失败 → 业务失败回灌 → 模型 file_read 看原文重试"，闭环更简单、结果更确定。**已定案砍掉（2026-08-28 与持有人确认）**。
- **referencedBy（符号被哪些文件引用）**——依赖符号索引 + 调用者映射（token 分数），量级不匹配；本项目搜索是关键词 bigram / 子串，无符号分析层。
- **BPE token 估算（GPT-4o BPE、1024 字符分块）**——与本项目"字符粗估"哲学直接冲突（truncateOutput 8000 字符、agent 预算字符估算）；精确 token 的收益在个人 CLI 场景不成立比。
- **四级截断（none → 删不重要 → 删低分符号 → 深度优先删最深）+ 符号分数排序 + 内存文件树**——全部依赖符号索引；file_tree 用"预算耗尽省略 N 项"即可，不建内存树、不算分数。

## 3. 常见实现方法与优缺点对比（选型依据）

> 本节汇总市面 agent 项目（Claude Code / Codex / Aider / OpenHands 等）对文件系统工具族的常见实现形态，逐项给优缺点；§4 各工具的选型结论即基于本节对比。

### 3.1 文件读取

| 形态 | 代表 | 优点 | 缺点 |
|---|---|---|---|
| 整读 + 截断 | 本项目 file_read 现状 | 实现最简单，一次给全貌 | 大文件浪费 token；读不到中间/尾部，只能从头看 |
| 窗口化读取（offset/limit） | Claude Code Read、Codex read | 按需取区间，token 受控，是标配 | 模型需多轮调用才能拼全貌；参数解释成本 |
| 头尾 + 命中行 | Claude Code Read 默认行为 | 省 token 且给关键锚点 | 实现稍复杂，需与 grep 协同定位 |

选型：**窗口化（offset/limit）**——参数直观（1-based 行号），与既有带行号渲染无缝叠加，向后兼容（不传即现行为）。补充：超界给提示、截断给续读建议（offset=end+1 或 doc_search），引导模型低成本续读。

### 3.2 目录树速览（read_subtree）

| 形态 | 代表 | 优点 | 缺点 |
|---|---|---|---|
| 纯目录树（tree 命令） | 常规 CLI | 结构清晰、token 极省 | 无内容，模型仍需逐个读 |
| 目录树 + 文件头部摘要 | 本项目 file_tree（拟） | 结构与要点兼得，一次建立全局感 | 预算控制要小心：预算太小太浅、太大超 token |
| 目录树 + 符号索引（ctags 类） | 语言服务器生态 | 精度高、定位准 | 重依赖、语言相关；本项目明确不做（§2.1） |

选型：**树 + 头部摘要 + 字符预算**（file_tree）。暂缓落地（B 档）：项目规模尚小，file_list + 逐个读撑得住。不做符号索引系增强。

### 3.3 写文件三种主流形态

| 形态 | 代表 | 优点 | 缺点 |
|---|---|---|---|
| 整体重写（write_file） | Claude Code Write | 实现最简、无解析错误、支持新建文件 | 大文件 token 贵；模型重写时易意外改动无关部分 |
| 字符串替换（str_replace） | Claude Code Edit | 精准、省 token、只动目标行 | 旧串必须逐字符匹配（模型易记错原文/缩进）；一处多命中需消歧 |
| unified diff / apply_patch | Aider、Codex CLI | 一次改多处、可读性好 | diff 解析脆弱，模型写 patch 语法错率高，需重试循环 |

选型：**整体重写 + 字符串替换并行，diff 不做**——两者覆盖"新建/重写"与"精准小改"两类主流场景；diff 的"一次改多处"价值会被多轮 file_edit 稀释，解析复杂度却不成立比。补充：file_edit 支持**多组替换**（replacements 数组）消解"一次改多处"的主要诉求，且不引入 diff 解析。

### 3.4 写操作的落地安全机制

| 形态 | 代表 | 优点 | 缺点 |
|---|---|---|---|
| 每步确认回调 | 本项目 exec 先例、Claude Code 默认 | 轻量、即时、fail-closed | 每步打断用户，批量任务繁琐 |
| 提议暂存 + 批量确认（propose store） | Claude Code Plan mode | 先看 diff 再落地、可批量、可反悔 | 需"确认回合"机制，交互复杂度高 |
| 沙箱 + 策略管道 | Codex sandbox、bubblewrap/landlock | 强制约束、无需人工逐项确认 | 重依赖、量级不匹配本项目 |

选型：**每步确认回调（先做）**；propose 机制等 roadmap 权限横切层落地时随统一 policy 升级——避免提前在 agent 循环里塞"确认回合"（YAGNI 门控）。

### 3.5 文件查找与搜索

| 形态 | 本项目现状 | 优点 | 缺点 |
|---|---|---|---|
| glob / 列目录 | file_list | 零依赖、结果稳定 | 无内容、不能按内容搜 |
| 子串 grep | doc_search | 零依赖、字面精确、够用 | 同义改写 / 语义相关搜不到 |
| ripgrep 二进制 | 无（可走 exec 白名单） | 快、正则、忽略规则完善 | 外部依赖 |
| 语义搜索（embedding） | 无 | 语义相关召回 | 索引维护、成本、语料量级门槛（roadmap 已判暂缓） |

选型：**保持 glob + 子串 grep**；语义搜索明确暂缓（与 roadmap 一致：语料量级不到，bigram 关键词先顶着）；rg 若将来需要，走 exec 白名单而非进工具。

### 3.6 第三方文档查询（read_docs）

| 形态 | 优点 | 缺点 |
|---|---|---|
| 项目内文档读取（file_read / doc_search） | 零新增、已覆盖 | 查不到外部库 |
| 在线抓取（web_fetch 抓 pkg.go.dev 等） | 覆盖全、零依赖 | 页面结构相关、慢、可能被反爬 |
| 本地预建文档索引（向量库） | 快、离线 | 索引维护成本、重依赖 |

选型：**组合（file_read + doc_search + web_fetch），不做专用工具**——覆盖"项目内文档 + 在线参考"两类场景，避免新增工具增加模型选择负担。

### 3.7 终端执行

| 形态 | 优点 | 缺点 |
|---|---|---|
| 无沙箱直跑 | 无限制、灵活 | 危险 |
| 白名单 + 超时 + 每步确认（本项目 exec 现状） | fail-closed、可控 | 白名单外命令用不了 |
| 真沙箱（bubblewrap/landlock/seccomp/docker） | 强制隔离 | 重依赖、学习成本 |

选型：**维持 exec 最小安全版**；真沙箱归 roadmap 安全护栏，量级不匹配，不提前做。

## 4. 工具规格

### 4.1 file_read 窗口化（改既有）

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| path | string | ✓ | 相对工作区根，或工作区外绝对路径（仅非敏感目录，2026-09-01 放宽） |
| offset | int | 否 | 起始行号（1-based），缺省 1 |
| limit | int | 否 | 读取行数，缺省读到文件尾（仍受既有 truncateOutput 截断） |

- 向后兼容：不传 offset/limit 即现行为（整读 + 行号 + 超长截断）。
- 渲染：仍带行号（`%5d│`）。
- **超界提示**：offset 超出总行数 → 返回 `[file_read: 共 N 行，offset=X 超出]`（业务失败回灌）。
- **续读建议**：limit 截断时，输出末尾追加 `（继续可 offset=N+1，或用 doc_search 定位）`——引导模型低成本续读，减少"以为读完了"的误判。
- **敏感文件防护**：路径命中 `.env*`（含 `.env` / `.env.example`）→ 直接拒绝（fail-closed），返回 `[file_read: 敏感文件不读取（IGNORED）]`。机密不进模型上下文。
- **整读参数化**：>10MB 的文件直接跳过（返回提示"文件过大，建议 doc_search 定位后窗口化读"）；整读截断保持前缀截断 + truncateOutput（8000 字符粗估，不引入 BPE，见 §2.1）。
- 忽略集说明：file_read 是**显式点名读取**，不受忽略集限制（用户明确要读就读）；忽略集只作用于 file_list/doc_search/file_tree 的**遍历**（§4.4）。

### 4.2 file_write（新增）

- **名称**：`file_write`（对齐 file_* 前缀，对应清单 write_file）
- **参数**：`path`（必填）、`content`（必填）
- **行为**：**整体覆盖**——文件不存在则创建，存在则覆盖。父目录必须已存在（**不自动建目录**，防误建）；写同目录临时文件 + `os.Rename` 原子替换，防半截文件。
- **权限**：Confirm 回调（Ask 语义）——展示 path + 新内容摘要，用户确认才执行；Confirm 为 nil 或返回 false → 拒绝执行（业务失败回灌，文件不动）。
- **返回**：写入成功提示 + 字节数。

### 4.3 file_edit（新增，对应清单 str_replace）

- **名称**：`file_edit`（对齐 file_* 前缀；如坚持清单命名 `str_replace` 可改，见 §10 待决）
- **参数**：
  - `path`（必填）
  - `replacements`（必填，≥1 组）：`[{ "old_string": "...", "new_string": "..." }, ...]`——多组一次提交，减少往返
  - `allow_multiple`（bool，默认 false）：true 时允许 old_string 多处命中并全部替换
- **行为**（逐组执行，单组失败不中止）：
  - 读文件 → 对每组 old_string 定位 → **唯一性校验**：
    - 0 命中：记入失败消息"未找到（可能缩进/空白不符）"。
    - 1 命中：替换。
    - 多命中（allow_multiple=false）：记入失败消息"出现 N 处，需用更大上下文消歧或开 allow_multiple"。
  - **行尾归一化**：匹配前把文件内容与 old_string 统一 `\r\n → \n` 后匹配；替换完成后把整份内容恢复为文件**原有的行尾风格**（CRLF 文件保持 CRLF）——跨平台文件不因替换引入行尾混乱。
  - **失败累积**：所有组的跳过原因（空 old_string / 未找到 / 无变化 / 多命中）收集进 messages；**全部失败**才返回 error `"文件无变更"`（业务失败回灌）；部分成功则返回成功组数 + 失败原因清单。
- **权限**：同 file_write（每步确认；确认回调 summary 给"变更摘要：第 1 组 old→new …"）。
- **返回**：成功组数 + 每组成败说明。

#### 4.3.1 缩进容错 / 去空白兜底（不做，已定案 2026-08-28）

- 两者均**砍掉**（理由见 §2.1 ③①）：本质都是模糊匹配，与"严格匹配 + 失败回灌"的确定性哲学冲突。
- file_edit 对"未找到"的失败消息提示"可能缩进/空白不符"，引导模型用 file_read 看原文后重试——把修正权交给模型，工具保持确定性。

### 4.4 file_tree（B 档规格，本次不实现，对应 read_subtree）

- **名称**：`file_tree`（对齐 file_* 前缀）
- **参数**：`path`（缺省工作区根）、`budget`（字符预算，缺省 8000 对齐 outputLimit）
- **行为**：递归遍历：目录列名字（带 `/`），文件读前若干行摘要；**应用常量忽略集**（node_modules/dist/*.min.js/*.map 等，§4.1 说明）+ 跳过隐藏目录；预算耗尽后输出"…省略 N 项"收尾。
- **取舍**：字符粗估，不精确 token（对齐 truncate 思路）；服务"快速建立项目结构感"，不替代 file_read 精读。**不做符号索引/分数排序/内存文件树**（§2.1）。

### 4.5 propose_file_write / propose_file_edit（✅ 已实现 2026-08-28）

- **语义**：改动暂存到 `ProposedStore`（Run 级 ctx 注入，`WithProposals`，形态对齐 PlanStore），**不碰磁盘**；用户确认后由组合根调 `ApplyProposed` 落地（复用 writeFileAtomic）。
- **落地路径（原卡点解决）**：不需要 agent 循环加"确认回合"——**确认交互在用户侧**：组合根在每轮 Run 结束后检查 `Pending()`，逐条展示摘要 + 读 stdin，确认即落地、拒绝即丢弃。模型不感知该交互（agent 零改动）。
- **提议时即校验**：propose_file_edit 暂存时就用 applyReplacements 验证替换并算好落地内容（无效提议当场拒绝），避免用户确认注定失败的改动。
- **fail-closed**：ctx 未注入 ProposedStore（组合根不处理提议）→ 工具拒绝。
- **定位**：这是"权限横切层"的最小形态——propose 把写决策权完全交给组合根（用户侧），与 exec/file_write 的每步 Confirm 同一安全取向；完整 policy 横切层（命令+工具统一）仍归 roadmap 安全护栏。

## 5. 安全边界（本次落地部分）

| 边界 | 界定 | 验证 |
|---|---|---|
| fail-closed | `FileConfig.Confirm == nil` → 不注册写工具（读工具照常） | 单测：无 Confirm 时 reg.Get 不到 file_write/file_edit |
| 每步确认 | 每次写前 Confirm 回调展示 path + 变更摘要 | 单测：Confirm 返回 false → 拒绝且文件内容不变 |
| 路径约束（写） | 复用 resolveInRoot（严格），写路径必须在工作区内 | 单测：越界写路径拒绝 |
| 路径约束（读） | resolveReadPath（2026-09-01 放宽）：工作区内放行；工作区外绝对路径须过敏感黑名单（fail-closed） | 单测：工作区外非敏感放行 / /etc、/proc 等拒绝 |
| 敏感目录黑名单 | 只读放宽后，/etc /proc /sys /usr /bin /sbin /boot /dev /root /var 前缀命中即拒绝 | 单测：isSensitivePath 各目录命中 |
| 原子写 | 同目录 temp + Rename，防半截文件 | 单测：写入后内容完整；中途失败不留 temp |
| 唯一性 | old_string 多命中拒绝（0 命中报未找到） | 单测：N=0/N=2 报错，N=1 成功 |
| 敏感文件 | `.env*` 拒绝读取（fail-closed），机密不进上下文 | 单测：file_read 对 .env 返回 IGNORED |
| 输出预算 | 返回信息走 truncateOutput | 复用既有 |

## 6. 依赖注入面

- `FileConfig` 扩字段：
  - `Confirm func(action, path, summary string) bool`——action ∈ "write" / "edit"，summary 为给用户看的变更说明（write 给新内容摘要，edit 给 old→new 摘要）；nil → fail-closed。
- `Deps.File` 结构体扩字段，`RegisterBuiltins` 签名不破（对齐 tool-extend.md 边界①的既定扩展点）。
- 组合根（main）：FileConfig 装配处注入 Confirm 实现（CLI 交互：打印变更摘要 + 读 stdin 确认）。

## 7. 实现要点

- 执行逻辑外置具名函数 `runFileWrite` / `runFileEdit`（可脱离工具壳独立单测，对齐既有惯例）。
- 原子写 helper：`writeFileAtomic(abs string, content []byte) error`（CreateTemp 同目录 → Write → Sync → Rename）。
- file_edit：读文件 → 行尾归一化探测（是否含 `\r\n`）→ 逐组匹配/替换（统一在 `\n` 空间操作）→ 恢复原行尾 → 整体原子写回。
- 敏感文件判断：`strings.HasPrefix(filepath.Base(abs), ".env")` 即拒绝（读工具共用同一防护）。
- 忽略集：包级常量 `ignoredPaths`（node_modules / dist / vendor / *.min.js / *.map 等）+ 既有隐藏目录跳过，file_list/doc_search/file_tree 共用。
- 工具构造仍走 `tool.NewFunc`；`RegisterBuiltins` 在 `deps.File != nil` 分支追加写工具（按 Confirm 是否注入决定）。

## 8. 测试计划

- file_read 窗口化：offset 边界（1 / 中间 / 越界提示）/ limit 截断 / 续读建议文案 / 缺省兼容（行为不回归）。
- file_read 敏感文件：`.env` / `.env.example` 拒绝；普通文件不受影响。
- file_write：新建 / 覆盖 / 父目录缺失报错 / 越界拒绝 / Confirm 拒绝（文件不变）/ 原子写完整性。
- file_edit：单组唯一命中 / 0 命中 / 多命中拒绝 / allow_multiple 全替换 / **多组部分成功（返回成功数 + 失败原因）** / 全失败 error / **CRLF 文件替换后保持 CRLF**。
- fail-closed：无 Confirm 不注册写工具（file.go 只读工具照常注册）。
- 依赖方向：builtin 仍只 import tool/memory（imports_test.go 不破）。

## 9. 与现有模块的关系

- **agent**：零改动（工具调用循环不感知，业务失败照常回灌）。
- **tool-extend.md**：本次是边界①（Deps 扩字段）+ 边界③（权限）的首个实例——Confirm 是"每步确认"最小审批形态，真 policy 横切层落地后替换，工具壳不动。
- **exec**：Confirm 先例复用（`ExecConfig.Confirm` → `FileConfig.Confirm`），fail-closed 语义完全一致。
- **trace**：写事件经 `WithToolObserver` 自动记录（入参/结果），无需改动。
- **session**：不感知（工具结果本就回灌模型，写入结果正常走对话历史）。

## 10. 待决

1. ✅ **命名**：file_write / file_edit（已定案 2026-08-28，file_* 前缀一致）。
2. ✅ **Confirm 回调签名**：`(action, path, summary)`（已定案 2026-08-28；summary 为变更摘要，够展示）。
3. ✅ **file_write 自动建目录**：不做（父目录缺失报错，防模型误建目录树）。

已定案（2026-08-28）：缩进容错、去空白兜底砍掉（§2.1）；其余不做项见 §2 分档表 C 档。

## 11. 落地步骤（审批后执行）

1. ✅ builtin/file.go：file_read 加 offset/limit + 超界提示 + 续读建议 + 敏感文件防护（2026-08-28）。
2. ✅ builtin/file_write.go：file_write 工具 + `writeFileAtomic` helper（2026-08-28）。
3. ✅ builtin/file_edit.go：file_edit 工具 + 多组替换 + 唯一性 + 行尾归一化 + 失败累积（2026-08-28）。
4. ✅ FileConfig 扩 Confirm；RegisterBuiltins 写工具按 fail-closed 注册（2026-08-28）。
5. ✅ main.go 注入 Confirm（CLI 确认交互：摘要 + stdin yes/no）（2026-08-28）。
6. ✅ 常量忽略集接入 file_list/doc_search（2026-08-28）。
7. ✅ builtin/file_tree.go：file_tree 工具（目录树 + 头部摘要 + budget 截断）（2026-08-28）。
8. ✅ builtin/file_ignore.go：gitignore 解析（够用版）接入 file_list/doc_search/file_tree 遍历（2026-08-28）。
9. ✅ builtin/propose.go：ProposedStore + WithProposals + propose_file_write/edit + ApplyProposed（2026-08-28）；file_edit.go 抽 applyReplacements 共用。
10. ✅ main.go：每轮注入 ProposedStore，Run 结束后确认落地（2026-08-28）。
11. ✅ 单测（§8）+ `go test -race ./...` / `go vet` / `gofmt` 全绿（2026-08-28）。
12. ✅ 读工具路径放宽（2026-09-01）：resolveReadPath 新增（工作区外放行 + 敏感目录黑名单），file_read/file_list/doc_search/file_tree 换用；写工具保持 resolveInRoot 严格；base 提示词加软确认约束；单测补充（§5 读路径/黑名单）。
13. ✅ 软确认提示词 + 合并门槛验证（2026-09-01）。

## 12. 待决追加（2026-09-01）

1. ✅ 读工具放宽范围：file_read/file_list/file_tree/doc_search 全部（用户确认）。
2. ✅ 安全防护：敏感系统目录黑名单 fail-closed（/etc /proc /sys /usr /bin /sbin /boot /dev /root /var；用户确认）。
3. ✅ 工作区外访问确认：软确认（base 提示词约束模型先征得用户同意，Codex 式问答；用户确认）。不做硬确认——现有权限表按工具名静态，路径级动态 Ask 需改权限模型，量级不匹配本期。
