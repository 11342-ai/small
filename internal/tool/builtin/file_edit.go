package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"small/internal/tool"
)

// FileEdit 构造字符串替换编辑工具（file_edit）。
// 设计要点（tool-fs.md §4.3）：多组 replacements 顺序执行、默认唯一匹配（多命中拒绝、
// allow_multiple 显式放行）、行尾归一化（CRLF 文件匹配后恢复原风格）、失败累积
// （单组失败不中止，全部失败才返回"文件无变更"）。确认已由 agent 权限横切层提供
// （file_edit 为 Ask，见 Zoo/model/policy.md）。
func FileEdit(cfg *FileConfig) tool.Tool {
	return tool.NewFunc(
		tool.Spec{
			Name:        "file_edit",
			Description: "在工作区内按字符串替换编辑文件：replacements 为 [{old_string, new_string}] 多组替换（默认每组 old_string 必须唯一命中；出现多次需带更大上下文消歧，或置 allow_multiple=true 全部替换）。每次编辑需用户确认。用于修改代码/配置的局部内容，比整文件重写省 token、改动精准。",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"path": {"type": "string", "description": "文件路径（相对工作区根）"},
					"replacements": {"type": "array", "items": {"type": "object", "properties": {"old_string": {"type": "string", "description": "要替换的原文（须与文件内容逐字符一致）"}, "new_string": {"type": "string", "description": "替换后的内容"}}, "required": ["old_string", "new_string"]}, "description": "替换组（≥1 组，顺序执行，前一组结果作为后一组输入）"},
					"allow_multiple": {"type": "boolean", "description": "允许 old_string 多处命中并全部替换（默认 false）"}
				},
				"required": ["path", "replacements"]
			}`),
		},
		func(_ context.Context, args json.RawMessage) (tool.Result, error) {
			return runFileEdit(cfg, args)
		},
	)
}

// editReplacement 一组 old→new 替换（与 JSON 字段同名，json.Unmarshal 直接映射）。
type editReplacement struct {
	Old string `json:"old_string"`
	New string `json:"new_string"`
}

// applyReplacements 在文件内容上应用多组替换：行尾归一化（CRLF 统一到 \n 空间匹配，
// 落盘前恢复原风格）、逐组顺序执行、失败累积。返回替换后完整内容、成功组数、失败原因。
// file_edit（落盘）与 propose_file_edit（暂存）共用同一替换语义（tool-fs.md §4.3/§4.5）。
func applyReplacements(data []byte, reps []editReplacement, allowMultiple bool) (content string, done int, failed []string) {
	crlf := bytes.Contains(data, []byte("\r\n"))
	content = string(data)
	if crlf {
		content = strings.ReplaceAll(content, "\r\n", "\n")
	}
	for _, r := range reps {
		old, new := r.Old, r.New
		if old == "" {
			failed = append(failed, "old_string 为空")
			continue
		}
		old = strings.ReplaceAll(old, "\r\n", "\n") // 模型给的片段同样归一化后匹配
		n := strings.Count(content, old)
		switch {
		case n == 0:
			failed = append(failed, fmt.Sprintf("未找到 %q（可能缩进/空白不符，可先 file_read 看原文）", truncateOutput(old)))
		case n > 1 && !allowMultiple:
			failed = append(failed, fmt.Sprintf("%q 出现 %d 处，需带更大上下文消歧或置 allow_multiple=true", truncateOutput(old), n))
		default:
			if old == new {
				failed = append(failed, fmt.Sprintf("%q 无变化（old_string 与 new_string 相同）", truncateOutput(old)))
				continue
			}
			if allowMultiple {
				content = strings.ReplaceAll(content, old, new)
			} else {
				content = strings.Replace(content, old, new, 1)
			}
			done++
		}
	}
	if crlf {
		content = strings.ReplaceAll(content, "\n", "\r\n")
	}
	return content, done, failed
}

// runFileEdit file_edit 执行逻辑（外置具名函数，可独立单测）。
func runFileEdit(cfg *FileConfig, args json.RawMessage) (tool.Result, error) {
	var in struct {
		Path          string            `json:"path"`
		Replacements  []editReplacement `json:"replacements"`
		AllowMultiple bool              `json:"allow_multiple"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return tool.Result{Data: "参数错误: " + err.Error(), IsError: true}, nil
	}
	if strings.TrimSpace(in.Path) == "" {
		return tool.Result{Data: "参数错误: path 为空", IsError: true}, nil
	}
	if len(in.Replacements) == 0 {
		return tool.Result{Data: "参数错误: replacements 为空", IsError: true}, nil
	}
	abs, err := resolveInRoot(cfg.Root, strings.TrimSpace(in.Path))
	if err != nil {
		return tool.Result{Data: err.Error(), IsError: true}, nil
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return tool.Result{Data: "读取失败: " + err.Error(), IsError: true}, nil
	}
	// 多组替换（行尾归一化空间内）：propose_file_edit 共用同一逻辑（tool-fs.md §4.3/§4.5）。
	content, done, failed := applyReplacements(data, in.Replacements, in.AllowMultiple)
	if done == 0 {
		return tool.Result{Data: "文件无变更：\n- " + strings.Join(failed, "\n- "), IsError: true}, nil
	}
	if err := writeFileAtomic(abs, []byte(content)); err != nil {
		return tool.Result{Data: "写入失败: " + err.Error(), IsError: true}, nil
	}
	out := fmt.Sprintf("已编辑 %s（替换 %d 组）", in.Path, done)
	if len(failed) > 0 {
		out += "；跳过：\n- " + strings.Join(failed, "\n- ")
	}
	return tool.Result{Data: out}, nil
}

// writeFileAtomic 原子写：同目录临时文件 → Sync → 保留原权限（新建 0644）→ Rename。
// 任一路径失败都清理临时文件；Rename 保证目标文件要么是旧内容、要么是完整新内容
// （防半截文件，tool-fs.md §5）。file_write 未来复用同一 helper。
func writeFileAtomic(abs string, content []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(abs), ".small-write-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // 失败清理；成功后已 Rename，Remove 报错可忽略
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// 覆盖场景保留原文件权限，新建用 0644（CreateTemp 默认 0600，需显式调）。
	mode := os.FileMode(0o644)
	if fi, err := os.Stat(abs); err == nil {
		mode = fi.Mode().Perm()
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		return err
	}
	return os.Rename(tmpName, abs)
}
