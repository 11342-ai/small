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

// FileTree 构造目录树速览工具（file_tree）。
// 递归遍历：目录列名字（带 /）、文件附带头部摘要；应用隐藏目录/常量忽略集/gitignore；
// budget 字符预算耗尽后省略并提示（tool-fs.md §4.4）。服务"快速建立项目结构感"，
// 精读仍用 file_read；不做符号索引/分数排序/内存文件树（§2.1）。
func FileTree(cfg *FileConfig) tool.Tool {
	return tool.NewFunc(
		tool.Spec{
			Name:        "file_tree",
			Description: "以目录树速览工作区结构：目录列名字（带 /），文件附带前几行内容摘要；按 budget 字符预算截断（超预算省略并提示）。path 缺省工作区根。用于快速建立项目结构感，精读仍用 file_read。",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"path": {"type": "string", "description": "目录路径（相对工作区根），缺省工作区根"},
					"budget": {"type": "integer", "description": "输出字符预算，缺省 8000"}
				},
				"required": []
			}`),
		},
		func(_ context.Context, args json.RawMessage) (tool.Result, error) {
			return runFileTree(cfg, args)
		},
	)
}

// file 头部摘要参数：前 N 行、每行截断宽度（防单行超长刷爆预算）。
const (
	treeHeadLines = 2
	treeHeadWidth = 100
)

// runFileTree 目录树速览执行逻辑（外置具名函数，可独立单测）。
func runFileTree(cfg *FileConfig, args json.RawMessage) (tool.Result, error) {
	var in struct {
		Path   string `json:"path"`
		Budget int    `json:"budget"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return tool.Result{Data: "参数错误: " + err.Error(), IsError: true}, nil
	}
	p := strings.TrimSpace(in.Path)
	if p == "" {
		p = "."
	}
	abs, err := resolveInRoot(cfg.Root, p)
	if err != nil {
		return tool.Result{Data: err.Error(), IsError: true}, nil
	}
	budget := in.Budget
	if budget <= 0 {
		budget = outputLimit
	}
	root := filepath.Clean(cfg.Root)
	gi := loadGitIgnore(root)

	var b strings.Builder
	skipped := 0
	over := false
	_ = filepath.WalkDir(abs, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // 跳过不可读（如权限不足）
		}
		if path == abs {
			return nil // 起点自身不输出（名字由上下文可知）
		}
		// 忽略判断用相对工作区根的路径（gitignore 规则一致，含目录忽略）。
		relRoot, _ := filepath.Rel(root, path)
		relRoot = filepath.ToSlash(relRoot)
		if d.IsDir() {
			if skipIgnoredDir(abs, path, d.Name()) || gi.Ignored(relRoot, true) {
				return filepath.SkipDir
			}
		} else if ignoredFile(d.Name()) || gi.Ignored(relRoot, false) {
			return nil
		}
		// 预算耗尽：不再输出，仅统计剩余可见文件数；目录不下钻（SkipDir 防全树遍历）。
		if over {
			if !d.IsDir() {
				skipped++
			} else {
				return filepath.SkipDir
			}
			return nil
		}
		// 深度 = 相对遍历起点的路径段数（缩进两空格/层）。
		rel, _ := filepath.Rel(abs, path)
		indent := strings.Repeat("  ", strings.Count(filepath.ToSlash(rel), "/"))
		if d.IsDir() {
			line := indent + d.Name() + "/\n"
			if b.Len()+len(line) > budget {
				over = true
				return nil
			}
			b.WriteString(line)
			return nil
		}
		line := indent + d.Name() + "\n"
		for _, h := range fileHead(path, treeHeadLines, treeHeadWidth) {
			line += indent + "  " + h + "\n"
		}
		if b.Len()+len(line) > budget {
			over = true
			return nil
		}
		b.WriteString(line)
		return nil
	})

	out := strings.TrimSuffix(b.String(), "\n")
	switch {
	case out == "" && !over:
		out = "（空目录或无可见文件）"
	case over || skipped > 0:
		// 预算耗尽：区分"省略了 N 个文件"与"预算过小一个都没装下"。
		if out != "" {
			out += "\n"
		}
		if skipped > 0 {
			out += fmt.Sprintf("…（省略 %d 个文件，可调大 budget 查看）", skipped)
		} else {
			out += "…（预算耗尽，可调大 budget 查看）"
		}
	}
	return tool.Result{Data: truncateOutput(out)}, nil
}

// fileHead 读文件前 maxLines 行摘要（跳过空行、单行截断 maxWidth），供 file_tree 用。
// 读取失败返回 nil（目录树不因单文件不可读中断）。
func fileHead(path string, maxLines, maxWidth int) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []string
	for _, ln := range strings.Split(string(data), "\n") {
		if len(out) >= maxLines {
			break
		}
		trimmed := strings.TrimSpace(ln)
		if trimmed == "" {
			continue // 空行跳过，省预算
		}
		if r := []rune(trimmed); len(r) > maxWidth {
			trimmed = string(r[:maxWidth]) + "…"
		}
		out = append(out, trimmed)
	}
	return out
}
