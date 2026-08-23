package builtin

import "small/internal/tool"

// RegisterBuiltins 把全部内置工具批量注册进 reg。
// 内置工具清单由本包集中持有（权责与组合根分离）：新增内置工具时只需在下方
// 追加一个元素，组合根（main）的装配代码无需改动。
// 命名与 Registry.RegisterAll（批量原语）区分：这里是"注册全部内置工具"的入口。
func RegisterBuiltins(reg *tool.Registry) error {
	return reg.RegisterAll(
		Echo(),
	)
}
