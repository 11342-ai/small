package kb

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeFile 建目录并写入文件（测试辅助）。
func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
	// 保证 mtime 变化可被惰性刷新感知（部分文件系统 mtime 精度低）。
	time.Sleep(2 * time.Millisecond)
}

// openStore 新建 Store 并刷新一次（测试辅助）。
func openStore(t *testing.T, root string) *Store {
	t.Helper()
	s, err := New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.mu.Lock()
	if err := s.refresh(); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	s.mu.Unlock()
	return s
}

func TestBuildNodesAndEdges(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "Java/事务.md", `---
domains: [java, 数据一致性]
level: 3
---
# 事务
## 原理
Spring 事务基于 AOP 实现。
## 边界
自调用会失效，参考[一致性](../数据/一致性.md#定义)。
`)
	writeFile(t, root, "数据/一致性.md", `---
domains: [mysql, kafka, redis, 数据一致性]
---
# 数据一致性
## 定义
缓存与数据库的一致问题。
## 示例
延迟双删方案。
## 失效
无过期时间时延迟双删失效，参考[事务](../Java/事务.md#原理)。
`)
	s := openStore(t, root)

	// 节点：文件根 + 段落，parent 由标题层级派生。
	rootNode, ok := s.Node("数据/一致性.md")
	if !ok {
		t.Fatalf("root node 缺失")
	}
	if rootNode.Parent != "" || rootNode.Level != 4 {
		t.Fatalf("root node 期望 Parent=\"\" Level=4, 得 %+v", rootNode)
	}
	h1, ok := s.Node("数据/一致性.md#数据一致性")
	if !ok || h1.Parent != "数据/一致性.md" {
		t.Fatalf("H1 父应为文件根，得 %+v ok=%v", h1, ok)
	}
	def, ok := s.Node("数据/一致性.md#定义")
	if !ok || def.Parent != "数据/一致性.md#数据一致性" {
		t.Fatalf("H2 父应为 H1，得 %+v ok=%v", def, ok)
	}

	// 引用边：双向互相引用，均 active。
	edges := s.Edges()
	find := func(want Edge) bool {
		for _, e := range edges {
			if e == want {
				return true
			}
		}
		return false
	}
	if !find(Edge{Source: "数据/一致性.md#失效", Target: "Java/事务.md#原理", Kind: EdgeReference}) {
		t.Fatalf("缺引用边 一致性→事务：%+v", edges)
	}
	if !find(Edge{Source: "Java/事务.md#边界", Target: "数据/一致性.md#定义", Kind: EdgeReference}) {
		t.Fatalf("缺引用边 事务→一致性：%+v", edges)
	}
	if !find(Edge{Source: "数据/一致性.md", Target: "数据/一致性.md#数据一致性", Kind: EdgeStructure}) {
		t.Fatalf("缺结构边 根→H1：%+v", edges)
	}

	// 反链：删除检查的第一性依赖。
	got := s.Backlinks("Java/事务.md#原理")
	if len(got) != 1 || got[0] != "数据/一致性.md#失效" {
		t.Fatalf("Backlinks 期望 [数据/一致性.md#失效]，得 %v", got)
	}

	// 结构子树。
	if c := s.Children("数据/一致性.md"); len(c) != 1 || c[0] != "数据/一致性.md#数据一致性" {
		t.Fatalf("Children 期望 [数据/一致性.md#数据一致性]，得 %v", c)
	}

	// 域聚合（大方块自动视图）。
	domains := s.Domains()
	wantDomains := []string{"java", "kafka", "mysql", "redis", "数据一致性"}
	if len(domains) != len(wantDomains) {
		t.Fatalf("Domains 期望 %v，得 %v", wantDomains, domains)
	}
	for i := range wantDomains {
		if domains[i] != wantDomains[i] {
			t.Fatalf("Domains 期望 %v，得 %v", wantDomains, domains)
		}
	}
	view := s.DomainView("数据一致性")
	if len(view) != 2 || view[0] != "Java/事务.md" || view[1] != "数据/一致性.md" {
		t.Fatalf("DomainView 期望 [Java/事务.md 数据/一致性.md]，得 %v", view)
	}
}

func TestLevelMismatch(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "x.md", `---
level: 3
---
# X
## 原理
a
## 失效
b
`)
	s := openStore(t, root)
	// 段落最高档 L4（失效），frontmatter 手填 3 → 覆盖生效 + 记 mismatch。
	n, _ := s.Node("x.md")
	if n.Level != 3 {
		t.Fatalf("frontmatter 覆盖后 Level 期望 3，得 %d", n.Level)
	}
	found := false
	for _, is := range s.Check() {
		if is.Type == IssueLevel && is.Ref == "x.md" {
			found = true
		}
	}
	if !found {
		t.Fatalf("期望 level_mismatch issue，得 %+v", s.Check())
	}
}

