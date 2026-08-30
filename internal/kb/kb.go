// Package kb 提供知识库层的索引能力：把 ~/.small/kb/ 下的 md 文件树解析为
// 节点/边/反链，支撑结构树查询、域聚合、无环校验与巡检。设计稿见 Zoo/model/kb.md。
//
// 边界约定：
//   - 叶子包：不 import 任何内部包（同 memory 的定位），避免循环依赖；
//   - 文件布局：<dir>/ 下任意 md 文件树——文件夹纯归置，不承载语义；
//   - 节点 = path#heading（段落），文件是容器；结构边单父无环（文件内标题层级
//     派生，frontmatter parent 仅作逃生舱）；引用边 = 正文相对链接，允许成环靠反链；
//   - 索引全派生不落盘：节点表/边表/反链/域每次查询前按 mtime 惰性重建
//     （语料小、毫秒级，无需增量索引）；
//   - 错误分层：写入越界（路径逃逸/文件已存在）与空参数属业务失败；目录不可读等
//     系统边界才返回 error。目录由 New 自动创建（对齐 memory，幂等）。
package kb

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// EdgeKind 边类型。
type EdgeKind string

const (
	// EdgeStructure 结构边：parent → child（单父无环）。
	EdgeStructure EdgeKind = "structure"
	// EdgeReference 引用边：source → target（允许成环）。
	EdgeReference EdgeKind = "reference"
)

// Node 知识节点：文件根节点（Heading 为空）或文件内一个段落（## 标题）。
type Node struct {
	Ref     string   // path#heading；文件根节点无锚点
	File    string   // 相对路径（斜杠分隔）
	Heading string   // 段落标题；文件根节点为空
	Level   int      // 认知深度 1-5（段落词表派生；frontmatter 手填覆盖文件根）
	Domains []string // frontmatter domains（文件级）
	Status  string   // frontmatter status
	Parent  string   // 结构父 Ref（标题层级派生；frontmatter parent 覆盖文件根）
}

// Edge 一条边：结构边或引用边。
type Edge struct {
	Source string   // 源节点 Ref
	Target string   // 目标节点 Ref
	Kind   EdgeKind // EdgeStructure | EdgeReference
	Broken bool     // 引用边目标节点不存在（断链）
}

// IssueType 巡检问题类型。
type IssueType string

const (
	IssueCycle  IssueType = "cycle"          // 结构边成环
	IssueBroken IssueType = "broken_link"    // 引用边指向不存在的节点
	IssueDup    IssueType = "dup_heading"    // 同文件重复标题
	IssueLevel  IssueType = "level_mismatch" // frontmatter 手填 level 与段落派生不一致
)

// Issue 巡检发现的一个问题。
type Issue struct {
	Type   IssueType
	Ref    string
	Detail string
}

// Store 知识库索引：把 md 文件树解析为节点/边/反链/域的只读快照。
// 模块对象、不接口化（唯一实现，同 memory.Store 先例）；
// 单进程 CLI，互斥锁保护重建，-race 干净。
type Store struct {
	dir       string
	mu        sync.Mutex
	nodes     map[string]Node      // Ref → Node
	edges     []Edge               // 全量边（结构 + 引用）
	children  map[string][]string  // 结构边：Parent → []Child Ref
	backlinks map[string][]string  // 引用边：Target → []Source Ref
	domains   []string             // 全部域（去重排序）
	issues    []Issue              // 巡检问题（构建时计算，惰性刷新保证新鲜）
	files     map[string]time.Time // 上次扫描的 mtime 快照
}

// New 构造知识库索引。目录不存在则自动创建（幂等，对齐 memory.New）——
// 知识库目录是 agent 的写入目标，缺目录应自动就绪而不是让调用方先建目录。
func New(dir string) (*Store, error) {
	if dir == "" {
		return nil, errors.New("kb: empty dir")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("kb: create dir %q: %w", dir, err)
	}
	return &Store{
		dir:       dir,
		nodes:     make(map[string]Node),
		children:  make(map[string][]string),
		backlinks: make(map[string][]string),
		files:     make(map[string]time.Time),
	}, nil
}

// Nodes 返回全部节点，按 Ref 字典序（结果确定可复现）。
func (s *Store) Nodes() []Node {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.refresh(); err != nil {
		return nil
	}
	nodes := make([]Node, 0, len(s.nodes))
	for _, n := range s.nodes {
		nodes = append(nodes, n)
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Ref < nodes[j].Ref })
	return nodes
}

