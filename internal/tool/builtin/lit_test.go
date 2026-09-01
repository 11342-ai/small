package builtin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"small/internal/tool"
)

// ---- clean 规则单测（tool-lit.md §8：真实样例驱动） ----

// TestCleanDocLines_RealCase 用真实解析产物片段验证各规则命中（水印/页码/分页符/空行）。
func TestCleanDocLines_RealCase(t *testing.T) {
	in := []string{
		"      人民法院案例库        人民法院案例库", // 行内重复水印（多空格）
		"    基本案情",
		"    被害人小花（案发时9周岁），系雷某与王某婚生女。",
		"        第 1 页", // 页码残留
		"----",          // 分页符
		"     人民法院案例库    人民法院案例库",
		"正文第一段",
		"",
		"",
		"正文第二段",
	}
	out, counts := cleanDocLines(in)
	joined := strings.Join(out, "\n")
	if counts.pageBreaks != 1 {
		t.Errorf("pageBreaks = %d, want 1", counts.pageBreaks)
	}
	if counts.pageNums != 1 {
		t.Errorf("pageNums = %d, want 1", counts.pageNums)
	}
	if counts.inlineDup != 2 {
		t.Errorf("inlineDup = %d, want 2", counts.inlineDup)
	}
	if counts.blankFold != 1 {
		t.Errorf("blankFold = %d, want 1", counts.blankFold)
	}
	for _, bad := range []string{"第 1 页", "----", "人民法院案例库 人民法院案例库"} {
		if strings.Contains(joined, bad) {
			t.Errorf("清洗后仍含 %q", bad)
		}
	}
	// 正常内容必须保留。
	for _, keep := range []string{"基本案情", "被害人小花", "正文第一段", "正文第二段"} {
		if !strings.Contains(joined, keep) {
			t.Errorf("清洗误删正常内容 %q", keep)
		}
	}
}

func TestCleanDocLines_EmptyAndTrim(t *testing.T) {
	// 空行收尾：末尾空行应被收掉；纯空文件输出空。
	if out, _ := cleanDocLines([]string{"a", ""}); len(out) != 1 || out[0] != "a" {
		t.Errorf("尾空行未收掉: %v", out)
	}
	if out, _ := cleanDocLines([]string{"", ""}); len(out) != 0 {
		t.Errorf("纯空输入应有 0 行: %v", out)
	}
}

// TestCleanDocLines_V2Rules 规则 6/7（pdf-workflow.md §3）：改进点1 真实样例
// 逐字拆分压缩 + 横线页码残留删行；数字/英文间空格不动（防误伤 4.4 亿 类）。
func TestCleanDocLines_V2Rules(t *testing.T) {
	in := []string{
		"第 一 百 四 十 一 条 生 产 、 销",
		"售 假 药 的 ， 处 三 年 以 下 有 期 徒 刑",
		" －70－ ",
		"- 12 -",
		"正常 4.4 亿 数字 与 abc xyz",
	}
	out, counts := cleanDocLines(in)
	if counts.dashedPages != 2 {
		t.Errorf("dashedPages = %d, want 2", counts.dashedPages)
	}
	if counts.cjkSpace == 0 {
		t.Error("cjkSpace 应为正（逐字拆分样例）")
	}
	if len(out) != 3 {
		t.Fatalf("应剩 3 行（2 行中文 + 1 行数字），got %d: %v", len(out), out)
	}
	if out[0] != "第一百四十一条生产、销" || out[1] != "售假药的，处三年以下有期徒刑" {
		t.Errorf("逐字拆分未正确压缩: %q %q", out[0], out[1])
	}
	// 数字/英文间空格保留（"4.4 亿"不被拆）；CJK 间空格压缩（"数字 与"→"数字与"）。
	if out[2] != "正常 4.4 亿数字与 abc xyz" {
		t.Errorf("数字/英文空格处理异常: %q", out[2])
	}
}

func TestDocFoldCJKSpace(t *testing.T) {
	cases := []struct{ in, want string }{
		{"第 一 百 四 十 一 条", "第一百四十一条"},
		{"处 三 年 以 下 有 期 徒 刑", "处三年以下有期徒刑"},
		{"生产 、 销 售", "生产、销售"},
		{"hello world", "hello world"}, // 英文不动
		{"4.4 亿", "4.4 亿"},             // 数字+单位间空格不动（AI 阶段处理）
		{"", ""},
	}
	for _, c := range cases {
		got, removed := docFoldCJKSpace(c.in)
		if got != c.want {
			t.Errorf("docFoldCJKSpace(%q) = %q, want %q", c.in, got, c.want)
		}
		if c.in != c.want && removed == 0 {
			t.Errorf("docFoldCJKSpace(%q) 应有删除计数", c.in)
		}
	}
}

func TestDocDashedPageRe(t *testing.T) {
	for _, in := range []string{"－70－", "- 70 -", "——12——", " - 3 - "} {
		if !docDashedPageRe.MatchString(strings.TrimSpace(in)) {
			t.Errorf("docDashedPageRe 应匹配横线页码 %q", in)
		}
	}
	for _, in := range []string{"第 70 页", "正文", "70", "－x－"} {
		if docDashedPageRe.MatchString(strings.TrimSpace(in)) {
			t.Errorf("docDashedPageRe 误匹配 %q", in)
		}
	}
}

