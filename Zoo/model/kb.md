# 知识库规范（KB 格式约定）

> 位置：无独立模块（约定文档，落地于 Zoo/ 与 memory/ 文件树）
> 状态：**约定定稿**（2026-08-25 讨论沉淀）
> 关联：`model/tui.md`（查看层）；`model/memory.md`（agent 索引层）；`model/persona.md`（frontmatter 先例）

## 1. 结论先行

人和 agent 共用的 markdown 知识库，格式上限一句话：

> **纯 md 文件树 + 相对路径链接（可带 #锚点）+ YAML frontmatter 特定标记，不支持任何自定义语法。**

再复杂一格（JSON 知识库、图数据库、向量持久化文件）就是"坐牢"：要写解析器、查看器、迁移，agent 也不能再直接 grep。

## 2. 为什么是 md（最低公共分母）

| 消费者 | 需求 | md 满足 |
|---|---|---|
| 人 | 直接读、IDE/glow 渲染 | ✅ |
| agent | grep/ripgrep、工具读取 | ✅ |
| 双向同步 | 一份文件两个消费者，无迁移 | ✅ |

Obsidian/Logseq/Foam/Dendron 全部选 md 的原因：**结构化程度由"人 + grep"共同决定，不是由"数据库能力"决定**。

## 3. 格式上限：三条能力（正好 Obsidian 内核）

| 能力 | 纯 md 实现 | 人用 | agent 用 |
|---|---|---|---|
| 链接跳转 | `[说明](路径.md#标题)` | 渲染点击 | `doc_read` 按相对路径解析 |
| 文本嵌入 | `![[笔记]]` 或直接引用路径 | 渲染展开 | 读链接文件内容带回（= memory 注入思路） |
| 特定标记 | YAML frontmatter（tags/状态/关联） | 渲染元数据栏 | 解析为上下文（= persona 先例） |

### 3.1 链接约定（重要）

- 用**相对路径** `[x](docs/foo.md#小节)`，不用 Obsidian `[[wiki-link]]`。
- 理由：agent 解析相对路径 = `filepath.Join` 一次；wiki-link 需要解析器查笔记位置，多一步自定义逻辑。
- `#锚点` 指向标题（`# 小节`）：人点击跳转，agent 可定位。

### 3.2 frontmatter 约定

- 头部 `---\nkey: value\n---`，字段保持最小：`tags`/`status`/`关联`。
- 不发明 schema；缺省字段零回归（对齐 persona 的缺省语义）。

## 4. 边界红线

- 不发明自定义语法（`.kb.json`、特殊指令、专有链接协议）。
- 不上 JSON 知识库/图数据库/向量持久化文件——量级不匹配（对齐 memory.md §9：向量等语料大了再评估）。
- 元数据只走 frontmatter，正文保持纯 markdown。

## 5. 现状盘点与缺口

| 层 | 现状 |
|---|---|
| 存储（Zoo/ + memory/*.md + MEMORY.md） | ✅ 已是 md 文件树 |
| 索引（agent） | ✅ bigram + memory_search/get/save |
| 查看（人） | ⚠️ IDE + glow 可看，缺 `doc <path>` 终端命令 |
| agent 翻全库 | ❌ 缺 `doc_read`/`doc_list` 工具（现有 memory 工具只管 memory/ 子目录） |

## 6. 落地路径（需要时才做）

1. **阶段一**：`doc <path>` 命令（glamour 渲染任意 md 文件）→ 人看层最小闭环（命令系统见 `model/cli.md`）。
2. **阶段二**：`doc_read`/`doc_list` 工具 → agent 可翻 Zoo/ 全库（对齐 builtin 三件套模板：声明/执行/注册）。
3. **阶段三**：文件树浏览 TUI（yazi 式）→ 查看层完整形态（触发条件见 `model/tui.md`）。

## 7. 待决

- Zoo/ 是否纳入 agent 检索范围（现 memory 只管 memory/ 目录）：倾向纳入，待阶段二评估。
- 链接失效检测（断链扫描）：YAGNI，暂不做。