// Node 按 Ref 查节点；不存在返回 ok=false。
func (s *Store) Node(ref string) (Node, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.refresh(); err != nil {
		return Node{}, false
	}
	n, ok := s.nodes[ref]
	return n, ok
}

// Edges 返回全部边（构建顺序确定）。
func (s *Store) Edges() []Edge {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.refresh(); err != nil {
		return nil
	}
	out := make([]Edge, len(s.edges))
	copy(out, s.edges)
	return out
}

// Children 返回结构子节点 Ref 列表（字典序）；无子节点返回空。
func (s *Store) Children(ref string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.refresh(); err != nil {
		return nil
	}
	return append([]string(nil), s.children[ref]...)
}

// Backlinks 返回引用边中指向 ref 的来源 Ref 列表（字典序）。
// 删除检查的前置：先看谁引用了它，再与用户协作处理（设计稿 §6）。
func (s *Store) Backlinks(ref string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.refresh(); err != nil {
		return nil
	}
	return append([]string(nil), s.backlinks[ref]...)
}

// Domains 返回全部域（去重字典序）。
func (s *Store) Domains() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.refresh(); err != nil {
		return nil
	}
	return append([]string(nil), s.domains...)
}

// DomainView 返回属于该域的文件根节点 Ref 列表（字典序）——大方块的自动聚合视图。
func (s *Store) DomainView(domain string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.refresh(); err != nil {
		return nil
	}
	var out []string
	for ref, n := range s.nodes {
		if n.Heading != "" {
			continue
		}
		for _, d := range n.Domains {
			if d == domain {
				out = append(out, ref)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

// Check 全库巡检：无环、断链、重复标题、level 一致性。索引为惰性快照，结果与构建同步。
func (s *Store) Check() []Issue {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.refresh(); err != nil {
		return nil
	}
	return append([]Issue(nil), s.issues...)
}

// Write 把一段知识点写入 <dir>/<rel>.md（不存在则创建），返回节点 Ref（斜杠相对路径）。
// 受控写入对齐 memory.Append 的约束：路径锁死知识库目录内（rel 须为相对路径，
// 不含绝对路径前缀与 ".." 逃逸，防路径注入）；文件已存在报错（防覆盖——更新走文件工具）。
// frontmatter 由本方法生成：domains（多值）+ level（1-5 时写入，缺省由段落词表派生）。
// 写入后下一次查询按 mtime 惰性刷新自动可见，无需手动重建索引。
func (s *Store) Write(rel, title, content string, domains []string, level int) (string, error) {
	rel = strings.TrimSpace(rel)
	if rel == "" {
		return "", errors.New("kb: empty file path")
	}
	if strings.HasPrefix(rel, "/") || strings.HasPrefix(rel, "\\") {
		return "", errors.New("kb: absolute path not allowed")
	}
	clean := filepath.Clean(filepath.FromSlash(rel))
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", errors.New("kb: path escapes knowledge base")
	}
	clean = strings.TrimSuffix(clean, ".md")
	if strings.TrimSpace(title) == "" {
		return "", errors.New("kb: empty title")
	}
	if strings.TrimSpace(content) == "" {
		return "", errors.New("kb: empty content")
	}
	path := filepath.Join(s.dir, clean+".md")
	if _, err := os.Stat(path); err == nil {
		return "", fmt.Errorf("kb: file already exists: %s", filepath.ToSlash(clean))
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("kb: stat %q: %w", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("kb: create dir: %w", err)
	}
	fm := map[string]any{}
	if len(domains) > 0 {
		fm["domains"] = domains
	}
	if level > 0 && level <= 5 {
		fm["level"] = level
	}
	var b strings.Builder
	if len(fm) > 0 {
		fmData, err := yaml.Marshal(fm)
		if err != nil {
			return "", fmt.Errorf("kb: marshal frontmatter: %w", err)
		}
		b.WriteString("---\n")
		b.Write(fmData)
		b.WriteString("---\n\n")
	}
	fmt.Fprintf(&b, "# %s\n\n%s\n", title, content)
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return "", fmt.Errorf("kb: write %q: %w", filepath.ToSlash(clean), err)
	}
	// 返回带 .md 的 Ref：与 build 索引的节点 Ref（文件相对路径）保持一致，
	// 调用方拿返回值可直接 Node()/链接。
	return filepath.ToSlash(clean + ".md"), nil
}

// refresh 惰性重建索引：扫描文件集并比对 mtime，无变化跳过；有变化全量重建。
func (s *Store) refresh() error {
	current, err := scanFiles(s.dir)
	if err != nil {
		return err
	}
	if len(current) == len(s.files) {
		same := true
		for path, m := range current {
			if s.files[path] != m {
				same = false
				break
			}
		}
		if same {
			return nil // 无变化：复用旧索引，零成本
		}
	}
	if err := s.build(current); err != nil {
		return err
	}
	s.files = current
	return nil
}

// scanFiles 递归收集 dir 下全部 .md 文件 → mtime 快照。
// 目录不存在返回空集合（空知识库不算错误）。
func scanFiles(dir string) (map[string]time.Time, error) {
	if _, err := os.Stat(dir); err != nil {
		if os.IsNotExist(err) {
			return map[string]time.Time{}, nil
		}
		return nil, fmt.Errorf("kb: stat %q: %w", dir, err)
	}
	files := make(map[string]time.Time)
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if strings.HasSuffix(d.Name(), ".md") {
			if info, err := d.Info(); err == nil {
				files[path] = info.ModTime()
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("kb: scan %q: %w", dir, err)
	}
	return files, nil
}

// build 全量重建索引：解析全部文件 → 节点 + 结构边，再统一建引用边（避免
// 文件顺序导致的断链误判），最后做无环校验与巡检汇总。
func (s *Store) build(files map[string]time.Time) error {
	var raws []rawFile
	for path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			continue // 单文件读取失败：跳过，不阻塞其余索引
		}
		rel, err := filepath.Rel(s.dir, path)
		if err != nil {
			continue
		}
		raws = append(raws, parseFile(filepath.ToSlash(rel), string(data)))
	}
	sort.Slice(raws, func(i, j int) bool { return raws[i].rel < raws[j].rel })

	nodes := make(map[string]Node, len(files)*2)
	edges := make([]Edge, 0, len(files)*2)
	children := make(map[string][]string)
	var issues []Issue
	domainSet := make(map[string]bool)

	// 阶段一：节点 + 结构边（文件内标题层级派生 parent）。
	for _, rf := range raws {
		rootParent := ""
		if rf.meta.parent != "" {
			rootParent = resolveRef(rf.rel, rf.meta.parent)
		}
		// 文件根 level：段落最高档派生；frontmatter 手填覆盖（不一致记 issue）。
		maxLevel := 1
		for _, sec := range rf.sections {
			if lv := deriveLevel(sec.title); lv > maxLevel {
				maxLevel = lv
			}
		}
		fileLevel := maxLevel
		if rf.meta.level > 0 {
			fileLevel = rf.meta.level
			if rf.meta.level != maxLevel {
				issues = append(issues, Issue{
					Type: IssueLevel, Ref: rf.rel,
					Detail: fmt.Sprintf("frontmatter level=%d 与段落派生 %d 不一致", rf.meta.level, maxLevel),
				})
			}
		}
		root := Node{
			Ref:     rf.rel,
			File:    rf.rel,
			Level:   fileLevel,
			Domains: rf.meta.domains,
			Status:  rf.meta.status,
			Parent:  rootParent,
		}
		nodes[rf.rel] = root
		for _, d := range rf.meta.domains {
			domainSet[d] = true
		}
		if rootParent != "" {
			edges = append(edges, Edge{Source: rootParent, Target: rf.rel, Kind: EdgeStructure})
			children[rootParent] = append(children[rootParent], rf.rel)
		}

		// 段落节点：弹栈至栈顶层级 < 当前层级，栈顶即父（兄弟同父、层级递增成父子）。
		stack := []struct {
			level int
			ref   string
		}{}
		seen := map[string]bool{rf.rel: true}
		for _, sec := range rf.sections {
			ref := rf.rel + "#" + sec.title
			if seen[ref] {
				issues = append(issues, Issue{Type: IssueDup, Ref: ref, Detail: "同文件重复标题"})
				continue // 重复标题：后出现者并入首个节点（ref 冲突），记 issue 提示整理
			}
			seen[ref] = true
			for len(stack) > 0 && stack[len(stack)-1].level >= sec.level {
				stack = stack[:len(stack)-1]
			}
			parent := rf.rel
			if len(stack) > 0 {
				parent = stack[len(stack)-1].ref
			}
			nodes[ref] = Node{
				Ref:     ref,
				File:    rf.rel,
				Heading: sec.title,
				Level:   deriveLevel(sec.title),
				Parent:  parent,
			}
			edges = append(edges, Edge{Source: parent, Target: ref, Kind: EdgeStructure})
			children[parent] = append(children[parent], ref)
			stack = append(stack, struct {
				level int
				ref   string
			}{sec.level, ref})
		}
	}

	// 阶段二：引用边（全部节点就绪后判定 broken 才准确）。
	backlinks := make(map[string][]string)
	for _, rf := range raws {
		for _, lk := range rf.links {
			target, ok := linkTarget(rf.rel, lk.dest)
			if !ok {
				continue
			}
			source := rf.rel
			if lk.section >= 0 && lk.section < len(rf.sections) {
				source = rf.rel + "#" + rf.sections[lk.section].title
			}
			_, exists := nodes[target]
			broken := !exists
			edges = append(edges, Edge{Source: source, Target: target, Kind: EdgeReference, Broken: broken})
			backlinks[target] = append(backlinks[target], source)
			if broken {
				issues = append(issues, Issue{Type: IssueBroken, Ref: source, Detail: "断链：" + target})
			}
		}
	}

	// 无环校验 + 排序保证结果确定。
	issues = append(issues, detectCycles(nodes)...)
	for k := range children {
		sort.Strings(children[k])
	}
	for k := range backlinks {
		sort.Strings(backlinks[k])
	}
	domains := make([]string, 0, len(domainSet))
	for d := range domainSet {
		domains = append(domains, d)
	}
	sort.Strings(domains)

	s.nodes, s.edges, s.children = nodes, edges, children
	s.backlinks, s.domains, s.issues = backlinks, domains, issues
	return nil
}

// detectCycles 沿父链检测结构环：每节点只有一条父链，沿链上溯若回到已访问节点即成环。
// 结构边单父，无需完整图 DFS，父链法即可覆盖全部环（含 frontmatter parent 引入的）。
func detectCycles(nodes map[string]Node) []Issue {
	var issues []Issue
	for ref := range nodes {
		seen := make(map[string]bool)
		for cur := ref; cur != ""; cur = nodes[cur].Parent {
			if seen[cur] {
				issues = append(issues, Issue{
					Type: IssueCycle, Ref: ref,
					Detail: "结构边成环，碰撞节点：" + cur,
				})
				break
			}
			seen[cur] = true
		}
	}
	return issues
}

// resolveRef 把相对当前文件的"路径#锚点"规范化为绝对 Ref（斜杠、Clean、锚点解码）。
// fromRel 为斜杠相对路径（如 "Java/事务.md"）；target 如 "数据/一致性.md#原理"。
func resolveRef(fromRel, target string) string {
	path, anchor, _ := strings.Cut(target, "#")
	if anchor != "" {
		anchor = unescapeAnchor(anchor)
	}
	if path == "" {
		return fromRel + "#" + anchor
	}
	base := filepath.Dir(filepath.FromSlash(fromRel))
	abs := filepath.Clean(filepath.Join(base, filepath.FromSlash(path)))
	rel := filepath.ToSlash(abs)
	if anchor != "" {
		return rel + "#" + anchor
	}
	return rel
}

// linkTarget 把链接目标解析为节点 Ref；外部链接/非 md 资源返回 ok=false（不算知识引用）。
func linkTarget(fromRel, dest string) (string, bool) {
	if strings.Contains(dest, "://") || strings.HasPrefix(dest, "mailto:") {
		return "", false
	}
	path, _, _ := strings.Cut(dest, "#")
	if path != "" && !strings.HasSuffix(strings.ToLower(path), ".md") {
		return "", false
	}
	return resolveRef(fromRel, dest), true
}

// unescapeAnchor 解码锚点（md 中标题锚点常被百分号编码，宽松匹配真实标题文本）。
func unescapeAnchor(anchor string) string {
	if s, err := url.PathUnescape(anchor); err == nil {
		return s
	}
	return anchor
}

// levelKeywords 认知深度段落词表：标题文本命中哪档关键词，节点就落在哪档。
// 设计稿 §5：L1 定义 → L2 示例/操作 → L3 原理 → L4 失效/边界/权衡 → L5 演进/对比/批判。
var levelKeywords = []struct {
	level int
	words []string
}{
	{1, []string{"定义", "是什么", "概念", "概述", "介绍"}},
	{2, []string{"示例", "例子", "操作", "用法", "实战"}},
	{3, []string{"原理", "机制", "为什么", "实现", "源码", "流程"}},
	{4, []string{"失效", "边界", "权衡", "局限", "限制", "缺陷", "坑"}},
	{5, []string{"演进", "演变", "发展", "趋势", "批判", "反思", "对比", "替代"}},
}

// deriveLevel 标题文本 → 认知深度档位（未命中 = L1，多档命中取最高）。
func deriveLevel(title string) int {
	best := 1
	for _, kw := range levelKeywords {
		for _, w := range kw.words {
			if strings.Contains(title, w) && kw.level > best {
				best = kw.level
			}
		}
	}
	return best
}
