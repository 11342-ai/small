package memory

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// newTestStore 构造基于临时目录的仓库，返回仓库与目录（便于写记忆文件）。
func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, dir
}

// writeMemory 在记忆目录下写入文件（自动建父目录）。
func writeMemory(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll %s: %v", name, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile %s: %v", name, err)
	}
}

func TestNew_EmptyDir(t *testing.T) {
	if _, err := New(""); err == nil {
		t.Error("empty dir should error")
	}
}

func TestSplitChunks(t *testing.T) {
	got := splitChunks("## 主题A\n内容a\n\n## 主题B\n内容b")
	want := []string{"## 主题A\n内容a", "## 主题B\n内容b"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("splitChunks = %#v, want %#v", got, want)
	}
}

func TestSplitChunks_HeaderBlockAndEmptySkipped(t *testing.T) {
	// 首个标题前的头部作为块 0；空块跳过。
	got := splitChunks("# 记忆\n\n## 主题\n内容\n\n## 空块\n\n## 有内容\nx")
	want := []string{"# 记忆", "## 主题\n内容", "## 空块", "## 有内容\nx"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("splitChunks = %#v, want %#v", got, want)
	}
}

func TestSearch_ChunkByTitle(t *testing.T) {
	s, dir := newTestStore(t)
	writeMemory(t, dir, "MEMORY.md", `# 记忆

## 部署决策
我们决定用 JSONL 存储会话。

## 检索方案
采用 bigram 分词，零依赖。
`)
	hits, err := s.Search("JSONL", 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("len(hits) = %d, want 1 (hits=%#v)", len(hits), hits)
	}
	if hits[0].Ref != "MEMORY.md#1" {
		t.Errorf("Ref = %q, want %q（部署决策块）", hits[0].Ref, "MEMORY.md#1")
	}
	if !strings.Contains(hits[0].Snippet, "JSONL") {
		t.Errorf("Snippet = %q, want contains JSONL", hits[0].Snippet)
	}
}

func TestSearch_ChineseBigram(t *testing.T) {
	s, dir := newTestStore(t)
	writeMemory(t, dir, "MEMORY.md", "## 决策\n我们决定用 JSONL 存储会话，理由是可读性好。")
	hits, err := s.Search("我们决定", 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("len(hits) = %d, want 1（中文短语应命中）", len(hits))
	}
}

func TestSearch_IdentifierSubstring(t *testing.T) {
	s, dir := newTestStore(t)
	writeMemory(t, dir, "memory/tools.md", "## 工具清单\n内置 memory_search 与 memory_get 两个检索工具。")
	hits, err := s.Search("memory_search", 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("len(hits) = %d, want 1（标识符应命中）", len(hits))
	}
	if hits[0].Ref != "memory/tools.md#0" {
		t.Errorf("Ref = %q, want %q", hits[0].Ref, "memory/tools.md#0")
	}
}

func TestSearch_MultiTermRanksHigher(t *testing.T) {
	s, dir := newTestStore(t)
	writeMemory(t, dir, "memory/a.md", "## 主题\n苹果")
	writeMemory(t, dir, "memory/b.md", "## 主题\n苹果 香蕉")
	hits, err := s.Search("苹果 香蕉", 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("len(hits) = %d, want 2", len(hits))
	}
	// 多词命中的 b 应排在前，同分则按 Ref 字典序。
	if hits[0].Ref != "memory/b.md#0" {
		t.Errorf("hits[0].Ref = %q, want memory/b.md#0（多词命中应更相关）", hits[0].Ref)
	}
}

func TestSearch_TieBreaksByRef(t *testing.T) {
	s, dir := newTestStore(t)
	writeMemory(t, dir, "memory/b.md", "## 主题\n苹果")
	writeMemory(t, dir, "memory/a.md", "## 主题\n苹果")
	hits, err := s.Search("苹果", 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("len(hits) = %d, want 2", len(hits))
	}
	// 同分：按 Ref 字典序 a 在 b 前。
	if hits[0].Ref != "memory/a.md#0" || hits[1].Ref != "memory/b.md#0" {
		t.Errorf("order = %q, %q; want a, b", hits[0].Ref, hits[1].Ref)
	}
}

