# TUI 方向探索与决策记录

> 位置：无独立模块（组合根 REPL 形态不变）
> 状态：**探索记录**（2026-08-25 讨论沉淀，不排期实现）
> 关联：`路线图.md` §1 接入面（现只有 CLI）；`agent.go` 观察者模式（`WithToolObserver`/`WithReplyObserver` 同款）

## 1. 结论先行

- **现阶段保持逐行 REPL**（`bufio.Scanner` + `fmt.Printf`），零新依赖，能跑就行。
- 出现"resize 不错乱 / 多面板 / 流式原地渲染 / 滚动历史"等**真实需求**时 → 上 bubbletea，agent 零改动（观察者模式已预留）。
- 概念模型（块/条）成立，**写文档不写代码**——落地时抄框架，不自己造。

## 2. 关键认知（为什么这么定）

### 2.1 resize 错乱的本质

"拉伸时编码错乱"来自三个点：CJK 宽字符被当 1 列、SIGWINCH 后不整屏重绘、ANSI 样式未清理。标准解是框架内部做好的：**单元格网格 + 收到 resize 整帧重绘 + 宽度按 runewidth 计算**。自己写 = 把框架已填的坑重新挖一遍。

### 2.2 显示文字的坑位分布

| 世界 | 字形渲染 | 宽度计算 | 结论 |
|---|---|---|---|
| Web | 浏览器免费 | 免费 | 没人觉得难 |
| 终端 TUI | 终端模拟器免费 | 要自己算（CJK 双宽） | **坑最浅**，runewidth 一个函数填坑 |
| 像素 GUI（clay/Nuklear） | 全要自己来 | 要自己算 | 字体渲染链路是深坑 |

### 2.3 "精美 TUI"的真相 = 调包 + 设计系统 + 迭代

| 项目 | 底层 |
|---|---|
| OpenAI Codex CLI | Ink（React 渲染器） |
| charmbracelet Crush | bubbletea + bubbles + lipgloss + glamour |
| OpenCode | Ink |

没人给生产 AI agent 手写裸 ANSI。精美 = 框架兜住正确性（resize/CJK/事件循环）+ 设计系统（lipgloss/glamour，Charm 打磨五年）+ 产品迭代。**调包是正确工程决策，不是贬义。**

## 3. 概念模型：块（容器）/ 条（内容）

- **块**：背景 + 布局容器，管空间分配与滚动裁剪，不管文字。
- **条**：内容视图，管文字样式，只向块上报自身尺寸。
- **"块是平衡的"** = flexbox `grow` / grid `fr` 权重分配。

等价物验证（该切法在各界都成立）：

| 本模型 | HTML | tview | ratatui | 像素世界 |
|---|---|---|---|---|
| 块 | `<div>` | Box | Block | Nuklear 窗口 / clay 容器 |
| 条 | 文本节点 | TextView | Paragraph | Nuklear 控件 / clay 文本元素 |

易漏的四样（决定"不错乱"的关键，也是自研成本大头）：**布局引擎、文本模型（CJK 换行）、事件循环、尺寸传播（resize→重排）**。

教训：clay（4.8k 行）也只做布局引擎、渲染/测量全交出去——**分层是对的，自研是错的**。

## 4. 选型对照

| 方案 | 依赖 | 你写什么 | 结论 |
|---|---|---|---|
| 裸 ANSI | 0 | 全部（termios/resize/宽字符/渲染/按键） | 只做 300 行 spike 学原理，别产品化 |
| tcell 中间层 | 1 | 布局 + 状态 | 控制感路线，风险中低 |
| bubbletea / tview | 3–4 | Model/View | **产品路线（推荐）** |

## 5. 衍生想法（暂缓清单）

- **升级 bash**（颜色/readline/流式/斜杠命令）：复杂度不值，暂缓；真要动从"命令系统 + 颜色"起，readline 是补全的前提（raw mode 才能收到 Tab）。
- **独立终端窗口**：污染问题 alt-screen 已解决（退出恢复原样）；要独立会话用 tmux；**app 不负责开窗口**（平台差异/SSH 无显示/env 丢失）。
- **命令补全**：路线 = 先建命令系统（现只有硬编码 exit）→ readline Completer 回调（**候选来自真实状态**：`reg.List()`/`mgr.List()`/`store.List()`）→ 上下文排序 → AI 推荐慎上（量级不匹配）。命令系统设计见 `model/cli.md`。
- **HTML 布局联系**：块/条 = div/文本 成立；Textual（Python，终端真 CSS）与 go-tui（HTML 语法 + yoga flexbox）是实践；照搬不了的部分：字符网格 vs 像素、键盘 vs 鼠标、字体不可控。

## 6. 市面项目参考

- **bubbletea + bubbles + lipgloss + glamour**（Charm 全家桶）——Go TUI 事实标准，v2（2026-02）性能大提升。
- **tview**（k9s 在用）、**ratatui**（Rust，resize/宽字符处理标杆）、**Textual**（Python，终端 CSS）。
- **clay / Nuklear**（像素 immediate mode GUI）——验证"容器/内容"分层，但不解决 TUI。
- **go-tui**——HTML-like 声明式 + flexbox，含 inline 流式聊天示例。

## 7. 落地触发条件与步骤（需要时才做）

1. **触发**：用户要求流式逐字渲染 / 多面板 / 滚动历史，或 REPL 的妥协（流式时不能打字）无法接受。
2. **步骤**：bubbletea 包展示层（历史区 + 输入区 + 状态栏）→ `WithToolObserver` 事件接渲染 → 流式加 `WithReplyObserver`（与 ToolObserver 完全同款模式，agent 循环逻辑零改动）。
3. **依赖方向不变**：展示层走组合根装配，agent/会话/provider 一行不动，隔离点模板不变。

## 8. 红线

- 不手写裸 ANSI 渲染、不自研布局引擎（clay 的教训：4.8k 行只做布局）。
- app 不负责 spawn 终端窗口（那是 tmux/用户/终端模拟器的事）。
- 命令补全前先有命令系统；AI 推荐在量级匹配前不碰。
- "块/条"概念只进文档，实现永远用框架。
