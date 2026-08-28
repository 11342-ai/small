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
// Root 为 nil 时工具不注册（退化，同 mem/exec 惯例）。写工具确认已迁至 agent 权限
// 横切层（file_write/file_edit 在权限表中为 Ask，见 Zoo/model/policy.md）。
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

// FileRead 读取工作区内文本文件（带行号、窗口化、敏感文件防护）。
// 窗口化（tool-fs.md §4.1）：offset 起始行号（1-based，缺省 1）+ limit 行数（缺省读到文件尾）；
// 超界给提示、limit 截断给续读建议，引导模型低成本续读而非误判"读完了"。
func FileRead(cfg *FileConfig) tool.Tool {
	return tool.NewFunc(
		tool.Spec{
			Name:        "file_read",
			Description: "读取工作区内文本文件内容（带行号；支持 offset/limit 窗口化：offset 起始行号 1-based，limit 行数；超长截断；.env 等敏感文件拒绝读取）。path 相对工作区根（如 internal/agent/agent.go）或工作区内的绝对路径；仅允许访问工作区内文件。用于查看文档、代码、配置。",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"path": {"type": "string", "description": "文件路径（相对工作区根）"},
					"offset": {"type": "integer", "description": "起始行号（1-based，缺省 1）"},
					"limit": {"type": "integer", "description": "读取行数（缺省读到文件尾）"}
				},
				"required": ["path"]
			}`),
		},
		func(_ context.Context, args json.RawMessage) (tool.Result, error) {
			return runFileRead(cfg, args)
		},
	)
}

// maxFileReadSize file_read 单文件上限（字节）：超限跳过，提示窗口化/搜索定位。
// 与 doc_search 的 512KB 不同：file_read 是显式点名读取，上限放宽（tool-fs.md §4.1）。
const maxFileReadSize = 10 << 20

// runFileRead 文件读取执行逻辑（外置具名函数，可独立单测）。
func runFileRead(cfg *FileConfig, args json.RawMessage) (tool.Result, error) {
	var in struct {
		Path   string `json:"path"`
		Offset *int   `json:"offset"` // 指针区分"未传"与显式值（≤1 视为缺省 1）
		Limit  *int   `json:"limit"`  // 指针区分"未传"与显式值（≤0 视为未传）
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return tool.Result{Data: "参数错误: " + err.Error(), IsError: true}, nil
	}
	abs, err := resolveInRoot(cfg.Root, strings.TrimSpace(in.Path))
	if err != nil {
		return tool.Result{Data: err.Error(), IsError: true}, nil
	}
	// 敏感文件防护（fail-closed）：.env* 直接拒绝，机密不进模型上下文（tool-fs.md §4.1）。
	if strings.HasPrefix(filepath.Base(abs), ".env") {
		return tool.Result{Data: "[file_read: 敏感文件不读取（IGNORED）]", IsError: true}, nil
	}
	// 大小上限：Stat 先判，避免大文件整读占内存（显式点名读放宽到 10MB，仍须提示）。
	if fi, err := os.Stat(abs); err != nil {
		return tool.Result{Data: "读取失败: " + err.Error(), IsError: true}, nil
	} else if fi.Size() > maxFileReadSize {
		return tool.Result{Data: "[file_read: 文件过大（>10MB），建议 doc_search 定位后窗口化读]", IsError: true}, nil
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return tool.Result{Data: "读取失败: " + err.Error(), IsError: true}, nil
	}
	lines := strings.Split(string(data), "\n")
	total := len(lines)

	// 窗口：offset 1-based；limit ≤0 视为未传（读到文件尾）。
	start := 1
	if in.Offset != nil && *in.Offset > 1 {
		start = *in.Offset
	}
	if start > total {
		return tool.Result{Data: fmt.Sprintf("[file_read: 共 %d 行，offset=%d 超出]", total, start), IsError: true}, nil
	}
	end := total
	if in.Limit != nil && *in.Limit > 0 {
		end = start + *in.Limit - 1
		if end > total {
			end = total
		}
	}

	// 带行号渲染（人读友好），整体超限截断（tool-extend.md 边界④）。
	var b strings.Builder
	for i := start - 1; i < end; i++ {
		fmt.Fprintf(&b, "%5d│ %s\n", i+1, lines[i])
	}
	out := strings.TrimSuffix(b.String(), "\n")
	// 续读建议：limit 截断且未到文件尾时，提示下一窗口起点（tool-fs.md §4.1）。
	if end < total {
		out += fmt.Sprintf("\n（继续可 offset=%d，或用 doc_search 定位）", end+1)
	}
	return tool.Result{Data: truncateOutput(out)}, nil
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

// ignoredDirs 遍历时跳过的目录名（构建/依赖产物，tool-fs.md 常量忽略集）。
// 与"隐藏目录跳过"并存；只作用于 file_list/doc_search/file_tree 的**遍历**，
// file_read/file_write 是显式点名读取，不受此限制（用户明确要读就读）。
var ignoredDirs = map[string]bool{
	"node_modules": true,
	"dist":         true,
	"build":        true,
	"vendor":       true,
	"out":          true,
	"coverage":     true,
}

// ignoredFile 文件名是否应跳过（小写后缀匹配，覆盖压缩产物与源码映射，
// 防无关文件污染搜索/速览；与 ignoredDirs 同一语义）。
func ignoredFile(name string) bool {
	lower := strings.ToLower(name)
	for _, s := range []string{".min.js", ".min.css", ".map", ".d.ts"} {
		if strings.HasSuffix(lower, s) {
			return true
		}
	}
	return false
}

// skipIgnoredDir 遍历回调中判断目录是否应跳过（隐藏目录 + 常量忽略集）。
// root 是遍历起点：起点本身永远不跳（只跳其下的子目录）。
func skipIgnoredDir(root, path, name string) bool {
	return path != root && (strings.HasPrefix(name, ".") || ignoredDirs[name])
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
	root := filepath.Clean(cfg.Root)
	gi := loadGitIgnore(root)
	var b strings.Builder
	if !globMeta(p) {
		// 目录列举：读目录条目，目录加后缀（隐藏/忽略集/gitignore 一并过滤，
		// 保证列出的条目 = 模型可见的搜索空间）。
		entries, err := os.ReadDir(abs)
		if err != nil {
			return tool.Result{Data: "读取失败: " + err.Error(), IsError: true}, nil
		}
		for _, e := range entries {
			name := e.Name()
			isDir := e.IsDir()
			relRoot, _ := filepath.Rel(root, filepath.Join(abs, name))
			if gi.Ignored(filepath.ToSlash(relRoot), isDir) {
				continue
			}
			if isDir {
				if strings.HasPrefix(name, ".") || ignoredDirs[name] {
					continue // 隐藏/忽略目录不列出（与 glob 遍历同一视野）
				}
				name += "/"
			} else if ignoredFile(name) {
				continue // 忽略产物文件（*.min.js/*.map/*.d.ts 等）
			}
			fmt.Fprintln(&b, name)
		}
	} else {
		// glob：递归遍历，相对路径匹配（支持 **）。
		pattern := filepath.ToSlash(strings.TrimPrefix(p, "./"))
		_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil // 跳过不可读（如权限不足）
			}
			rel, rerr := filepath.Rel(root, path)
			if rerr != nil {
				return nil
			}
			relSlash := filepath.ToSlash(rel)
			if d.IsDir() {
				if skipIgnoredDir(root, path, d.Name()) || gi.Ignored(relSlash, true) {
					return filepath.SkipDir // 隐藏目录 + 忽略集目录 + gitignore 目录
				}
				return nil
			}
			if ignoredFile(d.Name()) || gi.Ignored(relSlash, false) {
				return nil // 忽略产物文件 / gitignore 文件
			}
			if globMatch(pattern, relSlash) {
				fmt.Fprintln(&b, relSlash)
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
// path 可限定搜索范围（文件或子目录，相对工作区根；显式点名不受忽略集/gitignore 限制，
// 对齐 file_read 语义）；缺省全工作区。
func DocSearch(cfg *FileConfig) tool.Tool {
	return tool.NewFunc(
		tool.Spec{
			Name:        "doc_search",
			Description: "在工作区文本文件中按关键词搜索（子串匹配，不区分大小写；跳过隐藏目录、忽略集目录/文件（node_modules/dist/*.min.js 等）与超 512KB 文件），返回 top-N 命中（相对路径 + 行号 + 片段）。path 可限定搜索范围（文件或子目录，如 internal/agent 或 README.md），缺省全工作区。用于找文档/代码中提到某概念的位置。",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"query": {"type": "string", "description": "搜索关键词"},
					"path": {"type": "string", "description": "限定搜索范围（文件或子目录，相对工作区根），缺省全工作区"},
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
		Path  string `json:"path"`
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
	gi := loadGitIgnore(root)

	// 搜索起点：path 指定则限定范围（文件或子目录），缺省全工作区（root）。
	// 显式点名（path == start）不受忽略集/gitignore 限制（对齐 file_read 语义，
	// 用户明确要搜它就搜）；其下内容照常过滤。
	start := root
	if p := strings.TrimSpace(in.Path); p != "" {
		abs, err := resolveInRoot(cfg.Root, p)
		if err != nil {
			return tool.Result{Data: err.Error(), IsError: true}, nil
		}
		if _, err := os.Stat(abs); err != nil {
			return tool.Result{Data: "路径不存在: " + err.Error(), IsError: true}, nil
		}
		start = abs
	}
	var hits []docSearchHit
	_ = filepath.WalkDir(start, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // 跳过不可读
		}
		rel, _ := filepath.Rel(root, path)
		relSlash := filepath.ToSlash(rel)
		atStart := path == start
		if d.IsDir() {
			if !atStart && (skipIgnoredDir(start, path, d.Name()) || gi.Ignored(relSlash, true)) {
				return filepath.SkipDir // 隐藏目录 + 忽略集目录 + gitignore 目录
			}
			return nil
		}
		if !atStart && (ignoredFile(d.Name()) || gi.Ignored(relSlash, false)) {
			return nil // 忽略产物文件 / gitignore 文件
		}
		info, err := d.Info()
		if err != nil || info.Size() > docSearchMaxFileSize {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
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
