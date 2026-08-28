package builtin

import (
	"fmt"

	"small/internal/policy"
	"small/internal/tool"
)

// ToolPermissions 内置工具权限表（静态声明，按工具名；cli.md §5 预留位 1：
// 工具权限在注册/执行侧声明，不进 Tool.Spec——Spec 序列化进模型上下文，
// 内部策略不污染模型视角）。全量列举：新增工具必须登记，
// RegisterBuiltins 注册后校验防漏（漏登记 → 缺省 Pass 裸奔）。
var ToolPermissions = map[string]policy.Permission{
	"echo":               policy.Pass,
	"plan":               policy.Pass,
	"get_current_time":   policy.Pass,
	"file_read":          policy.Pass,
	"file_list":          policy.Pass,
	"file_tree":          policy.Pass,
	"doc_search":         policy.Pass,
	"web_fetch":          policy.Pass,
	"propose_file_write": policy.Pass, // 只暂存不落盘，无副作用（落地确认在组合根）
	"propose_file_edit":  policy.Pass,
	"memory_search":      policy.Pass,
	"memory_get":         policy.Pass,
	"memory_save":        policy.Pass,
	"exec":               policy.Ask, // 有副作用 + 越界面大，必须每步确认
	"file_write":         policy.Ask,
	"file_edit":          policy.Ask,
}

// validateToolPermissions 校验：注册的每个工具都必须在权限表内——防"新增工具漏登记 →
// 缺省 Pass 裸奔"（写工具无确认直接执行）。注册期校验，fail-fast。
func validateToolPermissions(reg *tool.Registry) error {
	for _, spec := range reg.List() {
		if _, ok := ToolPermissions[spec.Name]; !ok {
			return fmt.Errorf("tool: %q 未登记权限（builtin.ToolPermissions 需全量列举）", spec.Name)
		}
	}
	return nil
}
