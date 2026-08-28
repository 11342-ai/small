# 权限横切层（工具接入）方案

> 位置：`internal/policy`（已有种子）+ `internal/agent`（拦截点）+ `internal/tool/builtin`（权限表）+ `main`（装配）
> 状态：**已实现（2026-08-28 落地）**：builtin.ToolPermissions 权限表 + agent 拦截点 + exec/file 确认迁移 + main 装配；§7 步骤全 ✅
> 关联：`model/cli.md`（§5 权限四决策 + 三预留位，本方案实现"工具接入位"）；`model/tool-fs.md`（propose = 权限横切最小形态，本方案通用化）；`Zoo/路线图.md`（安全护栏）；`CLAUDE.md`（约定）

## 1. 结论先行

- **拦截点**：agent 工具循环（`t.Execute` 之前）——执行任意工具前按工具名查权限表：`Pass` 直接放行 / `Ask` 走确认回调（无回调则拒绝，fail-closed）。
- **权限声明**：`builtin.ToolPermissions` 静态表（按工具名 → `policy.Permission`），**不进 `Tool.Spec`**（cli.md 预留位 1 已定：Spec 序列化进模型上下文，内部策略不污染模型视角）。
- **权限集合**：只 `Pass` / `Ask`（与命令层一致，Deny 留注释位，YAGNI）。
- **确认回调**：`func(name string) bool`，与命令层 `Registry.Confirm` 同签名——main 收敛为同一实现。
- **迁移**：exec / file_write / file_edit 工具内部 `Confirm` 字段**一步到位移除**，保护由 agent 权限层兜底（不再依赖"组合根是否记得注入"）。
- **propose 落地确认保留**：propose 工具的"Run 后落地确认"是落地语义（不同概念），不并入工具执行确认。
- 依赖方向：`agent → policy`、`builtin → policy`（policy 保持叶子，不 import 内部包）。

## 2. 现状盘点（为什么要统一）

当前 4 处确认机制各自为政：

| 位置 | 机制 | 问题 |
|---|---|---|
| 命令层 | `Spec.Perm` + `Registry.Confirm`（走 policy） | ✅ 已有正确形态 |
| exec | `ExecConfig.Confirm` 回调 | 工具内部各自实现 |
| file_write/file_edit | `FileConfig.Confirm` 回调 | 同上 |
| propose 落地 | main 里手写 if 块 | 特例逻辑 |

不统一的问题：确认语义重复实现、新增 Ask 工具要再抄一份回调、组合根要分别记得注入、无法集中审计。

## 3. 设计

### 3.1 权限表（builtin 静态声明）

```go
// internal/tool/builtin/tool_permissions.go
// ToolPermissions 内置工具权限表（静态声明，按工具名；cli.md §5 预留位 1：
// 工具权限在注册/执行侧声明，不进 Tool.Spec）。全量列举——新增工具必须登记，
// register 时校验"注册的工具都在表内"防漏。
var ToolPermissions = map[string]policy.Permission{
	"echo": policy.Pass, "plan": policy.Pass, "get_current_time": policy.Pass,
	"file_read": policy.Pass, "file_list": policy.Pass, "file_tree": policy.Pass,
	"doc_search": policy.Pass, "web_fetch": policy.Pass,
	"propose_file_write": policy.Pass, "propose_file_edit": policy.Pass, // 只暂存不落盘
	"memory_search": policy.Pass, "memory_get": policy.Pass, "memory_save": policy.Pass,
	"exec": policy.Ask, "file_write": policy.Ask, "file_edit": policy.Ask,
}
```

- `RegisterBuiltins` 注册后校验：注册的每个工具名都存在于 `ToolPermissions`（防新增工具漏登记 → 缺省 Pass 裸奔）。
- 后续新增 Ask 工具：登记进表即可，工具内部不再写确认逻辑。

### 3.2 agent 拦截点（核心改动）

```go
// internal/agent —— 新增字段 + Option
type Agent struct {
	...
	perms map[string]policy.Permission // 工具权限表；nil = 全 Pass（旧行为）
	confirm func(name string) bool     // Ask 确认回调；nil-safe，Ask 且 nil → 拒绝
}

func WithToolPermissions(m map[string]policy.Permission) Option { ... }
func WithToolConfirm(fn func(name string) bool) Option { ... }

// 工具循环执行点（t.Execute 之前）：
func (a *Agent) guard(call ToolCall) (tool.Result, bool) {
	if a.perms != nil {
		switch a.perms[call.Name] { // 缺省 Pass
		case policy.Ask:
			if a.confirm == nil || !a.confirm(call.Name) {
				return tool.Result{Data: call.Name + " 执行被拒绝：未获确认", IsError: true}, false
			}
		}
	}
	return tool.Result{}, true
}
```

- **拒绝语义**：业务失败回灌（`Result{IsError}`），模型收到拒绝后自行调整（对齐 exec 现状）。
- **nil 表 = 全 Pass**：组合根不注入权限表时行为与现状完全一致（测试兼容）。
- **Ask 无确认回调 = 拒绝**：fail-closed 由 agent 层硬约束，不依赖组合根是否记得注入。

### 3.3 迁移（exec / file / propose）

| 项 | 改法 |
|---|---|
| `ExecConfig.Confirm` | 移除字段；exec 工具删除内部确认逻辑 |
| `FileConfig.Confirm` | 移除字段；file_write/file_edit 删除内部确认逻辑 |
| `RegisterBuiltins` | 不再按 `Confirm == nil` 判断注册写工具（权限层兜底）——File 配置存在即注册全部 |
| propose 落地 | 保留 main 的 Run 后确认（落地语义 ≠ 工具执行确认，两回事） |
| main | `agent.New(..., WithToolPermissions(builtin.ToolPermissions), WithToolConfirm(confirmName))`；确认交互复用命令层 `confirmName`，收敛为一处 |