func TestDocFoldInlineRepeat(t *testing.T) {
	cases := []struct {
		in   string
		want string
		fold bool
	}{
		{"人民法院案例库 人民法院案例库", "人民法院案例库", true},
		{"人民法院案例库    人民法院案例库", "人民法院案例库", true}, // 多空格视为同 token
		{"人民法院案例库 人民法院案例库 人民法院案例库", "人民法院案例库", true},
		{"X 人民法院案例库 人民法院案例库", "X 人民法院案例库", true},
		{"a b a b", "a b", true}, // 最长段优先（k=2），避免误叠成 a b a
		{"正常文本内容", "正常文本内容", false},
		{"单个词", "单个词", false},
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := docFoldInlineRepeat(c.in)
		if ok != c.fold || got != c.want {
			t.Errorf("docFoldInlineRepeat(%q) = %q,%v want %q,%v", c.in, got, ok, c.want, c.fold)
		}
	}
}

func TestDocPageBreaksAndNums(t *testing.T) {
	// clean 流程先 TrimSpace 再匹配，故断言同样以 trim 后为准。
	for _, in := range []string{"----", "------", "  ----  ", "-", ""} {
		got := docPageBreakRe.MatchString(strings.TrimSpace(in))
		want := strings.TrimSpace(in) != "" && strings.Count(strings.TrimSpace(in), "-") >= 3
		if got != want {
			t.Errorf("docPageBreakRe(%q) = %v, want %v", in, got, want)
		}
	}
	// 页码行匹配；正文行（含页码字样）不误匹配。
	for _, in := range []string{"第 1 页", "第10页", "  第 2 页  "} {
		if !docPageNumRe.MatchString(strings.TrimSpace(in)) {
			t.Errorf("docPageNumRe 应匹配页码行 %q", in)
		}
	}
	for _, in := range []string{"第 页", "正文第 1 页"} {
		if docPageNumRe.MatchString(strings.TrimSpace(in)) {
			t.Errorf("docPageNumRe 误匹配非页码行 %q", in)
		}
	}
}

// ---- 缓存命名与路径约束 ----

func TestDocCacheName(t *testing.T) {
	// 路径 hash：同路径稳定；扩展名按 format 映射。
	got := docCacheName("/a/b/报告.pdf", "text")
	if !strings.HasPrefix(got, "报告.") || !strings.HasSuffix(got, ".md") {
		t.Errorf("docCacheName = %q, want 报告.<hash>.md", got)
	}
	if !strings.HasSuffix(docCacheName("/a/b/报告.pdf", "json"), ".json") {
		t.Error("json format should map to .json")
	}
	if a, b := docCacheName("/a/b/报告.pdf", "text"), docCacheName("/a/b/报告.pdf", "text"); a != b {
		t.Errorf("同源命名不稳定: %q != %q", a, b)
	}
}

func TestResolveInCache(t *testing.T) {
	root := "/tmp/xx-cache"
	if abs, err := resolveInCache(root, filepath.Join(root, "abc.md")); err != nil || abs != filepath.Join(root, "abc.md") {
		t.Errorf("缓存内绝对路径应放行: %v %v", abs, err)
	}
	if abs, err := resolveInCache(root, "abc.md"); err != nil || abs != filepath.Join(root, "abc.md") {
		t.Errorf("相对路径应解析到缓存内: %v %v", abs, err)
	}
	for _, p := range []string{"/etc/passwd", "../x", filepath.Join(root, "..", "x")} {
		if _, err := resolveInCache(root, p); err == nil {
			t.Errorf("越界路径 %q 应拒绝", p)
		}
	}
	if _, err := resolveInCache("", "x"); err == nil {
		t.Error("空缓存根应报错")
	}
}

// ---- doc_parse 执行 ----

// newLitCfg 构造测试用缓存根（临时目录，保证测试隔离）。
func newLitCfg(t *testing.T) *LitConfig {
	t.Helper()
	return &LitConfig{Root: t.TempDir()}
}

// TestRunLitParse_NoLit 环境无 lit（PATH 指空目录）→ 返回安装指引（fail-closed）。
func TestRunLitParse_NoLit(t *testing.T) {
	t.Setenv("PATH", "/nonexistent")
	src := filepath.Join(t.TempDir(), "doc.pdf")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}
	args, _ := json.Marshal(map[string]string{"source": src})
	res, err := runLitParse(t.Context(), newLitCfg(t), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.IsError || !strings.Contains(res.Data, "lit") {
		t.Errorf("want 安装指引 error, got %q", res.Data)
	}
}

