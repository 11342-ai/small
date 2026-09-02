# GUI 界面（浏览器 app-server，--gui 可选入口）

> 位置：`internal/gui`（http server + embed 单页）、`internal/agent`（回复流式透出，小改）、`main.go`（--gui 入口分支）
> 状态：设计（2026-09-01，待用户确认后实现）
> 关联：`model/workflow.md`（工作流分支，GUI 复用以命令触发）、`model/cli.md`（命令系统）、`internal/persona`（embed 模式，前端单页对齐）、`Zoo/model/tool-lit.md`（doc_* 工具，markdown 展示数据源）

## 1. 结论先行

目标：加一个 GUI 界面解决两个真实痛点——对话中文乱码（终端无解，浏览器 UTF-8 天然解决）与 markdown 可视化阅读（doc_parse 产物的自然配套）。后期可展示 mp4。

形态（2026-09-01 用户拍板）：
1. 浏览器 app-server：Go net/http 起本地 server + 单个 embed HTML 页面（前端手写，不引框架）。零新增依赖——net/http 标准库 + goldmark（kb 已引入）。
2. 对话 SSE 流式：agent 透出回复增量回调（对齐 WithToolObserver 模式），前端打字机效果。
3. 入口：`go run . --gui` 可选启动；CLI 主路径原样保留（GUI 与 CLI 是平行 I/O 层）。
4. 范围焊死：输入 + 展示 + 命令切换，其他不做。

## 2. 需求理解

用户（2026-09-01）：
- 展示 markdown 文件；展示对话（解决乱码）；后期可看 mp4。
- 命令为先，通过命令切换页面展示的东西；不考虑复杂交互；就是"输入 + 展示"。
- 外部依赖小、降低复杂度；常见 app-server 模式。

## 3. 模块是否该加（回答用户纠结）

- 实质影响：GUI 是纯展示层，不触碰 agent/工具/权限核心。乱码在终端无解（编码/字体属终端环境），浏览器天然解决；markdown 可视化是文档链路的配套。
- 不破坏风格：两条前提——可选入口（--gui，CLI 主路径不动）+ 零新增依赖（标准库 + 已有 goldmark，前端手写单页）。
- 真正风险是范围蔓延：文档锁死"输入+展示+命令切换"，mp4 只留后期位（HTML video 标签天然承载）。

## 4. 架构

### 4.1 agent 回复流式透出（小改，对齐 WithToolObserver）

现状：adapter.completeViaStream 把增量攒成完整 Result，agent.Run 返回整体，GUI 拿不到中间态。

改动：
- providerChat（adapter）加字段 `replyObs func(string)`；completeViaStream 的 OnContent 逐段回调。
- Agent 加 `WithReplyObserver(obs func(string))` Option + 字段；New 时经类型断言注入 adapter（同包未导出类型，接口不变）；nil 时不触发（纯 CLI 零回归）。
- 只透 content 增量；thinking 不透（保持简洁，Result.Thinking 仍整体返回）。
- 非流式降级路径无增量：GUI 兼容——该轮整段回复作为单事件推送（前端照常渲染）。

### 4.2 gui 包（internal/gui）

依赖方向：main → gui → agent/session/tool/config（展示层，组合根装配注入）。gui 是叶子展示层，不反向依赖。

端点：
- GET / → embed 单页 HTML（对话 + 展示双区）。
- POST /chat → 对话流：body 为 {message}；server 执行 agent.Run，过程中把事件写成 HTTP 流式响应（text/event-stream）：
  - `event: reply` + `data: <content 增量>`（agent 流式回调逐段）；
  - `event: tool` + `data: <工具名/摘要>`（WithToolObserver 复用，展示工具调用过程）；
  - `event: done` + `data: <完整回复>`（结束标记，前端兜底）。
  - 前端 fetch + ReadableStream 逐事件渲染（单请求流式返回，不引 SSE 专用库）。
- GET /view?path=<路径> → markdown 展示：goldmark 渲染为 HTML 返回（工作区或缓存产物路径，复用 resolveReadPath 语义校验）。mp4 后期位：识别视频后缀返回 video 页。

### 4.3 命令切换展示（命令为先）

前端输入框统一收命令：
- `/view <path>` → 前端切换到展示区，fetch /view?path= 渲染 markdown（或后期 mp4）。
- 其余 `/xx` 与普通文本 → 走 /chat 对话流（workflow 分支、/pdf 等既有命令语义由 agent/对话处理）。
- 对话区与展示区并存：对话流实时在上，/view 切换下方展示区内容。

### 4.4 GUI 命令响应（v3，2026-09-01）

问题：v1/v2 前端与 server 只内置 /pdf /help /session /tools 等少量命令，/persona、/clear 等 CLI 命令在 GUI 不可用（走"未知命令"分支）。

方案（v3）：GUI 命令处理改为**注入 CLI 命令注册表**——组合根复用 `cmdXxx` 构造函数注册一套命令，经 `CommandFunc` 桥接进 gui.Server（Dispatch 结果 → SSE 事件；errInject 注入消息走 agent）。GUI 命令 = CLI 命令全集（/help /session /tools /clear /persona /pdf），语义与 CLI 完全一致：
- gui.Config 增 `Command func(ctx, input) (handled, output, inject, err)`，由 main 注入（main 认识 errInject，gui 不感知）。
- /persona、/clear 等经 cmdReg.Dispatch 执行（依赖 agent/mgr/store/compose 由 main 闭包绑定）。
- Ask 命令（/clear）GUI 下 confirm 回调返回 false → 拒绝并提示（无 stdin 确认，对齐写工具策略）。
- /view 仍由前端拦截（server 端防御性提示）。

