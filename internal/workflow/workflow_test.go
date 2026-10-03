package workflow

import (
	"strings"
	"testing"
	"testing/fstest"
)

// TestLoad 真实 embed 加载：pdf 分支解析出 frontmatter 元数据与步骤正文。
func TestLoad(t *testing.T) {
	m, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	w, err := m.Get("pdf")
	if err != nil {
		t.Fatalf("Get(pdf): %v", err)
	}
	if w.Description == "" || w.Trigger == "" || w.Input == "" || w.Output == "" || w.Stop == "" {
		t.Errorf("pdf 分支元数据不全: %+v", w)
	}
	// 步骤正文必须含关键动作（步骤规划完整）。
	for _, kw := range []string{"doc_parse", "doc_clean", "doc_read", "整理", "落盘", "汇报"} {
		if !strings.Contains(w.Steps, kw) {
			t.Errorf("pdf 步骤缺 %q:\n%s", kw, w.Steps)
		}
	}
}

// TestLoad_K8sDiag k8s-diag 分支：元数据齐全，步骤含"收集→分析→验证→提交"的关键动作。
func TestLoad_K8sDiag(t *testing.T) {
	m, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	w, err := m.Get("k8s-diag")
	if err != nil {
		t.Fatalf("Get(k8s-diag): %v", err)
	}
	if w.Description == "" || w.Trigger == "" || w.Input == "" || w.Output == "" || w.Stop == "" {
		t.Errorf("k8s-diag 分支元数据不全: %+v", w)
	}
	// requires 是能力裁剪的挂点（k8s 客户端未就绪时该分支不注入），资产里漏写会让裁剪失效。
	if w.Requires != CapK8s {
		t.Errorf("k8s-diag 的 requires = %q，期望 %q", w.Requires, CapK8s)
	}
	for _, kw := range []string{"k8s_pod", "k8s_evidence", "k8s_report", "previous", "missing_evidence", "分支完成:k8s-diag"} {
		if !strings.Contains(w.Steps, kw) {
			t.Errorf("k8s-diag 步骤缺 %q:\n%s", kw, w.Steps)
		}
	}
}

// TestManager_GetList 未命中报错附可用列表；List 稳定有序。
func TestManager_GetList(t *testing.T) {
	m, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, err := m.Get("nope"); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Errorf("未命中应报错并附名: %v", err)
	}
	list := m.List()
	for i := 1; i < len(list); i++ {
		if list[i-1].Name > list[i].Name {
			t.Errorf("List 顺序不稳定: %v", list)
		}
	}
}

// TestRenderBranch 渲染格式：引言 + 每分支 名称/触发/输入/输出/停止/步骤。
func TestRenderBranch(t *testing.T) {
	m, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	s := m.RenderBranch()
	if s == "" {
		t.Fatal("有分支时 RenderBranch 不应为空")
	}
	for _, kw := range []string{"可用工作流分支", "[pdf]", "[k8s-diag]", "触发：", "输入：", "输出：", "停止：", "步骤：", "[进入分支:分支名]", "[分支完成:分支名]"} {
		if !strings.Contains(s, kw) {
			t.Errorf("RenderBranch 缺 %q:\n%s", kw, s)
		}
	}
}

// fs 构造单个虚拟 workflow 文件系统。
func fsOf(name, content string) fstest.MapFS {
	return fstest.MapFS{"workflows/" + name: {Data: []byte(content)}}
}

// TestRenderBranchFor 分支清单的能力裁剪：requires 不满足的分支整段不注入（连步骤也不出现），
// 满足时与不裁剪完全一致；全被裁掉时返回空串（不留一个空的分支段）。
func TestRenderBranchFor(t *testing.T) {
	m, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// k8s 能力缺失：k8s-diag 整段消失，无 requires 的 pdf 不受影响。
	s := m.RenderBranchFor(map[string]bool{})
	if strings.Contains(s, "[k8s-diag]") || strings.Contains(s, "分支完成:k8s-diag") {
		t.Errorf("k8s 能力缺失时不该注入 k8s-diag 分支（含步骤）:\n%s", s)
	}
	if !strings.Contains(s, "[pdf]") {
		t.Errorf("无 requires 的分支不该被裁掉:\n%s", s)
	}
	// 能力满足 / nil：与"不裁剪"逐字一致（零回归）。
	if got, want := m.RenderBranchFor(map[string]bool{CapK8s: true}), m.RenderBranch(); got != want {
		t.Error("能力齐全时应与 RenderBranch 逐字一致")
	}
	if m.RenderBranchFor(nil) != m.RenderBranch() {
		t.Error("available 为 nil 应等价于不裁剪")
	}

	// 只有 requires 分支且能力缺失：返回空串（组合根不追加空段）。
	m2, err := load(fsOf("only.md", "---\nname: only\nrequires: nope\n---\n正文"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := m2.RenderBranchFor(map[string]bool{}); got != "" {
		t.Errorf("全部分支被裁掉时应返回空串，实际 %q", got)
	}
}

func TestLoad_ErrorCases(t *testing.T) {
	cases := []struct {
		name, content string
	}{
		{"broken.md", "---\nname: a\n（缺 closing）"},
		{"bad-yaml.md", "---\nname: [unclosed\n---\n正文"},
		{"empty-body.md", "---\nname: a\n---\n"},
	}
	for _, c := range cases {
		if _, err := load(fsOf(c.name, c.content)); err == nil {
			t.Errorf("%s 应解析失败", c.name)
		}
	}
	// 重名：两个文件同 name → 报错。
	dup := fstest.MapFS{
		"workflows/a.md": {Data: []byte("---\nname: x\n---\n正文1")},
		"workflows/b.md": {Data: []byte("---\nname: x\n---\n正文2")},
	}
	if _, err := load(dup); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("重名应报错: %v", err)
	}
	// 缺 name：取文件名 stem。
	m, err := load(fsOf("myname.md", "---\ndescription: d\n---\n正文"))
	if err != nil {
		t.Fatalf("缺 name 应取文件名: %v", err)
	}
	if _, err := m.Get("myname"); err != nil {
		t.Errorf("Get(myname) 应命中: %v", err)
	}
}
