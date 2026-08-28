package builtin

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"time"

	"small/internal/tool"
)

// ExecConfig exec 工具的安全配置（最小安全版：白名单 + 超时 + 每步确认，三把锁）。
type ExecConfig struct {
	// Allow 命令白名单（按命令名，fail-closed：空或不含即拒绝）。
	Allow []string
	// Timeout 单次执行超时上限；<=0 用默认 30s。
	Timeout time.Duration
	// Confirm 执行前确认回调（nil-safe）：返回 true 放行；nil 时一律拒绝（fail-closed）——
	// 不注入确认就没有 exec，保证不会出现"裸奔版"。
	Confirm func(cmd string, args []string) bool
}

// defaultExecTimeout exec 默认超时。
const defaultExecTimeout = 30 * time.Second

// Exec 构造命令执行工具（exec）。最小安全版（见 Zoo/model/tool-extend.md B 档）：
// 白名单 + 超时 + 每步确认，缺一即拒绝（fail-closed）。
// 刻意不用 shell（sh -c）解析——直接 argv 执行，白名单按命令名生效，
// 避免 shell 语法拼接绕过白名单；参数也不做 shell 解释，减少注入面。
func Exec(cfg ExecConfig) tool.Tool {
	return tool.NewFunc(
		tool.Spec{
			Name:        "exec",
			Description: "在本地执行白名单内的命令并返回输出（如 ls/cat/grep/echo/date/pwd）。每步执行前需用户确认，命令必须在白名单内否则拒绝。用于查文件、看环境、跑只读命令；被拒时说明原因，可尝试换白名单内命令。",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"command": {"type": "string", "description": "白名单内的命令名"},
					"args": {"type": "array", "items": {"type": "string"}, "description": "命令参数（可选）"}
				},
				"required": ["command"]
			}`),
		},
		func(ctx context.Context, args json.RawMessage) (tool.Result, error) {
			return runExec(ctx, cfg, args)
		},
	)
}

// runExec exec 执行逻辑（外置具名函数，可脱离工具壳独立单测）。
func runExec(ctx context.Context, cfg ExecConfig, args json.RawMessage) (tool.Result, error) {
	var in struct {
		Command string   `json:"command"`
		Args    []string `json:"args"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return tool.Result{Data: "参数错误: " + err.Error(), IsError: true}, nil
	}
	cmd := strings.TrimSpace(in.Command)
	if cmd == "" {
		return tool.Result{Data: "参数错误: command 为空", IsError: true}, nil
	}
	// 白名单（fail-closed）：命令名不在 Allow 内直接拒绝，不给确认机会。
	if !execAllowed(cfg.Allow, cmd) {
		return tool.Result{Data: "exec 拒绝：命令 " + cmd + " 不在白名单内", IsError: true}, nil
	}
	// 每步确认（fail-closed）：Confirm 为 nil 或用户拒绝都不执行。
	if cfg.Confirm == nil || !cfg.Confirm(cmd, in.Args) {
		return tool.Result{Data: "exec 拒绝：未获确认", IsError: true}, nil
	}
	// 超时：取配置与 ctx 中较早的截止（外部 ctx 取消同样生效）。
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultExecTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, err := exec.CommandContext(runCtx, cmd, in.Args...).CombinedOutput()
	if err != nil {
		if runCtx.Err() == context.DeadlineExceeded {
			return tool.Result{Data: "exec 超时（超过 " + timeout.String() + "）", IsError: true}, nil
		}
		return tool.Result{Data: "exec 失败: " + err.Error() + "\n" + truncateOutput(string(out)), IsError: true}, nil
	}
	return tool.Result{Data: truncateOutput(string(out))}, nil
}

// execAllowed 判断命令名是否在白名单内（顺序无关，O(n) 对命令数足够）。
func execAllowed(allow []string, cmd string) bool {
	for _, a := range allow {
		if a == cmd {
			return true
		}
	}
	return false
}
