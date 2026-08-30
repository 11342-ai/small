package kb

import (
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
	"gopkg.in/yaml.v3"
)

// fmMeta 文件级元数据（frontmatter 提取；缺省零值 = 未填）。
type fmMeta struct {
	domains []string
	status  string
	level   int    // 手填覆盖值；0 表示未填
	parent  string // 相对当前文件的"路径#锚点"（结构父逃生舱）
}

// rawSection 一个标题段落的原始信息。
type rawSection struct {
	level int
	title string
}

// rawLink 一个引用链接的原始信息。
type rawLink struct {
	dest    string // 链接目标原文，如 "foo.md#标题"
	section int    // 所在段落下标；-1 = 文件头（归属文件根节点）
}

// rawFile 一个文件解析后的原始结构（纯内容，与物理位置解耦）。
type rawFile struct {
	rel      string
	meta     fmMeta
	sections []rawSection
	links    []rawLink
}

// markdownParser goldmark 解析器（共享实例，并发安全）。
var markdownParser = goldmark.New()

// parseFile 解析单个 md 文件：frontmatter + 标题序列 + 链接序列。
func parseFile(rel, content string) rawFile {
	meta, body := parseFrontmatter(content)
	rf := rawFile{rel: rel, meta: meta}
	src := []byte(body)
	doc := markdownParser.Parser().Parse(text.NewReader(src))
	curSection := -1 // 文件头：链接归属文件根节点
	ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		switch node := n.(type) {
		case *ast.Heading:
			if !entering {
				return ast.WalkContinue, nil
			}
			rf.sections = append(rf.sections, rawSection{
				level: node.Level,
				title: string(node.Text(src)),
			})
			curSection = len(rf.sections) - 1
		case *ast.Link:
			if entering {
				rf.links = append(rf.links, rawLink{dest: string(node.Destination), section: curSection})
			}
		}
		return ast.WalkContinue, nil
	})
	return rf
}

// parseFrontmatter 解析文件头 YAML frontmatter（--- 起止块），返回元数据与正文。
// 非 frontmatter 开头或解析失败时返回空元数据、原文本（容错，不阻塞索引）。
func parseFrontmatter(content string) (fmMeta, string) {
	lines := strings.Split(content, "\n")
	if len(lines) < 3 || strings.TrimSpace(lines[0]) != "---" {
		return fmMeta{}, content
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) != "---" {
			continue
		}
		var m map[string]any
		if err := yaml.Unmarshal([]byte(strings.Join(lines[1:i], "\n")), &m); err != nil {
			return fmMeta{}, content
		}
		meta := fmMeta{}
		meta.domains = toStrings(m["domains"])
		meta.status, _ = m["status"].(string)
		if lv, ok := m["level"].(int); ok && lv > 0 && lv <= 5 {
			meta.level = lv
		}
		meta.parent, _ = m["parent"].(string)
		return meta, strings.Join(lines[i+1:], "\n")
	}
	return fmMeta{}, content
}

// toStrings 把 yaml 解出的 domains（[]any 或单字符串）统一为 []string。
func toStrings(v any) []string {
	switch t := v.(type) {
	case []any:
		var out []string
		for _, x := range t {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case string:
		return []string{t}
	}
	return nil
}
