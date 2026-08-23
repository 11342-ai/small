package session

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// newTestStore 构造基于临时目录的仓库，测试结束自动清理。
func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func TestStore_AppendLoadRoundTrip(t *testing.T) {
	s := newTestStore(t)
	want := []Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Name: "echo", Args: `{"v":"hi"}`}}},
		{Role: "tool", ToolCallID: "c1", Content: "hi"},
		{Role: "assistant", Content: "done"},
	}
	if err := s.Append("s1", want); err != nil {
		t.Fatalf("Append: %v", err)
	}

	got, err := s.Load("s1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round-trip = %+v, want %+v", got, want)
	}
}

func TestStore_AppendIsAppendOnly(t *testing.T) {
	s := newTestStore(t)
	if err := s.Append("s1", []Message{{Role: "user", Content: "a"}}); err != nil {
		t.Fatalf("Append 1: %v", err)
	}
	if err := s.Append("s1", []Message{{Role: "user", Content: "b"}}); err != nil {
		t.Fatalf("Append 2: %v", err)
	}
	got, err := s.Load("s1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []Message{{Role: "user", Content: "a"}, {Role: "user", Content: "b"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("load = %+v, want %+v", got, want)
	}
}

func TestStore_LoadNewSessionIsEmpty(t *testing.T) {
	s := newTestStore(t)
	got, err := s.Load("missing")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("new session = %+v, want empty", got)
	}
}

func TestStore_LoadSkipsCorruptLine(t *testing.T) {
	s := newTestStore(t)
	if err := s.Append("s1", []Message{{Role: "user", Content: "ok"}}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	// 模拟崩溃残留：以追加方式在文件末尾写入半行非法 JSON。
	f, err := os.OpenFile(s.path("s1"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open for corrupt append: %v", err)
	}
	if _, err := f.Write([]byte("{corrupt-line\n")); err != nil {
		t.Fatalf("corrupt append: %v", err)
	}
	f.Close()

	got, err := s.Load("s1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []Message{{Role: "user", Content: "ok"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("load = %+v, want %+v (corrupt line skipped)", got, want)
	}
}

func TestStore_ListSortedAndStable(t *testing.T) {
	s := newTestStore(t)
	for _, id := range []string{"b", "a", "c"} {
		if err := s.Append(id, []Message{{Role: "user", Content: id}}); err != nil {
			t.Fatalf("Append %q: %v", id, err)
		}
	}

	first, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	second, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Errorf("List unstable: %+v vs %+v", first, second)
	}
	ids := make([]string, len(first))
	for i, m := range first {
		ids[i] = m.ID
	}
	want := []string{"a", "b", "c"}
	if !reflect.DeepEqual(ids, want) {
		t.Errorf("List ids = %v, want %v", ids, want)
	}
	if first[0].TurnCount != 1 {
		t.Errorf("TurnCount = %d, want 1", first[0].TurnCount)
	}
}

func TestStore_DeleteIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	if err := s.Append("s1", []Message{{Role: "user", Content: "x"}}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := s.Delete("s1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := s.Delete("s1"); err != nil {
		t.Fatalf("Delete again (idempotent): %v", err)
	}
	if err := s.Delete("never-existed"); err != nil {
		t.Fatalf("Delete missing: %v", err)
	}
}

func TestStore_RewriteOverwrites(t *testing.T) {
	s := newTestStore(t)
	if err := s.Append("s1", []Message{
		{Role: "user", Content: "a"},
		{Role: "assistant", Content: "b"},
		{Role: "user", Content: "c"},
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	// 模拟截断：只保留最近一条，全量覆写。
	if err := s.Rewrite("s1", []Message{{Role: "user", Content: "c"}}); err != nil {
		t.Fatalf("Rewrite: %v", err)
	}
	got, err := s.Load("s1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 1 || got[0].Content != "c" {
		t.Errorf("after rewrite = %+v, want [c]", got)
	}
	// Rewrite 后仍可继续 Append（追加到重写后的文件上，不复活旧数据）。
	if err := s.Append("s1", []Message{{Role: "assistant", Content: "d"}}); err != nil {
		t.Fatalf("Append after rewrite: %v", err)
	}
	got, err = s.Load("s1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 2 || got[0].Content != "c" || got[1].Content != "d" {
		t.Errorf("after append = %+v, want [c, d]", got)
	}
}

func TestStore_RewriteEmptyFile(t *testing.T) {
	s := newTestStore(t)
	if err := s.Append("s1", []Message{{Role: "user", Content: "x"}}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := s.Rewrite("s1", nil); err != nil {
		t.Fatalf("Rewrite empty: %v", err)
	}
	got, err := s.Load("s1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("after empty rewrite = %+v, want empty", got)
	}
}

func TestStore_SessionsAreIsolated(t *testing.T) {
	s := newTestStore(t)
	if err := s.Append("s1", []Message{{Role: "user", Content: "one"}}); err != nil {
		t.Fatalf("Append s1: %v", err)
	}
	if err := s.Append("s2", []Message{{Role: "user", Content: "two"}}); err != nil {
		t.Fatalf("Append s2: %v", err)
	}
	got, err := s.Load("s1")
	if err != nil {
		t.Fatalf("Load s1: %v", err)
	}
	if len(got) != 1 || got[0].Content != "one" {
		t.Errorf("s1 = %+v, want [one]", got)
	}
}

func TestStore_InvalidIDRejected(t *testing.T) {
	s := newTestStore(t)
	for _, id := range []string{"", ".", "..", "a/b", `a\b`} {
		if err := s.Append(id, nil); err == nil {
			t.Errorf("Append id %q: want error", id)
		}
		if _, err := s.Load(id); err == nil {
			t.Errorf("Load id %q: want error", id)
		}
		if err := s.Delete(id); err == nil {
			t.Errorf("Delete id %q: want error", id)
		}
	}
}

func TestStore_NewCreatesDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "sessions")
	if _, err := New(dir); err != nil {
		t.Fatalf("New nested dir: %v", err)
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Errorf("dir not created: %v", err)
	}
}
