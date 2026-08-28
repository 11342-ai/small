package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"small/internal/tool"
)

// FileConfig 文件类工具的工作区根（路径 policy：只允许访问 Root 内，tool-extend.md §5）。
// Root 由组合根经工作区定位注入（main.detectRoot：go.mod 优先、.git 兜底、CWD 最后）。
// Root 为 nil 时工具不注册（退化，同 mem/exec 惯例）。
type FileConfig struct {
	Root string
}

// resolveInRoot 把相对（或 Root 内绝对）路径解析为 Root 内的绝对路径。
// 越界（.. 逃逸 / 绝对路径在 Root 外）返回 error——路径约束，防工具读工作区外文件。
func resolveInRoot(root, p string) (string, error) {
	if root == "" {
		root = "."
	}
	var abs string
	if filepath.IsAbs(p) {
		abs = filepath.Clean(p)
	} else {
		abs = filepath.Clean(filepath.Join(root, p))
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return "", fmt.Errorf("路径解析失败: %w", err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("路径越界：%q 不在工作区（%s）内", p, root)
	}
	return abs, nil
}

// FileRead 读取工作区内文本文件（带行号、超长截断）。
func FileRead(cfg *FileConfig) tool.Tool {
	return tool.NewFunc(
		tool.Spec{
			Name:        "file_read",
			Description: "读取工作区内文本文件内容（带行号，超长截断）。path 相对工作区根（如 internal/agent/agent.go）或工作区内的绝对路径；仅允许访问工作区内文件。用于查看文档、代码、配置。",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"path": {"type": "string", "description": "文件路径（相对工作区根）"}
				},
				"required": ["path"]
			}`),
		},
		func(_ context.Context, args json.RawMessage) (tool.Result, error) {
			return runFileRead(cfg, args)
		},
	)
}

// runFileRead 文件读取执行逻辑（外置具名函数，可独立单测）。
func runFileRead(cfg *FileConfig, args json.RawMessage) (tool.Result, error) {
	var in struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return tool.Result{Data: "参数错误: " + err.Error(), IsError: true}, nil
	}
	abs, err := resolveInRoot(cfg.Root, strings.TrimSpace(in.Path))
	if err != nil {
		return tool.Result{Data: err.Error(), IsError: true}, nil
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return tool.Result{Data: "读取失败: " + err.Error(), IsError: true}, nil
	}
	// 带行号渲染（人读友好），整体超限截断（tool-extend.md 边界④）。
	lines := strings.Split(string(data), "\n")
	var b strings.Builder
	for i, ln := range lines {
		fmt.Fprintf(&b, "%5d│ %s\n", i+1, ln)
	}
	return tool.Result{Data: truncateOutput(strings.TrimSuffix(b.String(), "\n"))}, nil
}

// FileList 列目录或按 glob 找文件（相对 Root，支持 ** 跨目录）。
func FileList(cfg *FileConfig) tool.Tool {
	return tool.NewFunc(
		tool.Spec{
			Name:        "file_list",
			Description: "列出工作区内文件：path 为目录时列出其下条目；含 glob 通配符（* ? **）时按模式递归找文件（如 internal/**/*.go）。path 相对工作区根。用于了解项目结构、找文件。",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"path": {"type": "string", "description": "目录路径或 glob 模式（相对工作区根），缺省为工作区根"}
				},
				"required": []
			}`),
		},
		func(_ context.Context, args json.RawMessage) (tool.Result, error) {
			return runFileList(cfg, args)
		},
	)
}

// globMeta 是否含 glob 通配符。
func globMeta(p string) bool {
	return strings.ContainsAny(p, "*?[")
}

// runFileList 目录列举 / glob 搜索执行逻辑。
func runFileList(cfg *FileConfig, args json.RawMessage) (tool.Result, error) {
	var in struct {
		Path string `json:"path"`
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
	var b strings.Builder
	if !globMeta(p) {
		// 目录列举：读目录条目，目录加后缀。
		entries, err := os.ReadDir(abs)
		if err != nil {
			return tool.Result{Data: "读取失败: " + err.Error(), IsError: true}, nil
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() {
				name += "/"
			}
			fmt.Fprintln(&b, name)
		}
	} else {
		// glob：递归遍历，相对路径匹配（支持 **）。
		pattern := filepath.ToSlash(strings.TrimPrefix(p, "./"))
		root := filepath.Clean(cfg.Root)
		_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil // 跳过不可读（如权限不足）
			}
			if d.IsDir() {
				if path != root && strings.HasPrefix(d.Name(), ".") {
					return filepath.SkipDir // 跳过隐藏目录（.git/.idea 等）
				}
				return nil
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return nil
			}
			if globMatch(pattern, filepath.ToSlash(rel)) {
				fmt.Fprintln(&b, filepath.ToSlash(rel))
			}
			return nil
		})
	}
	out := strings.TrimSuffix(b.String(), "\n")
	if out == "" {
		out = "（未找到匹配）"
	}
	return tool.Result{Data: truncateOutput(out)}, nil
}

