// Package command 命令系统壳：声明 + 注册 + 分发（解析 "/cmd args"）。
// 平行于 tool 骨架但独立实现：命令是用户触发（文本参数），工具是模型触发（JSON Schema），
// 共享骨架形态，不共享实现（见 Zoo/model/cli.md §2）。
// 壳无内部依赖：具体命令在组合根注册（闭包捕获 agent/store/mgr），Dispatch 只做文本解析。
package command

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"small/internal/policy"
)

// CommandSpec 一条命令的声明。
type CommandSpec struct {
	// Name 命令名，以 "/" 开头（如 "/clear"）。
	Name string
	// Usage 用法说明，/help 展示。
	Usage string
	// Perm 权限级别（缺省 Pass）；Ask 的命令执行前需经 Confirm 回调确认。
	Perm policy.Permission
	// Run 执行命令。args 为去掉命令名后的剩余参数（按空格切分）。
	// 依赖经闭包捕获注入（组合根注册时绑定），壳不感知具体依赖。
	Run func(ctx context.Context, args []string) (string, error)
}

// Registry 命令注册表：启动期注册、运行期只读（同 tool.Registry 约定，不加锁）。
type Registry struct {
	cmds map[string]CommandSpec
	// Confirm 询问确认回调（nil-safe）：Perm == Ask 的命令执行前调用，
	// 返回 true 放行、false 拒绝（拒绝不算错误）。nil 时 Ask 直接拒绝（fail-closed）。
	Confirm func(name string) bool
}

// New 构造空注册表。
func New() *Registry {
	return &Registry{cmds: make(map[string]CommandSpec)}
}

// Register 注册命令：空名、非 "/" 前缀、nil Run、重名均报错。
func (r *Registry) Register(cmd CommandSpec) error {
	if cmd.Name == "" {
		return errors.New("command: register command with empty name")
	}
	if !strings.HasPrefix(cmd.Name, "/") {
		return fmt.Errorf("command: name %q must start with /", cmd.Name)
	}
	if cmd.Run == nil {
		return errors.New("command: register command with nil Run")
	}
	if _, dup := r.cmds[cmd.Name]; dup {
		return fmt.Errorf("command: duplicate command name %q", cmd.Name)
	}
	r.cmds[cmd.Name] = cmd
	return nil
}

// List 返回全部命令声明，按名称升序（/help 展示与补全候选，顺序稳定）。
func (r *Registry) List() []CommandSpec {
	specs := make([]CommandSpec, 0, len(r.cmds))
	for _, c := range r.cmds {
		specs = append(specs, c)
	}
	sort.Slice(specs, func(i, j int) bool { return specs[i].Name < specs[j].Name })
	return specs
}

// HelpText 生成可用命令清单（/help 命令的实现载体）。
func (r *Registry) HelpText() string {
	var b strings.Builder
	b.WriteString("可用命令：")
	for i, c := range r.List() {
		if i > 0 {
			b.WriteString("；")
		}
		fmt.Fprintf(&b, "%s（%s）", c.Name, c.Usage)
	}
	return b.String()
}

// Dispatch 分发一行输入：以 "/" 开头且命中已注册命令则执行。
// 返回 handled=true 表示已处理（成功、被拒、未知命令均算已处理，不再走对话）；
// 非命令输入返回 handled=false（交由对话循环）。
// 未知命令（"/" 开头但未注册）返回 handled=true 且带错误——对齐组合根"位置参数防护"
// 哲学：报错暴露（附可用命令），不静默降级。
func (r *Registry) Dispatch(ctx context.Context, input string) (handled bool, output string, err error) {
	if !strings.HasPrefix(input, "/") {
		return false, "", nil
	}
	fields := strings.Fields(input)
	name := fields[0]
	cmd, ok := r.cmds[name]
	if !ok {
		return true, "", fmt.Errorf("未知命令 %q（%s）", name, r.HelpText())
	}
	// 权限：Ask 命令需确认；Confirm 为 nil 时 fail-closed（拒绝执行）。
	if cmd.Perm == policy.Ask {
		if r.Confirm == nil || !r.Confirm(cmd.Name) {
			return true, "", nil // 用户拒绝，不算错误
		}
	}
	out, err := cmd.Run(ctx, fields[1:])
	return true, out, err
}
