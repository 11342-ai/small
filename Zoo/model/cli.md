# CLI 命令系统设计

> 位置：`internal/command`（独立模块，平行 tool 骨架）+ `internal/policy`（权限种子）
> 状态：**已实现**（2026-08-25 定稿并落地；main 接线 + 命令表见 §4/§9）
> 关联：`model/tui.md`（命令补全第 0 步）；`model/kb.md`（阶段一 doc 命令）；`model/tool.md`（骨架对照）；`agent.go`（SetSystemPrompt 最小改动）

## 1. 结论先行

命令系统做成**独立模块 `internal/command`**，形态平行于 `tool` 骨架（声明 + 执行 + 注册），实现注册在组合根。这是命令补全的地基——没有命令词汇表就没有补全对象。

## 2. 命令 ≠ 工具（共享骨架，不共享实现）

| | 工具 tool | 命令 command |
|---|---|---|
| 调用方 | 模型（function calling） | 用户（直接敲） |
| 参数 | JSON Schema（provider 契约） | 命令行文本 |
| 结果 | `Result{Data,IsError}` 回灌模型 | 打印给用户 |

不复用 `tool` 包（Spec 绑 JSON Schema，混入会污染语义），建平行结构。

## 3. 模块形态

```go
// internal/command —— 壳，无内部依赖；依赖经闭包捕获注入（组合根注册时绑定）
type CommandSpec struct {
    Name  string
    Usage string
    Perm  Permission // 缺省 Pass；Ask 命令执行前需 Confirm 确认（见 §5）
    Run   func(ctx context.Context, args []string) (string, error)
}
type Registry struct{ ... }
func New() *Registry
func (r *Registry) Register(cmd CommandSpec) error
func (r *Registry) List() []CommandSpec
func (r *Registry) HelpText() string
func (r *Registry) Dispatch(ctx context.Context, input string) (handled bool, output string, err error)
// Registry.Confirm func(name string) bool —— Ask 确认回调（nil-safe，fail-closed）
```

- 依赖方向：`main → command → policy`，`main → agent`，单向，不破红线。
- 实现注册在组合根（闭包持有 agent/store/mgr），对齐 `builtin/RegisterBuiltins` 接法。
- 确认交互（Confirm 回调）在组合根注入（读 stdin），壳不感知 I/O。

## 4. 命令表

| 命令 | 语义 | 落点 |
|---|---|---|
| `/exit`（及裸 `exit`） | 退出 | main 循环 |
| `/persona <name>` | 仅"无消息会话"可注入人格（见 §6） | `mgr.Get` + `SetSystemPrompt` + 重写 meta |
| `/help` | 列命令表 | Registry 遍历 |
| `/clear` | 清历史 + 删会话文件 | `a.Reset()`，`Perm: Ask` |
| `/session` | 打当前会话 id | main 已持有 |
| `/tools` | 列可用工具 | `reg.List()` |

未知 `/xxx`：报错 + 列可用命令（对齐"位置参数防护"哲学：报错暴露，不静默降级）。

## 5. 权限：Permission 类型（静态种子，策略留横切层）

权限判定四个决策（2026-08-25 确认）：**静态声明 + 仅命令层 + 按工具名 + 平行共享层**——当前阶段最小版，不超前。

```go
// internal/policy —— 现在很薄，将来是"安全护栏"的地基
type Permission string

const (
	Pass Permission = "pass" // 白名单：直接放行（缺省）
	Ask  Permission = "ask"  // 询问：交互确认
)

func (p Permission) String() string { return string(p) }
```

- 统一替换早期 `RequiresConfirm bool`（同一概念，不留两个）。
- 权限是**横切关注点**：命令与工具都是受管对象，注册各自分开（命令≠工具），判定走同一 policy。

**三个预留位（现在留"位"，不实现）**：

1. **工具接入位**：`Tool.Spec` 将来加 `Permission` 字段（tool-extend.md 边界③ 的接法），命令/工具共用一个检查函数。
2. **粒度升级位**：Ask 从命令层提升到工具层时，agent 循环加"挂起等确认"——exec 工具落地是触发器（届时评估）。
3. **动态化位**：静态 → 用户配置覆盖（config.yml 里 `shell = always allow`，Claude/Cline 同款）——roadmap"安全护栏"模块的事。

开源参照：Claude Code（每工具 allow/ask/deny + 用户覆盖）、Cline（每工具 approval 开关）——我们抄"静态声明 + 按工具名"，降级"工具层 Ask 与沙箱"（量级不匹配，exec 触发再评估）。

## 6. /persona 窗口语义（核心决策）

- 定型点从"创建时刻"挪到"第一条消息"：新建会话不立即 WriteMeta，首条消息时写入。
- `/persona` 仅在 `len(a.History()) == 0` 时生效，否则报"会话已定型"。
- 无消息 = 无串味，不违反"一个对话一个人格"。

## 7. agent 最小改动

```go
// system 是字段（allTurns 每次现拼），运行时换 prompt 不影响历史完整性
func (a *Agent) SetSystemPrompt(p string) { a.system = p }
```

## 8. 与命令补全的关系

命令词汇表（Registry 遍历）+ 系统状态候选（`reg.List()`/`mgr.List()`/`store.List()`）= 未来 readline Completer 的候选源（见 `model/tui.md` §5）。

## 9. 落地步骤（✅ 已实现 2026-08-25）

1. ✅ `internal/command`：Registry + Dispatch（解析 `/cmd args`）+ `internal/policy`（Permission 类型）
2. ✅ meta 定型时机后移（新建会话不立即写，首条消息前写）
3. ✅ agent 加 `SetSystemPrompt` + 测试
4. ✅ 组合根注册命令（`/clear` 走 `Perm: Ask` 确认）+ `/exit /help /session /tools /persona`

## 10. 待决

- `/tools` 是否带详情参数：YAGNI，先列名字。
- `doc <path>` 命令归属：kb.md 阶段一，依赖 glamour，量级匹配后再进命令表。
