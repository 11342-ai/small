package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"small/internal/tool"
)

// FileWrite 构造整体覆盖写工具（file_write）。
// 写操作 fail-closed 同 file_edit（tool-fs.md §4.2/§5）：cfg.Confirm 为 nil 时
// 本工具不注册（register.go 判断），执行层同样防御——Confirm 缺失/拒绝一律不落盘。
// 设计要点：整体覆盖（新建或替换）、**不自动建目录**（父目录必须已存在，防模型误建
// 目录树）、原子写（复用 writeFileAtomic，防半截文件）、保留原权限。
func FileWrite(cfg *FileConfig) tool.Tool {
	return tool.NewFunc(
		tool.Spec{
			Name:        "file_write",
			Description: "在工作区内整体写入文件：content 覆盖整个文件（文件不存在则创建，存在则覆盖；父目录必须已存在，不自动建目录）。每次写入需用户确认。用于创建新文件或整体重写现有文件；只改局部内容时优先用 file_edit（更省 token、改动精准）。",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"path": {"type": "string", "description": "文件路径（相对工作区根）"},
					"content": {"type": "string", "description": "写入的完整内容（可为空串，用于创建空文件或清空）"}
				},
				"required": ["path", "content"]
			}`),
		},
		func(_ context.Context, args json.RawMessage) (tool.Result, error) {
			return runFileWrite(cfg, args)
		},
	)
}

// runFileWrite file_write 执行逻辑（外置具名函数，可独立单测）。
func runFileWrite(cfg *FileConfig, args json.RawMessage) (tool.Result, error) {
	var in struct {
		Path    string  `json:"path"`
		Content *string `json:"content"` // 指针区分"未传"与"空串"（空串=清空/建空文件，合法）
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return tool.Result{Data: "参数错误: " + err.Error(), IsError: true}, nil
	}
	if strings.TrimSpace(in.Path) == "" {
		return tool.Result{Data: "参数错误: path 为空", IsError: true}, nil
	}
	if in.Content == nil {
		return tool.Result{Data: "参数错误: content 缺失", IsError: true}, nil
	}
	abs, err := resolveInRoot(cfg.Root, strings.TrimSpace(in.Path))
	if err != nil {
		return tool.Result{Data: err.Error(), IsError: true}, nil
	}
	// 不自动建目录：父目录缺失直接拒绝（防模型凭猜测路径误建目录树，tool-fs.md §4.2）。
	if fi, err := os.Stat(filepath.Dir(abs)); err != nil || !fi.IsDir() {
		return tool.Result{Data: fmt.Sprintf("父目录不存在：%s（file_write 不自动建目录，可先 file_list 确认路径）", filepath.Dir(in.Path)), IsError: true}, nil
	}
	if err := writeFileAtomic(abs, []byte(*in.Content)); err != nil {
		return tool.Result{Data: "写入失败: " + err.Error(), IsError: true}, nil
	}
	return tool.Result{Data: fmt.Sprintf("已写入 %s（%d 字节）", in.Path, len(*in.Content))}, nil
}