// globMatch 支持 ** 跨目录的路径模式匹配（相对路径，按 "/" 分段）。
// ** 匹配零或多个路径段；* 与 ? 经 filepath.Match 匹配单段。
func globMatch(pattern, name string) bool {
	pp := strings.Split(pattern, "/")
	nn := strings.Split(name, "/")
	var match func(pi, ni int) bool
	match = func(pi, ni int) bool {
		if pi == len(pp) {
			return ni == len(nn)
		}
		if pp[pi] == "**" {
			for k := ni; k <= len(nn); k++ {
				if match(pi+1, k) {
					return true
				}
			}
			return false
		}
		if ni == len(nn) {
			return false
		}
		ok, _ := filepath.Match(pp[pi], nn[ni])
		return ok && match(pi+1, ni+1)
	}
	return match(0, 0)
}

// DocSearch 工作区关键词搜索（子串匹配，不区分大小写；跳过隐藏目录与大文件）。
func DocSearch(cfg *FileConfig) tool.Tool {
	return tool.NewFunc(
		tool.Spec{
			Name:        "doc_search",
			Description: "在工作区文本文件中按关键词搜索（子串匹配，不区分大小写；跳过隐藏目录与超 512KB 文件），返回 top-N 命中（相对路径 + 行号 + 片段）。用于找文档/代码中提到某概念的位置。",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"query": {"type": "string", "description": "搜索关键词"},
					"limit": {"type": "integer", "description": "最多返回条数，默认 5"}
				},
				"required": ["query"]
			}`),
		},
		func(_ context.Context, args json.RawMessage) (tool.Result, error) {
			return runDocSearch(cfg, args)
		},
	)
}

// docSearchMaxFileSize 参与搜索的文件大小上限（字节）：防读超大文件拖垮搜索。
const docSearchMaxFileSize = 512 << 10

// docSearchHit 一次命中（相对路径 + 行号 + 片段）。
type docSearchHit struct {
	path string
	line int
	text string
}

// runDocSearch 关键词搜索执行逻辑。
func runDocSearch(cfg *FileConfig, args json.RawMessage) (tool.Result, error) {
	var in struct {
		Query string `json:"query"`
		Limit *int   `json:"limit"` // 指针区分"未传"与"显式 0"
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return tool.Result{Data: "参数错误: " + err.Error(), IsError: true}, nil
	}
	query := strings.TrimSpace(in.Query)
	if query == "" {
		return tool.Result{Data: "参数错误: query 为空", IsError: true}, nil
	}
	limit := 5
	if in.Limit != nil && *in.Limit > 0 {
		limit = *in.Limit
	}
	q := strings.ToLower(query)
	root := filepath.Clean(cfg.Root)
	var hits []docSearchHit
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // 跳过不可读
		}
		if d.IsDir() {
			if path != root && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir // 隐藏目录
			}
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() > docSearchMaxFileSize {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		for i, ln := range strings.Split(string(data), "\n") {
			if strings.Contains(strings.ToLower(ln), q) {
				hits = append(hits, docSearchHit{path: filepath.ToSlash(rel), line: i + 1, text: strings.TrimSpace(ln)})
			}
		}
		return nil
	})
	if len(hits) == 0 {
		return tool.Result{Data: "未找到匹配 \"" + query + "\"", IsError: true}, nil
	}
	// 按相对路径排序保证顺序稳定，截断 top-N。
	sort.Slice(hits, func(i, j int) bool { return hits[i].path < hits[j].path })
	if len(hits) > limit {
		hits = hits[:limit]
	}
	var b strings.Builder
	for _, h := range hits {
		fmt.Fprintf(&b, "%s:%d: %s\n", h.path, h.line, truncateOutput(h.text))
	}
	return tool.Result{Data: truncateOutput(strings.TrimSuffix(b.String(), "\n"))}, nil
}
