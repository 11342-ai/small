# 计划工具（plan/todo）设计

> 位置：`internal/tool/builtin`（一个带状态的内置工具）
> 状态：**已实现**（2026-08-25 定稿并落地；§7 步骤全 ✅）
> 关联：`model/tool.md`（工具三件套）；`model/tool-extend.md`（边界⑤无状态 → 本设计靠 ctx 注入解决）；`model/trace.md`（执行元数据独立哲学）；`agent.go`（工具执行 ctx）

## 1. 结论先行

- 形态：**工具式**（Claude Code `todoWrite` 同款）——模型用函数调用自维护 Run 内分步清单，展示给用户看进度。
- 生命周期：**单 Run 内存**，Run 结束即弃，恢复会话不复活。
- 模块：**不单独开模块**——它是 builtin 一个带状态的工具 + 一个 ctx 挂载点。
- 为什么它和别的工具不一样：**有状态**，状态生命周期与 Run 绑定（tool-extend.md 边界⑤），靠 ctx 注入解决，agent 与 `RegisterBuiltins` 均零改动。

## 2. 定位与职责

**plan = Run 内分步执行清单，模型自维护，展示给用户。**

核心红线：**todo 是"执行元数据"，不是"回灌模型的上下文"**（与 trace 同一边界哲学——session 语义是回灌历史，混入计划会让模型把清单当事实学习）。

## 3. 状态挂载点（关键接法：ctx 注入）

工具 `Execute` 签名固定 `(ctx, args)`，状态无法经参数传入，走 ctx：

```go
// 组合根：每轮 Run 新建 store 放进 ctx，agent 零改动
runCtx := context.WithValue(ctx, planCtxKey, plan.NewStore())
result, err := a.Run(runCtx, input)

// builtin 工具：从 ctx 读 Run 级状态
func runPlanUpdate(store *plan.Store, args json.RawMessage) (tool.Result, error) { ... }
```

- **生命周期由"组合根每轮新建"天然保证**：Run 结束即弃，恢复会话不复活，无需清理逻辑。
- 绕开 tool-extend.md 依赖注入面①：`RegisterBuiltins` 签名不扩，工具只是从 ctx 取值。
- ctx key 用包级私有类型（`type ctxKey struct{}`），防外部碰撞。

## 4. 工具接口（已实现）

| 能力 | 语义 | 参数 |
|---|---|---|
| `plan add` | 追加一步 | `{"action":"add","text":"..."}` |
| `plan update` | 改状态（pending/in_progress/done） | `{"action":"update","id":1,"status":"done"}` |
| `plan list` | 返回当前清单 | `{"action":"list"}` |

- 状态模型：`[]Step{ID int, Text string, Status string}`，纯内存。
- 触发约束：Description 声明"仅多步任务时维护清单，单步任务不建"——防模型滥用（对齐 memory_save 的触发约束先例）。

## 5. 边界清单（界定 + 影响 + 验证）

| 边界 | 界定 | 影响 | 验证 |
|---|---|---|---|
| 数据边界 | todo 不进 session 历史、不进回灌 | 混入会让模型把计划当事实 | 单测断言 session 文件无 todo 字段 |
| 生命周期 | Run 内有效，恢复不复活 | 过期计划误导 | 单测：恢复会话后清单为空 |
| 可控性 | 模型维护，`maxToolRounds` 兜底 | 死循环更新/刷屏 | 已有兜底；todo 调用不占对话轮数 |
| 展示 | 只在状态变化时打印、输出行数上限 | REPL 刷屏 | 输出上限测试 |
| 正确性 | 清单声称 vs trace 实际执行 | 自欺 + 误导用户 | 与 trace 对拍（对比声称步骤 vs 执行轨迹） |

## 6. 与现有模块的关系

- **agent**：零改动（状态经 ctx 注入，工具循环不感知）。
- **session**：不写入——todo 是元数据，不是回灌内容。
- **trace**：已定**记录** plan 变更——`trace.Entry` 加 `Type` 字段（缺省 `"tool"`，plan 事件 `"plan"`），向后兼容旧文件；§5 正确性边界（与 trace 对拍）依赖此记录，实现 plan 时同步扩展。
- **tool-extend.md 边界⑤**：本设计是"有状态工具"的第一个实例，ctx 注入可复用为后续有状态工具（如 shell 的 cwd）的通用接法。

## 7. 落地步骤（✅ 已实现 2026-08-25）

1. ✅ `builtin/plan.go`：`PlanStore`（add/update/list + 纯内存）+ `WithPlan`/`planFromCtx`（ctx 注入 helper）
2. ✅ builtin 工具构造 `Plan()`，执行逻辑外置 `runPlan`（对齐 memory 工具模式）
3. ✅ 组合根：每轮 Run 新建 store 放 ctx（`builtin.WithPlan`）；`RegisterBuiltins` 无条件注册（无构造依赖）
4. ✅ REPL 展示：每次操作返回最新清单，observer 顺带打印（状态变化即展示）

## 8. 使用文档

### 8.1 工具声明

- **名称**：`plan`
- **触发约束**：仅**多步任务**时维护清单（模型依据 Description 决策）；单步任务不要建清单——防清单垃圾化。
- **权限**：Pass（清单是纯内存只读操作，无需 Ask 确认）。

### 8.2 参数

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `action` | string | ✓ | `add` / `update` / `list` |
| `text` | string | add 时必填 | 步骤描述（自动去首尾空白） |
| `id` | int | update 时必填 | 目标步骤 ID（从 1 起） |
| `status` | string | update 时必填 | `pending` / `in_progress` / `done` |

### 8.3 动作与返回

| 动作 | 语义 | 返回 |
|---|---|---|
| `add` | 追加一步（初始 `pending`） | 最新清单 |
| `update` | 改指定步骤状态 | 最新清单 |
| `list` | 查看当前清单（只读） | 最新清单 |

清单渲染格式：`1. ☐ 查文件`（☐=待办 / ◐=进行中 / ☑=完成）；空清单返回 `（当前无计划步骤）`。每次操作都返回最新清单——模型可跟踪进度，组合根 observer 顺带打印给用户（状态变化即展示）。

### 8.4 生命周期与边界

- **Run 级内存**：一次用户输入对应一个 Run，清单随 Run 结束丢弃；恢复会话后不复活（无过期计划误导）。
- **不进 session / 回灌**：清单是执行元数据，不写入会话历史、不注入模型上下文（§2 红线）。
- **fail-closed**：组合根未注入清单（无 Run 上下文）时工具拒绝，业务失败回灌。
- **trace**：每次变更以 `type: "plan"` 落盘，可与工具调用轨迹对拍（§6）。

### 8.5 使用示例（对话）

```
用户：帮我整理这个项目的结构，并给出下一步建议
模型：plan add "扫描目录结构"
     → 1. ☐ 扫描目录结构
      exec ls
     → 1. ☐ 扫描目录结构
      plan add "分析模块依赖"
     → 1. ☐ 扫描目录结构
       2. ☐ 分析模块依赖
      plan update 1 done
     → 1. ☑ 扫描目录结构
       2. ☐ 分析模块依赖
     ...
     （任务收尾时 plan update 2 done）
```

## 9. 待决

无（工具命名与 trace 记录已定案 2026-08-25，见 §4/§6；RunScope 泛化移入 §10 观察项）。

## 10. 观察项

- **RunScope 泛化**：ctx 是否泛化为通用 Run 作用域（未来 shell cwd、进度事件都挂这）——等第二个有状态工具出现再评估（YAGNI 门控），不主动做。