func TestCycleDetection(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.md", `---
parent: b.md
---
# A
`)
	writeFile(t, root, "b.md", `---
parent: a.md
---
# B
`)
	s := openStore(t, root)
	found := false
	for _, is := range s.Check() {
		if is.Type == IssueCycle {
			found = true
		}
	}
	if !found {
		t.Fatalf("frontmatter parent 互指应报环，得 %+v", s.Check())
	}
}

func TestBrokenLink(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "x.md", "见[丢失](../不存在.md#标题)\n")
	s := openStore(t, root)
	found := false
	for _, e := range s.Edges() {
		if e.Kind == EdgeReference && e.Broken && e.Target == "../不存在.md#标题" {
			found = true
		}
	}
	if !found {
		t.Fatalf("期望 broken 引用边，得 %+v", s.Edges())
	}
	has := false
	for _, is := range s.Check() {
		if is.Type == IssueBroken {
			has = true
		}
	}
	if !has {
		t.Fatalf("期望 broken_link issue，得 %+v", s.Check())
	}
}

func TestDuplicateHeading(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "d.md", `# D
## 重复
x
## 重复
y
`)
	s := openStore(t, root)
	found := false
	for _, is := range s.Check() {
		if is.Type == IssueDup {
			found = true
		}
	}
	if !found {
		t.Fatalf("重复标题应报 dup_heading，得 %+v", s.Check())
	}
}

func TestLazyRefresh(t *testing.T) {
	root := t.TempDir()
	s := openStore(t, root)
	if len(s.Nodes()) != 0 {
		t.Fatalf("空目录应无节点，得 %d", len(s.Nodes()))
	}
	writeFile(t, root, "n.md", "# N\n")
	if len(s.Nodes()) != 2 {
		t.Fatalf("新增文件后应有 2 节点（文件根+H1），得 %d", len(s.Nodes()))
	}
	writeFile(t, root, "n.md", "# N\n## 段落\n内容\n")
	if len(s.Nodes()) != 3 {
		t.Fatalf("追加段落后应有 3 节点，得 %d", len(s.Nodes()))
	}
}

func TestWrite(t *testing.T) {
	root := t.TempDir()
	s := openStore(t, root)
	// 新知识点：子目录 + domains + level 覆盖。
	ref, err := s.Write("Java/延迟双删", "延迟双删", "## 定义\nxxx\n## 失效\nxxx\n", []string{"redis", "mysql", "数据一致性"}, 0)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if ref != "Java/延迟双删.md" {
		t.Fatalf("ref 期望 Java/延迟双删.md，得 %s", ref)
	}
	// 落盘内容：frontmatter（domains）+ # 标题。
	data, err := os.ReadFile(filepath.Join(root, "Java", "延迟双删.md"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	content := string(data)
	for _, want := range []string{"---", "domains:", "数据一致性", "# 延迟双删", "## 失效"} {
		if !strings.Contains(content, want) {
			t.Errorf("文件内容缺 %q，实际:\n%s", want, content)
		}
	}
	// 写入后索引立即可查（惰性刷新）：文件根 + 段落。
	n, ok := s.Node("Java/延迟双删.md")
	if !ok {
		t.Fatalf("写入后节点不可查")
	}
	if n.Level != 4 { // ## 失效 → L4（未手填 level，由段落派生）
		t.Fatalf("level 派生期望 4，得 %d", n.Level)
	}
	// 手填 level 覆盖派生值。
	if _, err := s.Write("Java/覆盖", "覆盖", "## 定义\nx\n", []string{"java"}, 3); err != nil {
		t.Fatalf("Write override: %v", err)
	}
	n2, ok := s.Node("Java/覆盖.md")
	if !ok || n2.Level != 3 {
		t.Fatalf("level 覆盖期望 3，得 %+v ok=%v", n2, ok)
	}
}

func TestWriteReject(t *testing.T) {
	root := t.TempDir()
	s := openStore(t, root)
	// 已存在 → 防覆盖。
	if _, err := s.Write("a", "A", "body", nil, 0); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if _, err := s.Write("a", "A2", "body2", nil, 0); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("已存在应报错，得 %v", err)
	}
	// 路径逃逸 → 防注入。
	if _, err := s.Write("../逃逸", "X", "body", nil, 0); err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("路径逃逸应报错，得 %v", err)
	}
	// 空参数。
	if _, err := s.Write("", "X", "body", nil, 0); err == nil {
		t.Fatal("空路径应报错")
	}
	if _, err := s.Write("b", "", "body", nil, 0); err == nil {
		t.Fatal("空标题应报错")
	}
}

func TestParseTolerance(t *testing.T) {
	root := t.TempDir()
	// 非 frontmatter 开头 + 外部链接 + 非 md 资源：都应正常索引、不误判断链。
	writeFile(t, root, "plain.md", `# 主题

外部[链接](https://example.com)和[图片](../img/a.png)不算知识引用。
`)
	s := openStore(t, root)
	if len(s.Nodes()) != 2 {
		t.Fatalf("期望 文件根+H1 两节点，得 %d", len(s.Nodes()))
	}
	for _, e := range s.Edges() {
		if e.Kind == EdgeReference && e.Broken {
			t.Fatalf("外部链接/图片不应记断链：%+v", e)
		}
	}
}
