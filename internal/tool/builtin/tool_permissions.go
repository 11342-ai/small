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
	"kb_tree":            policy.Pass, // 只读查询（结构树/域视图），无副作用
	"kb_refs":            policy.Pass, // 只读反链报告（删除检查前置，无副作用）
	"kb_check":           policy.Pass, // 只读巡检，无副作用
	"kb_write":           policy.Ask,  // 写知识库文件，有副作用，必须每步确认
	"exec":               policy.Ask,  // 有副作用 + 越界面大，必须每步确认
	"file_write":         policy.Ask,
	"file_edit":          policy.Ask,
	"file_diff":          policy.Pass, // 只读比对两个文本文件，无副作用
	"doc_parse":          policy.Pass, // 解析只读 + 写受控缓存目录（tool-lit.md §5，类比 memory_save 写归档层）
	"doc_read":           policy.Pass, // 只读缓存产物，无副作用
	"doc_clean":          policy.Pass, // 原地清洗缓存产物（受控缓存目录内），无工作区副作用

	"k8s_pod":      policy.Pass,
	"k8s_workload": policy.Pass,
	"k8s_events":   policy.Pass,
	"k8s_logs":     policy.Pass,
	"k8s_metrics":  policy.Pass,
	"k8s_node":     policy.Pass,
	"k8s_nodes":    policy.Pass, // 列全部节点（只读），Pending 归因的入口
	"k8s_evidence": policy.Pass, // 只读采集 + 写自有受控目录（~/.small/k8s），对集群无副作用
	"k8s_report":   policy.Pass, // 只写自有受控目录的报告文件，对集群无副作用
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