### 3.4 模块组合与依赖

```
main → agent（注入权限表 + 确认回调） → tool.Registry（执行）
  ↘ builtin.ToolPermissions（静态声明） → policy（类型）
main → command → policy
```

- `policy` 保持叶子：只加类型/常量，判定逻辑在消费方（agent）。
- `builtin` 新增对 `policy` 的 import（`builtin → policy`，叶子对叶子，不破红线）。
- `agent` 新增对 `policy` 的 import（`agent → policy`，单向，不破红线）。

## 4. 风险与预防

| 风险 | 预防 |
|---|---|
| 改 agent 工具循环 → 全工具回归 | Pass 路径判定后直接放行（零额外开销）；现有测试全绿；新增权限层单测 |
| 工具内部 Confirm 未迁干净 → 双重弹窗 | 迁移一步到位；测试断言 exec/file 工具执行不再依赖 Confirm |
| Ask 工具确认回调 nil 被放行（fail-open） | agent 层硬约束 + 单测矩阵（nil 回调 / 拒绝 / 放行） |
| 新增工具漏登记权限表 → 缺省 Pass 裸奔 | 权限表全量列举 + RegisterBuiltins 校验（注册名 ∈ 表） |
| 把 Perm 误加到 Spec（违反 cli.md 预留位 1） | 文档明示 + 评审核对 |
| 确认交互代码重复 | main 收敛为 `confirmName` 一处（命令层已复用） |

## 5. 测试计划

- **agent 权限层**：Pass 放行 / Ask 确认放行 / Ask 拒绝回灌（IsError + 工具未执行）/ Ask 无回调拒绝（fail-closed）/ 表外工具缺省 Pass。
- **迁移回归**：exec/file_write/file_edit 现有单测更新为"不注入确认回调也能构造"，确认行为由 agent 层测试覆盖。
- **权限表校验**：register 后所有注册工具名 ∈ ToolPermissions；故意漏登记应测试失败。
- 依赖方向：imports_test 固化（builtin 仍只 import tool/memory/policy；agent 可 import policy）。
- 合并门槛：`go test -race ./...` / `go vet` / `gofmt`。

## 6. 与现有模块的关系

- **cli.md**：实现预留位 1（工具接入位）；Permission 类型复用，命令/工具共享判定（"平行共享层"落地）。
- **tool-fs.md**：propose 从"最小形态"升级为通用机制的实例——其落地确认保持独立。
- **trace**：权限拒绝是否记 trace？——拒绝未执行，不触发 observer（观察者只在实际执行后回调），保持现状。
- **session**：拒绝回灌照常进历史（业务失败语义），无特殊处理。

## 8. 预留位 2/3 方案（讨论定稿，未实现）

> 2026-08-28 讨论：项目规模尚小，动态覆盖不实现（YAGNI）；仅把设计形态固化，防未来拍脑袋。

### 8.1 预留位 2：Ask 粒度 / 会话内放行

- **默认以用户为主**（fail-closed 每步确认）：Ask 工具只有 3 个（exec/file_write/file_edit），个人 CLI 单用户，打断成本可接受——安全基线不妥协。
- **增强形态（将来）**：确认交互升级为 `[y/N/a]`——`a` = 本会话放行该工具（内存级 allow 集合，不落盘）。guard 判定顺序：会话 allow 集合命中 → 放行；否则静态表 Ask → 确认回调。
- 约束：会话内放行随 Run/进程结束失效（不写 config、不持久化），语义清晰无残留。
- 与"以完成任务为主"（全局自动放行）的区别：**全局自动放行不做**（与 fail-closed 红线冲突，等量级/明确要求再评估）。

### 8.2 预留位 3：动态覆盖（config.yml）

- **两层权限源 + 覆盖优先**：`有效权限 = 覆盖层命中 ? 覆盖值 : 静态表`。guard 判定是 O(1) 两层查找，**不需要预先合并成新 map**（无需"遍历存储再遍历放出"）。
- **不影响注册顺序**：注册（RegisterBuiltins）是启动期静态装配；动态覆盖是**运行期判定层叠加**，与注册解耦。依赖方向不破：组合根读 config 注入覆盖规则（如 allow 集合），agent 不感知 config 来源（对齐 confirm 回调接法）。
- **覆盖语义**：放宽（Ask→Pass）必须显式声明（如 `tools: {exec: always_allow}`）；收紧（Pass→Ask）自然生效。不允许"升权限的隐式规则"。
- 依赖方向：`main → config`（组合根读）+ `main → agent`（注入覆盖），`agent` 不 import `config`。

## 9. 落地步骤（✅ 已实现 2026-08-28）

1. ✅ `builtin/tool_permissions.go`：权限表 + RegisterBuiltins 校验（注册名 ∈ 表）。
2. ✅ `agent`：加 `perms`/`confirm` 字段 + `WithToolPermissions`/`WithToolConfirm` + `guard` 拦截（挂在 `t.Execute` 前）。
3. ✅ 迁移：`ExecConfig.Confirm` / `FileConfig.Confirm` 移除，exec/file_write/file_edit 内部确认逻辑清零，register 判断简化。
4. ✅ `main`：注入权限表 + `confirmName`；确认交互收敛（命令层 + 工具层共用）。
5. ✅ 单测（§5）+ 更新既有测试断言（register fail-closed 语义变更）。
6. ✅ 文档：cli.md §5 勾选预留位 1，policy.md 状态更新。
