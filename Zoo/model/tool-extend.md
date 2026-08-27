# 工具拓展路线与边界清单

> 位置：`internal/tool/builtin`（拓展落点）
> 状态：**路线记录**（2026-08-25 讨论沉淀，未实现）
> 关联：`model/tool.md`（模块本体）；`model/cli.md`（RequiresConfirm 权限种子）；`路线图.md`（安全护栏）；`model/kb.md`（doc 命令）

## 1. 结论先行

加"文件读/搜索/抓网页"这类工具，tool 模块**零改动**——"builtin 追加一个元素，组合根不改一行"的承诺成立。真正触发改动的边界只有三点：**依赖注入面、权限、同步性**（见 §4）。

## 2. 推荐工具分档

**A 档：无痛扩展（零模块改动）**

| 工具 | 干什么 | 参考项目 | 依赖 |
|---|---|---|---|
| `file_read` | 读任意文本文件（行号/截断） | Claude Code Read、Codex | os，stdlib |
| `file_list` | 列目录/glob 找文件 | Claude Code Glob、Codex | filepath，stdlib |
| `doc_search` | 跨目录关键词搜索（Zoo/、代码） | Claude Code Grep、Codex grep | filepath+regex |
| `web_fetch` | GET URL → 纯文本 | Claude WebFetch、Codex | net/http + 超时/大小上限 |
| `calculator` | 安全数学表达式求值 | Claude、OpenHands | 自写小求值器（Go 无 eval，工作量较大） |

**B 档：触发设计边界（等权限/依赖就绪）**

| 工具 | 卡点 |
|---|---|
| `file_write`/`file_edit` | 破坏性 → 需要审批（接 cli.md `RequiresConfirm` 种子 / roadmap 安全护栏） |
| `exec` | ✅ **已实现最小安全版**（2026-08-25）：白名单 + 超时 + 每步确认，fail-closed，argv 执行非 shell；完整沙箱仍归 roadmap 安全护栏 |
| `web_search` | 需要外部搜索 API key（新依赖 + 成本决策） |

**C 档：暂缓/不做**——subagent（roadmap 独立项）、browser/computer use（量级不匹配）、MCP（另行评估）。

## 3. 开源项目标配（参考线）

| 项目 | 核心工具集 |
|---|---|
| Claude Code | Bash、Read、Write、Edit、Glob、Grep、WebSearch、WebFetch、TodoWrite、Task |
| Codex CLI | shell、read、write、apply_patch、grep、web search |
| OpenHands | bash、文件操作、web 搜索、浏览器 |
| OpenClaw | 记忆、web、computer use、消息通道、GitHub |
| Cline | 终端、文件读写、浏览器、web 搜索、MCP |

规律：**文件 + Shell + Web 是标配，记忆/规划/子代理是差异化**。本项目已有记忆（还领先多数项目），缺文件读 + web 抓取；shell 最后一块，等权限。

## 4. 模块边界清单（要不要改模块）

模块壳（`Spec`/`Execute`/`Registry`）不用动，边界在五点：

| 边界 | 触发点 | 接法 |
|---|---|---|
| ① 依赖注入面 | 新工具要新依赖（根目录/HTTP 配置/key）→ `RegisterBuiltins(reg, mem)` 签名要扩 | 第三个依赖类型出现时升级为 deps struct |
| ② 同步执行 | 长任务（web_fetch 10s）阻塞 agent 循环、无进度 | 先接受阻塞；要流式再仿 `WithReplyObserver` 加事件 |
| ③ 无权限 | 破坏性工具（file_write/shell）没审批 | cli.md `RequiresConfirm` 种子 + roadmap 安全护栏（横切） |
| ④ 结果单字符串 | 大文件/结构化输出超 `Data` 上限 | 截断 + 摘要（truncate 思路已有） |
| ⑤ 无状态 | 跨调用状态（shell 的 cwd）无处安放 | 显式注入，别在工具内藏全局（首个实例：`model/plan.md` 的 ctx 注入） |

关键判断："builtin 追加一个元素"的承诺在**工具只需 tool/memory** 时成立；工具要新依赖类型时 `RegisterBuiltins` 扩参——这是设计里写好的扩展点，不是缺陷。最可能先踩到 ①（file 工具要根目录）与 ③（写文件要审批）。

## 5. 工作区定位 + 路径约束 ≠ 沙箱（概念辨析）

全局运行（二进制入 PATH）时数据层已就绪：会话/记忆/配置都在 `~/.small`（全局），与 CWD 无关。真正依赖 CWD 的只有 file 工具——那时要回答"文件根在哪"：

- **CWD**（`os.Getwd()`）≠ **二进制位置**（`os.Executable()`）：PATH 只影响"能找到 binary"，不影响程序在哪工作；CWD 由启动它的 shell 决定。
- **工作区定位**：从 CWD 往上找 marker（`.git`/`go.mod`）定位文件根（Claude Code/Codex 同款）。这是一个**小函数**，不是模块；只有"多工作区切换"才值得模块化。

和沙箱的关系（常被混淆）：

| | 工作区/路径约束 | 沙箱 |
|---|---|---|
| 回答 | 我在哪（声明性上下文） | 你能碰什么（强制性能力限制） |
| 性质 | 约定，程序自己遵守 | 强制，越界即失败 |
| 例子 | file 工具只在 workspace 内 | bubblewrap/landlock/seccomp、审批门 |
| 违反后果 | 无"违反"，只是解析结果 | 系统拒绝/权限错误 |

结论：**"限制工具在 CWD/工作区内"只是沙箱的一层（路径 policy），不是沙箱**；真沙箱还覆盖网络/exec/资源/超时。路径约束属 roadmap 安全护栏的 policy 层，与 cli.md `RequiresConfirm` 同一棵树。

## 6. 建议落点

1. 先加 `file_read` + `doc_search`——纯 stdlib、零风险、直接服务"看文档/改代码"核心场景。
2. 再加 `web_fetch`——单工具高价值，注意超时 + 返回大小上限。
3. `file_write`/`shell` 等 roadmap 安全护栏落地后再上。
4. 第三个依赖出现时，`RegisterBuiltins` 升 deps struct（一改两处，向后兼容）。
3. ✅ `exec` 最小安全版已实现（2026-08-25，见 §2 B 档）；`file_write` 等 roadmap 安全护栏落地后再上。
4. ✅ `RegisterBuiltins` 已升 deps struct（`builtin.Deps{Mem, Exec}`，2026-08-25，exec 触发）。

## 7. 待决

- `file_read`/`doc_search` 的搜索根目录：走工作区定位（§5，CWD 往上找 marker），具体 marker 选型（`go.mod` vs `.git`）待定。
- `calculator` 求值器实现成本：无 eval，自写小解析器 vs 暂缓，待定。
