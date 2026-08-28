package builtin

import (
	"small/internal/memory"
	"small/internal/tool"
)

// Deps 内置工具的依赖集合（tool-extend.md 边界①：第三个依赖类型出现，RegisterBuiltins 升 deps struct）。
// 新增内置工具需要新依赖类型时在此扩展；对应字段为 nil 则退化（不注册该工具，同 mem 惯例）。
type Deps struct {
	// Mem 记忆仓库：nil 则不注册记忆工具。
	Mem *memory.Store
	// Exec exec 工具配置：nil 则不注册 exec（不注入确认就没有 exec，fail-closed）。
	Exec *ExecConfig
	// File 文件类工具配置（工作区根）：nil 则不注册 file_read/file_list/doc_search。
	File *FileConfig
}

// RegisterBuiltins 把全部内置工具批量注册进 reg。
// 内置工具清单由本包集中持有（权责与组合根分离）：新增内置工具时只需在下方
// 追加一个元素，组合根（main）的装配代码无需改动。
// 命名与 Registry.RegisterAll（批量原语）区分：这里是"注册全部内置工具"的入口。
func RegisterBuiltins(reg *tool.Registry, deps Deps) error {
	// 内置工具清单集中持有：新增内置工具在此追加一个元素（与 Echo/MemorySearch
	// 同款"一工具一构造函数"），组合根（main）的装配代码无需改动。
	tools := []tool.Tool{Echo(), GetCurrentTime(), Plan(), WebFetch()}
	if deps.Mem != nil {
		// 记忆工具依赖仓库实例：装配了仓库才注册（mem 传 nil 退化，只注册其余工具）。
		tools = append(tools, MemorySearch(deps.Mem), MemoryGet(deps.Mem), MemorySave(deps.Mem))
	}
	if deps.Exec != nil {
		tools = append(tools, Exec(*deps.Exec))
	}
	if deps.File != nil {
		tools = append(tools, FileRead(deps.File), FileList(deps.File), DocSearch(deps.File))
	}
	return reg.RegisterAll(tools...)
}