// TestRunLitParse_CacheHit 产物已存在 → 命中返回，无需 lit（PATH 空也过）。
func TestRunLitParse_CacheHit(t *testing.T) {
	t.Setenv("PATH", "/nonexistent")
	cfg := newLitCfg(t)
	src := filepath.Join(t.TempDir(), "doc.pdf")
	if err := os.WriteFile(src, []byte("pdf"), 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}
	out := filepath.Join(cfg.Root, docCacheName(src, "text"))
	if err := os.WriteFile(out, []byte("hello\nworld\n"), 0o644); err != nil {
		t.Fatalf("seed cache: %v", err)
	}
	args, _ := json.Marshal(map[string]string{"source": src})
	res, err := runLitParse(t.Context(), cfg, args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.IsError || !strings.Contains(res.Data, "缓存命中: true") {
		t.Errorf("want 命中返回, got %q", res.Data)
	}
	if !strings.Contains(res.Data, "hello") {
		t.Errorf("摘要应含产物内容, got %q", res.Data)
	}
}

// TestRunLitParse_MissingSource 源文件不存在 → 友好错误。
func TestRunLitParse_MissingSource(t *testing.T) {
	args, _ := json.Marshal(map[string]string{"source": "/no/such/file.pdf"})
	res, err := runLitParse(t.Context(), newLitCfg(t), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.IsError || !strings.Contains(res.Data, "源文件不存在") {
		t.Errorf("want 源文件不存在 error, got %q", res.Data)
	}
}

func TestRunLitParse_BadArgs(t *testing.T) {
	if res, _ := runLitParse(t.Context(), newLitCfg(t), json.RawMessage(`{}`)); !res.IsError || !strings.Contains(res.Data, "source") {
		t.Errorf("空 source 应报错: %q", res.Data)
	}
	args, _ := json.Marshal(map[string]string{"source": "x", "format": "xml"})
	if res, _ := runLitParse(t.Context(), newLitCfg(t), args); !res.IsError || !strings.Contains(res.Data, "format") {
		t.Errorf("非法 format 应报错: %q", res.Data)
	}
}

// ---- doc_read 执行 ----

func TestRunLitRead(t *testing.T) {
	cfg := newLitCfg(t)
	p := filepath.Join(cfg.Root, "a.md")
	if err := os.WriteFile(p, []byte("l1\nl2\nl3\nl4\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	// 窗口读（limit=2）：读到 l1/l2，提示续读起点。
	args, _ := json.Marshal(map[string]any{"path": p, "limit": 2})
	res, err := runLitRead(cfg, args)
	if err != nil || res.IsError {
		t.Fatalf("read failed: %v %q", err, res.Data)
	}
	if !strings.Contains(res.Data, "l2") || !strings.Contains(res.Data, "（继续可 offset=3）") {
		t.Errorf("缺窗口与续读提示: %q", res.Data)
	}
	// 越界路径拒绝。
	args, _ = json.Marshal(map[string]string{"path": "/etc/passwd"})
	if res, _ := runLitRead(cfg, args); !res.IsError || !strings.Contains(res.Data, "越界") {
		t.Errorf("越界读应拒绝: %q", res.Data)
	}
}

// ---- doc_clean 执行 ----

func TestRunLitClean(t *testing.T) {
	cfg := newLitCfg(t)
	p := filepath.Join(cfg.Root, "a.md")
	if err := os.WriteFile(p, []byte("----\n人民法院案例库 人民法院案例库\n正文\n第 1 页\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	args, _ := json.Marshal(map[string]string{"path": p})
	res, err := runLitClean(cfg, args)
	if err != nil || res.IsError {
		t.Fatalf("clean failed: %v %q", err, res.Data)
	}
	if !strings.Contains(res.Data, "分页符行: 1") || !strings.Contains(res.Data, "页码行: 1") || !strings.Contains(res.Data, "行内重复折叠: 1") {
		t.Errorf("报告缺项: %q", res.Data)
	}
	// 文件已原地改写：噪声消失、正文保留。
	got, _ := os.ReadFile(p)
	s := string(got)
	if strings.Contains(s, "----") || strings.Contains(s, "第 1 页") || strings.Contains(s, "人民法院案例库 人民法院案例库") {
		t.Errorf("清洗未生效: %q", s)
	}
	if !strings.Contains(s, "正文") {
		t.Errorf("清洗误删正文: %q", s)
	}
	// 越界路径拒绝。
	args, _ = json.Marshal(map[string]string{"path": "/etc/hosts"})
	if res, _ := runLitClean(cfg, args); !res.IsError || !strings.Contains(res.Data, "越界") {
		t.Errorf("越界清洗应拒绝: %q", res.Data)
	}
}

// ---- 工具壳完整性 ----

// TestLitToolsSpec 三工具的 Spec 三件套齐备（名称/描述/参数），防声明缺字段。
func TestLitToolsSpec(t *testing.T) {
	cfg := &LitConfig{Root: t.TempDir()}
	tools := []tool.Tool{LitParse(cfg), LitRead(cfg), LitClean(cfg)}
	for _, tl := range tools {
		spec := tl.Spec()
		if spec.Name == "" || spec.Description == "" || len(spec.Parameters) == 0 {
			t.Errorf("工具 Spec 不完整: %+v", spec)
		}
	}
}
