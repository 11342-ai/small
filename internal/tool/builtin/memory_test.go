package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"small/internal/agent"
	"small/internal/memory"
	"small/internal/tool"
)

// newMemWithFile 构造带一条中文记忆的仓库（临时目录）。
func newMemWithFile(t *testing.T, name, content string) *memory.Store {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	mem, err := memory.New(dir)
	if err != nil {
		t.Fatalf("memory.New: %v", err)
	}
	return mem
}

func TestMemorySearchTool_ExecuteHit(t *testing.T) {
	mem := newMemWithFile(t, "MEMORY.md", "## 部署决策\n我们决定用 JSONL 存储会话。")
	res, err := MemorySearch(mem).Execute(context.Background(), json.RawMessage(`{"query":"JSONL"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.IsError {
		t.Fatalf("want success, got error result: %s", res.Data)
	}
	if !strings.Contains(res.Data, "MEMORY.md#0") || !strings.Contains(res.Data, "JSONL") {
		t.Errorf("result = %q, want Ref + snippet", res.Data)
	}
}

func TestMemorySearchTool_ExecuteNoMatchIsError(t *testing.T) {
	mem := newMemWithFile(t, "MEMORY.md", "## 主题\n苹果")
	res, err := MemorySearch(mem).Execute(context.Background(), json.RawMessage(`{"query":"香蕉"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !res.IsError {
		t.Error("no-match should be business failure (IsError)")
	}
	if !strings.Contains(res.Data, "未找到") {
		t.Errorf("result = %q, want 未找到相关记忆", res.Data)
	}
}

func TestMemorySearchTool_ExecuteBadArgsIsError(t *testing.T) {
	mem := newMemWithFile(t, "MEMORY.md", "## 主题\n苹果")
	res, err := MemorySearch(mem).Execute(context.Background(), json.RawMessage(`{"query":""}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !res.IsError {
		t.Error("empty query should be business failure (IsError)")
	}
}

func TestMemoryGetTool_ExecuteRoundTrip(t *testing.T) {
	mem := newMemWithFile(t, "memory/notes.md", "## 决策\n采用 JSONL。")
	// 先 search 确认命中，再按 Ref 精读整块。
	searchRes, err := MemorySearch(mem).Execute(context.Background(), json.RawMessage(`{"query":"JSONL"}`))
	if err != nil {
		t.Fatalf("search Execute: %v", err)
	}
	if searchRes.IsError {
		t.Fatalf("search failed: %s", searchRes.Data)
	}
	getRes, err := MemoryGet(mem).Execute(context.Background(), json.RawMessage(`{"ref":"memory/notes.md#0"}`))
	if err != nil {
		t.Fatalf("get Execute: %v", err)
	}
	if getRes.IsError {
		t.Fatalf("want success, got error: %s", getRes.Data)
	}
	if !strings.Contains(getRes.Data, "JSONL") {
		t.Errorf("get result = %q, want full chunk", getRes.Data)
	}
}

func TestMemoryGetTool_ExecuteBadRefIsError(t *testing.T) {
	mem := newMemWithFile(t, "MEMORY.md", "## 主题\n苹果")
	res, err := MemoryGet(mem).Execute(context.Background(), json.RawMessage(`{"ref":"nope.md#9"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !res.IsError {
		t.Error("missing ref should be business failure (IsError)")
	}
}

func TestMemorySaveTool_ExecuteAndSearchable(t *testing.T) {
	mem := newMemWithFile(t, "MEMORY.md", "## 既有\n苹果")
	res, err := MemorySave(mem).Execute(context.Background(),
		json.RawMessage(`{"topic":"偏好","content":"用户喜欢中文交流"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.IsError {
		t.Fatalf("want success, got error: %s", res.Data)
	}
	// 写入后可被 memory_search 检索到。
	searchRes, err := MemorySearch(mem).Execute(context.Background(),
		json.RawMessage(`{"query":"中文交流"}`))
	if err != nil {
		t.Fatalf("search Execute: %v", err)
	}
	if searchRes.IsError || !strings.Contains(searchRes.Data, "用户喜欢中文交流") {
		t.Errorf("search after save = %q (IsError=%v), want hit", searchRes.Data, searchRes.IsError)
	}
	// 常驻层不被写：memory_save 只进归档层。
	boot, err := mem.Get("MEMORY.md#0")
	if err != nil {
		t.Fatalf("Get MEMORY.md: %v", err)
	}
	if strings.Contains(boot, "中文交流") {
		t.Error("MEMORY.md should not contain saved content（锁死只写归档层）")
	}
}

func TestMemorySaveTool_EmptyContentIsError(t *testing.T) {
	mem := newMemWithFile(t, "MEMORY.md", "## 主题\n苹果")
	res, err := MemorySave(mem).Execute(context.Background(),
		json.RawMessage(`{"topic":"x","content":"  "}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !res.IsError {
		t.Error("empty content should be business failure (IsError)")
	}
}

// stubCompleter 按脚本返回结果并记录收到的历史（agent 包测试桩的同款模式，
// 在 builtin 包内自持一份，避免跨包测试耦合）。
type stubCompleter struct {
	results  []agent.Result
	captured [][]agent.Turn
	calls    int
}

func (s *stubCompleter) Complete(_ context.Context, turns []agent.Turn) (agent.Result, error) {
	if s.calls >= len(s.results) {
		return agent.Result{}, errors.New("stub exhausted")
	}
	s.captured = append(s.captured, append([]agent.Turn(nil), turns...))
	r := s.results[s.calls]
	s.calls++
	return r, nil
}

// TestAgentLoop_MemoryToolIntegration 端到端：模型第一轮请求 memory_search，
// 工具结果以 tool role 回灌，第二轮模型据此作答。
// 注意：本测试 import agent 只存在于测试二进制，不参与生产依赖图
// （与 agent/imports_test.go 的"测试可自由 import"同一约定）。
func TestAgentLoop_MemoryToolIntegration(t *testing.T) {
	mem := newMemWithFile(t, "MEMORY.md", "## 部署决策\n我们决定用 JSONL 存储会话。")
	reg := tool.New()
	if err := RegisterBuiltins(reg, Deps{Mem: mem}); err != nil {
		t.Fatalf("register: %v", err)
	}
	script := &stubCompleter{results: []agent.Result{
		{ToolCalls: []agent.ToolCall{{ID: "c1", Name: "memory_search", Args: `{"query":"JSONL"}`}}},
		{Reply: "我们用了 JSONL。"},
	}}
	a := agent.New(script, reg, nil, agent.WithSystemPrompt("sys"))

	result, err := a.Run(context.Background(), "会话用什么格式？")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Reply != "我们用了 JSONL。" {
		t.Errorf("reply = %q", result.Reply)
	}
	// 第二轮历史应包含：assistant(带调用) + tool(检索结果回灌)。
	toolTurn := script.captured[1][len(script.captured[1])-1]
	if toolTurn.Role != "tool" || toolTurn.ToolCallID != "c1" {
		t.Fatalf("tool turn = %+v, want tool role with c1", toolTurn)
	}
	if !strings.Contains(toolTurn.Content, "JSONL") {
		t.Errorf("tool result = %q, want search hits", toolTurn.Content)
	}
}

// TestAgentLoop_MemorySaveIntegration 端到端：模型调用 memory_save 落盘，
// 结果以 tool role 回灌，且新内容可被后续 memory_search 检索到。
func TestAgentLoop_MemorySaveIntegration(t *testing.T) {
	mem := newMemWithFile(t, "MEMORY.md", "## 既有\n苹果")
	reg := tool.New()
	if err := RegisterBuiltins(reg, Deps{Mem: mem}); err != nil {
		t.Fatalf("register: %v", err)
	}
	script := &stubCompleter{results: []agent.Result{
		{ToolCalls: []agent.ToolCall{{ID: "c1", Name: "memory_save", Args: `{"topic":"偏好","content":"用户喜欢中文交流"}`}}},
		{Reply: "已记住。"},
	}}
	a := agent.New(script, reg, nil, agent.WithSystemPrompt("sys"))

	result, err := a.Run(context.Background(), "记住我喜欢中文")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Reply != "已记住。" {
		t.Errorf("reply = %q", result.Reply)
	}
	// 工具结果回灌成功。
	toolTurn := script.captured[1][len(script.captured[1])-1]
	if toolTurn.Role != "tool" || toolTurn.ToolCallID != "c1" || !strings.Contains(toolTurn.Content, "已记住") {
		t.Fatalf("tool turn = %+v, want success feedback", toolTurn)
	}
	// 落盘可检索（归档层，常驻层不受影响）。
	searchRes, err := MemorySearch(mem).Execute(context.Background(), json.RawMessage(`{"query":"中文交流"}`))
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if searchRes.IsError || !strings.Contains(searchRes.Data, "用户喜欢中文交流") {
		t.Errorf("search after loop = %q (IsError=%v), want hit", searchRes.Data, searchRes.IsError)
	}
}
