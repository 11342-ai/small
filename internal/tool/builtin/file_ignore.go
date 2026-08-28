package builtin

import (
	"os"
	"path/filepath"
	"strings"
)

// .gitignore 解析（够用版，tool-fs.md §2 B 档）：file_list/doc_search/file_tree
// 的遍历遵循工作区根 .gitignore，尊重用户既有忽略意图，与常量忽略集互补。
//
// 支持：空行与 # 注释、! 取反、目录后缀 /、无斜杠模式=任意层级 basename 匹配、
// 含斜杠模式=相对工作区根匹配（含祖先目录前缀匹配）、**/ 前缀归一为任意层级。
// 不支持（够用版边界）：嵌套 .gitignore、转义（\）、`**` 跨层匹配（归一为单层）。
type gitIgnorePattern struct {
	negate  bool // ! 取反：命中此规则不忽略
	dirOnly bool // 尾部 /：仅匹配目录
	glob    string
}

type gitIgnore struct {
	patterns []gitIgnorePattern
}

// loadGitIgnore 读取 root/.gitignore 并解析；文件缺失/不可读返回空规则集（不阻塞遍历）。
func loadGitIgnore(root string) *gitIgnore {
	data, err := os.ReadFile(filepath.Join(root, ".gitignore"))
	if err != nil {
		return &gitIgnore{}
	}
	var gi gitIgnore
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimRight(line, "\r") // CRLF 文件行尾归一
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") {
			continue // 空行/注释
		}
		negate := strings.HasPrefix(t, "!")
		if negate {
			t = strings.TrimSpace(t[1:])
		}
		dirOnly := strings.HasSuffix(t, "/")
		if dirOnly {
			t = strings.TrimSuffix(t, "/")
		}
		t = strings.TrimPrefix(t, "/") // 前导 / = 锚定 root；本项目匹配本就相对 root，语义等价，可安全去掉
		if t == "" || strings.Contains(t, "\\") {
			continue // 空模式 / 转义（够用版不支持）
		}
		// **/ 前缀归一为任意层级（如 **/foo 与 foo 同义）；剩余 ** 归一为 *（单层）。
		for strings.HasPrefix(t, "**/") {
			t = t[len("**/"):]
		}
		t = strings.ReplaceAll(t, "**", "*")
		gi.patterns = append(gi.patterns, gitIgnorePattern{negate: negate, dirOnly: dirOnly, glob: t})
	}
	return &gi
}

// Ignored 判断相对工作区根的路径（/ 分隔）是否应忽略。gitignore 语义：
// 最后匹配的规则生效——故从前往后遍历，靠后的规则覆盖靠前的（含 ! 取反覆盖）。
func (g *gitIgnore) Ignored(rel string, isDir bool) bool {
	ignored := false
	for i := 0; i < len(g.patterns); i++ {
		p := g.patterns[i]
		// 目录规则（尾部 /）：作用于目录本身及其下内容（dist/x.js 也命中 dist/）。
		if p.dirOnly && !isDir && !strings.HasPrefix(rel, p.glob+"/") {
			continue
		}
		if giMatch(p.glob, rel) {
			ignored = !p.negate
		}
	}
	return ignored
}

// giMatch 模式与相对路径匹配：无斜杠 → 任意层级 basename；
// 含斜杠 → 整段匹配或祖先目录前缀匹配（如 build 匹配 build/x）。
func giMatch(glob, rel string) bool {
	if !strings.Contains(glob, "/") {
		for _, seg := range strings.Split(rel, "/") {
			if ok, _ := filepath.Match(glob, seg); ok {
				return true
			}
		}
		return false
	}
	if ok, _ := filepath.Match(glob, rel); ok {
		return true
	}
	return strings.HasPrefix(rel, glob+"/")
}
