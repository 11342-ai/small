package builtin

import (
	"small/internal/memory"
	"small/internal/tool"
)

// RegisterBuiltins 把全部内置工具批量注册进 reg。
// 内置工具清单由本包集中持有（权责与组合根分离）：新增内置工具时只需在下方
// 追加一个元素，组合根（main）的装配代码无需改动。
// mem 是记忆仓库（结构协作对象显式入参）：传 nil 则不注册记忆工具，
// 其余内置工具不受影响（退化行为，同 agent 的 tools/store 传 nil 惯例）。
// 命名与 Registry.RegisterAll（批量原语）区分：这里是"注册全部内置工具"的入口。
func RegisterBuiltins(reg *tool.Registry, mem *memory.Store) error {
	// 内置工具清单集中持有：新增内置工具在此追加一个元素（与 Echo/MemorySearch
	// 同款"一工具一构造函数"），组合根（main）的装配代码无需改动。
	tools := []tool.Tool{Echo(), GetCurrentTime()}
	if mem != nil {
		// 记忆工具依赖仓库实例：装配了仓库才注册（mem 传 nil 退化，只注册其余工具）。
		tools = append(tools, MemorySearch(mem), MemoryGet(mem), MemorySave(mem))
	}
	return reg.RegisterAll(tools...)
}
