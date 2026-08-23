package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"small/internal/session"
	"small/internal/tool"
)

// persistedStore 构造基于临时目录的会话仓库，返回仓库与其目录（断言落盘文件用）。
func persistedStore(t *testing.T) (*session.Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := session.New(dir)
	if err != nil {
		t.Fatalf("session.New: %v", err)
	}
	return s, dir
}

// TestPersist_RunAppendsAndResumeContinues 全链路核心用例：
// 跑一轮落盘 → 用恢复的历史构造新 agent → 再跑一轮能引用前文（续聊）。
func TestPersist_RunAppendsAndResumeContinues(t *testing.T) {
	store, _ := persistedStore(t)

	first := &scriptCompleter{results: []Result{{Reply: "one"}}}
	a1 := New(first, nil, store, WithSession("s1"))
	if _, err := a1.Run(context.Background(), "first"); err != nil {
		t.Fatalf("run 1: %v", err)
	}

	// 模拟"重启"：从磁盘读回历史，注入新 agent。
	msgs, err := store.Load("s1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("persisted lines = %d, want 2 (user + assistant)", len(msgs))
	}

	second := &scriptCompleter{results: []Result{{Reply: "two"}}}
	a2 := New(second, nil, store, WithSession("s1"), WithHistory(FromSession(msgs)))
	if _, err := a2.Run(context.Background(), "second"); err != nil {
		t.Fatalf("run 2: %v", err)
	}

	// 第二轮请求应携带完整历史（含前一轮的 user/assistant），证明上下文续上了。
	want := []Turn{
		{Role: "user", Content: "first"},
		{Role: "assistant", Content: "one"},
		{Role: "user", Content: "second"},
	}
	if got := second.captured[0]; !reflect.DeepEqual(got, want) {
		t.Errorf("resumed turns = %+v, want %+v", got, want)
	}

	// 落盘总数应为 4 条（两轮各 2 条），无重复追加。
	msgs, err = store.Load("s1")
	if err != nil {
		t.Fatalf("Load after run 2: %v", err)
	}
	if len(msgs) != 4 {
		t.Errorf("persisted lines = %d, want 4", len(msgs))
	}
}

// TestPersist_ThinkingNotStored 落盘内容不含思考过程（不回传模型，故不存）。
func TestPersist_ThinkingNotStored(t *testing.T) {
	store, dir := persistedStore(t)
	script := &scriptCompleter{results: []Result{{Reply: "r", Thinking: "secret"}}}
	a := New(script, nil, store, WithSession("s1"))
	if _, err := a.Run(context.Background(), "hi"); err != nil {
		t.Fatalf("run: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "s1.jsonl"))
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if strings.Contains(string(data), "secret") {
		t.Error("thinking must not be persisted")
	}
}

// TestPersist_NoStoreIsNoop store 为 nil 时 WithSession 不应产生副作用或错误。
func TestPersist_NoStoreIsNoop(t *testing.T) {
	script := &scriptCompleter{results: []Result{{Reply: "r"}}}
	a := New(script, nil, nil, WithSession("s1"))
	if _, err := a.Run(context.Background(), "hi"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(a.history) != 2 {
		t.Errorf("history = %d, want 2", len(a.history))
	}
}

// TestPersist_ResetDeletesSessionFile Reset 后会话文件消失，重新对话从零开始。
func TestPersist_ResetDeletesSessionFile(t *testing.T) {
	store, _ := persistedStore(t)
	script := &scriptCompleter{results: []Result{{Reply: "r1"}, {Reply: "r2"}}}
	a := New(script, nil, store, WithSession("s1"))
	if _, err := a.Run(context.Background(), "hi"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if err := a.Reset(); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if msgs, _ := store.Load("s1"); len(msgs) != 0 {
		t.Errorf("after reset, persisted = %d, want 0", len(msgs))
	}
	// 重置后继续对话，落盘应为全新一轮（2 条）。
	if _, err := a.Run(context.Background(), "again"); err != nil {
		t.Fatalf("run after reset: %v", err)
	}
	if msgs, _ := store.Load("s1"); len(msgs) != 2 {
		t.Errorf("after rerun, persisted = %d, want 2", len(msgs))
	}
}

// TestPersist_FailurePropagates 落盘失败必须透传错误，不能静默丢数据。
func TestPersist_FailurePropagates(t *testing.T) {
	store, dir := persistedStore(t)
	script := &scriptCompleter{results: []Result{{Reply: "r"}}}
	a := New(script, nil, store, WithSession("s1"))

	// 目录被删后 Append 的 OpenFile 必然失败，模拟落盘故障。
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("remove dir: %v", err)
	}
	_, err := a.Run(context.Background(), "hi")
	if err == nil {
		t.Fatal("want persist error, got nil")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("error = %v, want not-exist wrapped", err)
	}
}

// TestPersist_ToolLoopPersisted 工具循环的多轮内部消息同样完整落盘（含 ToolCalls）。
func TestPersist_ToolLoopPersisted(t *testing.T) {
	store, _ := persistedStore(t)
	reg := tool.New()
	if err := reg.Register(scriptTool()); err != nil {
		t.Fatalf("register: %v", err)
	}
	script := &scriptCompleter{results: []Result{
		{ToolCalls: []ToolCall{toolCall("c1", "echo", `{"v":"hi"}`)}},
		{Reply: "done"},
	}}
	a := New(script, reg, store, WithSession("s1"))
	if _, err := a.Run(context.Background(), "hi"); err != nil {
		t.Fatalf("run: %v", err)
	}

	msgs, err := store.Load("s1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(msgs) != 4 {
		t.Fatalf("persisted = %d, want 4", len(msgs))
	}
	// assistant 那条应携带 ToolCalls，tool 那条应带 ToolCallID。
	if !reflect.DeepEqual(msgs[1].ToolCalls, []session.ToolCall{{ID: "c1", Name: "echo", Args: `{"v":"hi"}`}}) {
		t.Errorf("tool call persisted = %+v", msgs[1].ToolCalls)
	}
	if msgs[2].ToolCallID != "c1" || msgs[2].Content != "hi" {
		t.Errorf("tool result persisted = %+v", msgs[2])
	}
}