func TestSearch_StableAcrossCalls(t *testing.T) {
	s, dir := newTestStore(t)
	writeMemory(t, dir, "memory/b.md", "## 主题\n苹果")
	writeMemory(t, dir, "memory/a.md", "## 主题\n苹果")
	first, err := s.Search("苹果", 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	second, err := s.Search("苹果", 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Errorf("repeated Search results differ: %#v vs %#v", first, second)
	}
}

func TestSearch_NoMatch(t *testing.T) {
	s, dir := newTestStore(t)
	writeMemory(t, dir, "MEMORY.md", "## 主题\n苹果")
	hits, err := s.Search("香蕉", 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("len(hits) = %d, want 0", len(hits))
	}
}

func TestSearch_EmptyQuery(t *testing.T) {
	s, _ := newTestStore(t)
	if _, err := s.Search("", 5); err == nil {
		t.Error("empty query should error")
	}
}

func TestSearch_BadLimit(t *testing.T) {
	s, _ := newTestStore(t)
	if _, err := s.Search("x", 0); err == nil {
		t.Error("limit < 1 should error")
	}
}

func TestSearch_ToleratesMissingArchiveDir(t *testing.T) {
	// 只有 MEMORY.md、没有 memory/ 子目录：不算错误。
	s, dir := newTestStore(t)
	writeMemory(t, dir, "MEMORY.md", "## 主题\n苹果")
	if _, err := s.Search("苹果", 5); err != nil {
		t.Fatalf("Search without archive dir: %v", err)
	}
}

func TestSearch_SkipsEmptyFiles(t *testing.T) {
	// 空文件不产生块，检索不报错。
	s, dir := newTestStore(t)
	writeMemory(t, dir, "memory/empty.md", "")
	writeMemory(t, dir, "memory/normal.md", "## 主题\n苹果")
	hits, err := s.Search("苹果", 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 {
		t.Errorf("len(hits) = %d, want 1（空文件应被跳过）", len(hits))
	}
}

func TestGet_RoundTrip(t *testing.T) {
	s, dir := newTestStore(t)
	writeMemory(t, dir, "MEMORY.md", "## 部署决策\n我们决定用 JSONL 存储会话。")
	hits, err := s.Search("JSONL", 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("len(hits) = %d, want 1", len(hits))
	}
	got, err := s.Get(hits[0].Ref)
	if err != nil {
		t.Fatalf("Get(%q): %v", hits[0].Ref, err)
	}
	if want := "## 部署决策\n我们决定用 JSONL 存储会话。"; got != want {
		t.Errorf("Get = %q, want %q", got, want)
	}
}

func TestGet_NotFound(t *testing.T) {
	s, _ := newTestStore(t)
	if _, err := s.Get("memory/none.md#0"); err == nil {
		t.Error("missing ref should error")
	}
	if _, err := s.Get(""); err == nil {
		t.Error("empty ref should error")
	}
}

func TestRefresh_OnChange(t *testing.T) {
	s, dir := newTestStore(t)
	path := filepath.Join(dir, "MEMORY.md")
	writeMemory(t, dir, "MEMORY.md", "## 主题\n苹果")

	hits, err := s.Search("苹果", 5)
	if err != nil || len(hits) != 1 {
		t.Fatalf("initial Search: hits=%d err=%v, want 1 hit", len(hits), err)
	}

	// 覆盖写文件内容，并显式推进 mtime 保证刷新一定触发（避免同粒度时间戳）。
	writeMemory(t, dir, "MEMORY.md", "## 主题\n香蕉")
	future := time.Now().Add(time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	hits, err = s.Search("香蕉", 5)
	if err != nil || len(hits) != 1 {
		t.Fatalf("Search after change: hits=%d err=%v, want 1 hit", len(hits), err)
	}
	hits, err = s.Search("苹果", 5)
	if err != nil {
		t.Fatalf("Search stale term: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("stale term hits = %d, want 0（索引应已刷新）", len(hits))
	}
}

// TestSearch_DemoChinese 中文检索效果演示：构造贴近项目真实形态的中文记忆文件，
// 用多个中文关键词验证命中与排序（-run DemoChinese -v 可见命中明细）。
func TestSearch_DemoChinese(t *testing.T) {
	s, dir := newTestStore(t)
	writeMemory(t, dir, "MEMORY.md", `# 长期记忆

## 用户偏好
用户习惯使用中文交流，偏好零依赖的纯 Go 实现。

## 项目决策
会话持久化采用 JSONL，每会话一个文件；不引入 SQLite。

## 踩坑记录
流式拿到 2xx 后绝不重试，否则会重复已吐出的 token。

## 工具清单
内置 memory_search 与 memory_get 两个检索工具。
`)
	for _, q := range []string{"JSONL", "重试", "用户 中文", "memory_search", "不存在的词"} {
		hits, err := s.Search(q, 3)
		if err != nil {
			t.Fatalf("Search(%q): %v", q, err)
		}
		t.Logf("query %q -> %d hits", q, len(hits))
		for _, h := range hits {
			t.Logf("  [%s] score=%.3f snippet=%q", h.Ref, h.Score, h.Snippet)
		}
	}
}

func TestAppend_WritesToArchiveAndSearchable(t *testing.T) {
	s, dir := newTestStore(t)
	if err := s.Append("20260823", "## 备忘\n记住了 JSONL"); err != nil {
		t.Fatalf("Append: %v", err)
	}
	// 落盘位置：归档层 memory/20260823.md。
	data, err := os.ReadFile(filepath.Join(dir, "memory", "20260823.md"))
	if err != nil {
		t.Fatalf("read archive: %v", err)
	}
	if !strings.Contains(string(data), "记住了 JSONL") {
		t.Errorf("archive = %q, want appended content", data)
	}
	// 惰性索引：写入后 Search 立即可见。
	hits, err := s.Search("JSONL", 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 || hits[0].Ref != "memory/20260823.md#0" {
		t.Errorf("hits = %#v, want 1 hit at memory/20260823.md#0", hits)
	}
	// 常驻层不被写：Append 只进归档层。
	if _, err := os.Stat(filepath.Join(dir, "MEMORY.md")); !os.IsNotExist(err) {
		t.Error("MEMORY.md should not be created by Append（锁死只写归档层）")
	}
}

func TestAppend_AppendsToSameFile(t *testing.T) {
	s, _ := newTestStore(t)
	if err := s.Append("20260823", "## 一\n第一段"); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := s.Append("20260823", "## 二\n第二段"); err != nil {
		t.Fatalf("Append: %v", err)
	}
	// 两次追加进同一文件，且各自成块可检索。
	hits, err := s.Search("第二段", 5)
	if err != nil || len(hits) != 1 {
		t.Fatalf("Search second: hits=%d err=%v, want 1", len(hits), err)
	}
	if hits[0].Ref != "memory/20260823.md#1" {
		t.Errorf("Ref = %q, want memory/20260823.md#1（第二块）", hits[0].Ref)
	}
}

func TestAppend_InvalidName(t *testing.T) {
	s, _ := newTestStore(t)
	for _, name := range []string{"", "../x", "a/b", `a\b`, "a.b", "a b"} {
		if err := s.Append(name, "内容"); err == nil {
			t.Errorf("Append(%q) should error（防路径注入）", name)
		}
	}
}

func TestAppend_EmptyContent(t *testing.T) {
	s, _ := newTestStore(t)
	if err := s.Append("20260823", "  "); err == nil {
		t.Error("empty content should error")
	}
}
