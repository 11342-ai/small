// Package policy 权限判定。当前极薄：静态声明种子（Pass/Ask），
// 将来是 roadmap"安全护栏"模块的地基（见 Zoo/model/cli.md §5）。
// 依赖方向：叶子包，不 import 任何内部包；命令与工具将来共享本包判定（横切关注点）。
package policy

// Permission 一次操作需要的权限级别。
type Permission string

const (
	// Pass 白名单：直接放行（缺省）。
	Pass Permission = "pass"
	// Ask 询问：执行前需用户交互确认。
	Ask Permission = "ask"
)

// String 实现 fmt.Stringer，便于展示/落盘。
func (p Permission) String() string { return string(p) }