### 4.5 配置迁移（v2，2026-09-01）

端口/网址从 flag 迁移到 config.yml，对齐"目录/预算只认 config.yml + 内置默认值"的配置来源单一原则（同删除环境变量决策）：
- config.yml 加 `gui_addr`（缺省 `127.0.0.1:8090`）；删除 `--gui-addr` flag。
- `--gui` 保留为启动模式开关（flag，行为开关——对齐 --session/--persona 同属启动 flag）。

### 4.6 GUI 展示增强（v4，2026-09-01）

三个展示层问题与修复（纯 gui 包，不触碰核心）：

1. 刷新丢历史：会话持久化与 agent 内存历史都在，但前端刷新即空白。新增 GET /history → agent.History() 过滤 user/assistant 返回 JSON，前端加载时渲染（范围仅对话消息，tool 事件不恢复）。
2. SSE 换行丢失：reply 增量含换行时，SSE data 行被 \n 打断、前端按块只取首行 data → 内容堆一起。修复：前端 handleEvent 收集块内全部 data: 行，按 \n 拼接（SSE 规范：多行 data 合并即含换行）。
3. 展示区无说明：/view 目标区默认空白。改为默认显示占位说明（"输入 /view <路径> 显示 markdown 或视频"），有内容后替换。
4. GUI 工具调用落盘：与 CLI 同一 `~/.small/sessions/<id>.trace.jsonl`（组合根注入，SSE 实时推送 + trace 写盘双通道，复用 appendTrace）。

## 5. 技术选型对比（2026-09-01 讨论结论）

| 方案 | 依赖 | 乱码 | markdown | 结论 |
|---|---|---|---|---|
| 浏览器 app-server（Go http + embed 单页 + goldmark） | 零新增 | 浏览器 UTF-8 解决 | 后端渲染强 | 选定 |
| 终端 TUI（bubbletea/tview） | 第三方库 | 仍依赖终端 | 弱 | 弃（乱码没解决） |
| Electron/webview | 重 | 解决 | 强 | 弃（依赖重） |

对话流式：SSE/HTTP 流式响应（复用 provider 流式，打字机体验）> POST 一次性（实现最简但无反馈）。
入口：--gui flag 可选 > 默认 GUI（改动面大，打断 CLI 习惯）。

## 6. 实现落点

| 改动 | 落点 |
|---|---|
| agent 回复流式透出 | agent.go（WithReplyObserver + 字段 + New 断言注入）、adapter.go（replyObs 字段 + OnContent 回调） |
| gui 包 | internal/gui/gui.go（server + 路由 + handleCommand 命令分发）、internal/gui/index.html（embed 单页）、internal/gui/markdown.go（goldmark 渲染） |
| 入口 | main.go：--gui flag，装配完成后分支起 server（复用 buildAgent） |
| 配置迁移（v2） | config.go（GUIAddr 字段 + gui_addr yaml + 缺省 127.0.0.1:8090）、main.go（删 --gui-addr，用 cfg.GUIAddr） |
| 命令响应（v3） | gui.go（Config.Command 桥接 + handleCommand 简化）、main.go（GUI 分支注册 cmdXxx 注入） |
| 展示增强（v4） | gui.go（/history 端点）、index.html（历史渲染/SSE 多行 data/占位说明） |
| 文档 | Zoo/model/gui.md（本文件）；CLAUDE.md 常用命令补 /pdf、--gui |

## 7. 测试计划

- agent：WithReplyObserver 回调收到流式增量（mock 流式 Completer）；nil 时不触发（回归）。
- gui：/chat 流式响应（httptest 读事件流，断言 reply/tool/done 事件）；/view goldmark 渲染（markdown → HTML 含标题/代码块）；路径越界拒绝。
- 入口：--gui 分支编译通过（main 无单测，手动验证起 server）。

## 8. 后期位（不做进本期）

- mp4：/view 识别视频扩展名返回 <video> 页。
- GUI 内 workflow 状态可视化（[进入分支:pdf] 标记高亮）。

## 9. 决策记录

1. 浏览器 app-server 形态（2026-09-01 用户确认）：net/http + embed 单页 + goldmark，零新依赖。
2. 对话 SSE 流式（2026-09-01 用户确认）：agent 透出回复增量，前端打字机。
3. --gui flag 可选入口（2026-09-01 用户确认）：CLI 主路径零改动。
4. 范围焊死：输入 + 展示 + 命令切换；mp4 仅留后期位（2026-09-01 用户确认）。
5. GUI 命令扩展集（2026-09-01 用户确认）：/pdf /help /session /tools + 未知报错，server 端分发。
6. 配置迁移（2026-09-01 用户确认）：gui_addr 进 config.yml（缺省 127.0.0.1:8090），删 --gui-addr flag。
7. GUI 命令复用 CLI 命令表（2026-09-01 用户确认）：注入 cmdXxx 注册表经 CommandFunc 桥接，/persona /clear 等 GUI 可用，语义与 CLI 一致。
8. 展示增强（2026-09-01 用户确认）：/history 恢复 user/assistant 对话、SSE 多行 data 拼接修复换行、展示区占位说明。
