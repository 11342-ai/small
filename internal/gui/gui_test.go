package gui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"small/internal/agent"
)

// scriptCompleter 按脚本返回回复（无工具调用），驱动 agent 做确定性对话。
type scriptCompleter struct {
	replies []string
	i       int
}

func (s *scriptCompleter) Complete(_ context.Context, _ []agent.Turn) (agent.Result, error) {
	if s.i >= len(s.replies) {
		return agent.Result{}, errors.New("script exhausted")
	}
	r := s.replies[s.i]
	s.i++
	return agent.Result{Reply: r}, nil
}

// newTestServer 构造带 mock agent 的 server（缓存根/工作区根用临时目录）。
func newTestServer(t *testing.T, replies ...string) *Server {
	t.Helper()
	root := t.TempDir()
	srv := New(Config{FileRoot: root, CacheRoot: root})
	a := agent.New(&scriptCompleter{replies: replies}, nil, nil, agent.WithReplyObserver(srv.OnReply))
	srv.Attach(a)
	return srv
}

// TestHandleCommand 命令分发委托注入的 CommandFunc（gui.md §4.4 v3）：
// 输出直接回复、注入消息走 agent、未知命令透传报错。
func TestHandleCommand(t *testing.T) {
	srv := newTestServer(t)
	srv.cfg.Command = func(_ context.Context, input string) (bool, string, string, error) {
		switch {
		case input == "/persona catton":
			return true, "人格已切换为 catton", "", nil
		case strings.HasPrefix(input, "/pdf "):
			return true, "", "请按 pdf 工作流处理文档：" + strings.TrimPrefix(input, "/pdf "), nil
		case strings.HasPrefix(input, "/"):
			return true, "未知命令：" + strings.Fields(input)[0], "", nil
		}
		return false, "", "", nil
	}
	// 输出直接回复。
	inject, reply, handled := srv.handleCommand(context.Background(), "/persona catton")
	if !handled || inject != "" || !strings.Contains(reply, "catton") {
		t.Errorf("/persona: handled=%v inject=%q reply=%q", handled, inject, reply)
	}
	// 注入消息走 agent。
	inject, reply, handled = srv.handleCommand(context.Background(), "/pdf /tmp/x.pdf")
	if !handled || !strings.Contains(inject, "x.pdf") || reply != "" {
		t.Errorf("/pdf: handled=%v inject=%q reply=%q", handled, inject, reply)
	}
	// 未知命令报错透传。
	_, reply, handled = srv.handleCommand(context.Background(), "/nope")
	if !handled || !strings.Contains(reply, "未知命令") {
		t.Errorf("/nope: handled=%v reply=%q", handled, reply)
	}
	// 非命令返回未处理。
	_, _, handled = srv.handleCommand(context.Background(), "普通消息")
	if handled {
		t.Error("普通消息不应被当作命令")
	}
}

// TestHandleChat_CommandReply 命令走 done 事件直接回复，不调用模型。
func TestHandleChat_CommandReply(t *testing.T) {
	srv := newTestServer(t) // replies 为空：若走了模型会 script exhausted 报错
	srv.cfg.Command = func(_ context.Context, input string) (bool, string, string, error) {
		if strings.HasPrefix(input, "/") {
			return true, "命令已执行：" + input, "", nil
		}
		return false, "", "", nil
	}
	body, _ := json.Marshal(map[string]string{"message": "/persona catton"})
	req := httptest.NewRequest(http.MethodPost, "/chat", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.handleChat(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "命令已执行") {
		t.Errorf("命令应直接回复:\n%s", rec.Body.String())
	}
}

// TestHandleHistory /history 过滤 user/assistant 且跳过空回复（gui.md §4.6）。
func TestHandleHistory(t *testing.T) {
	srv := newTestServer(t)
	srv.a = agent.New(&scriptCompleter{replies: []string{"ok"}}, nil, nil,
		agent.WithHistory([]agent.Turn{
			{Role: "user", Content: "你好"},
			{Role: "assistant", Content: "回复一"},
			{Role: "assistant", Content: "", ToolCalls: []agent.ToolCall{{ID: "t1", Name: "echo", Args: "{}"}}},
			{Role: "tool", Content: "工具结果"},
			{Role: "assistant", Content: "最终回复"},
		}))
	req := httptest.NewRequest(http.MethodGet, "/history", nil)
	rec := httptest.NewRecorder()
	srv.handleHistory(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("应返回 3 条对话消息（tool 与空回复跳过），got %d: %v", len(got), got)
	}
	if got[0].Content != "你好" || got[1].Content != "回复一" || got[2].Content != "最终回复" {
		t.Errorf("历史内容不符: %v", got)
	}
}

// TestHandleChat_DoneEvent /chat 流式响应：done 事件携带完整回复（非流式 mock 兜底路径）。
func TestHandleChat_DoneEvent(t *testing.T) {
	srv := newTestServer(t, "你好，这是回复。")
	body, _ := json.Marshal(map[string]string{"message": "hi"})
	req := httptest.NewRequest(http.MethodPost, "/chat", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.handleChat(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "event: done") || !strings.Contains(rec.Body.String(), "你好，这是回复。") {
		t.Errorf("缺 done 事件与回复:\n%s", rec.Body.String())
	}
}

// TestHandleChat_EmptyMessage 空消息拒绝。
func TestHandleChat_EmptyMessage(t *testing.T) {
	srv := newTestServer(t)
	body, _ := json.Marshal(map[string]string{"message": "  "})
	req := httptest.NewRequest(http.MethodPost, "/chat", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.handleChat(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// TestHandleView_Markdown /view goldmark 渲染：标题/代码块转 HTML。
func TestHandleView_Markdown(t *testing.T) {
	srv := newTestServer(t)
	p := filepath.Join(srv.cfg.FileRoot, "doc.md")
	if err := os.WriteFile(p, []byte("# 标题\n\n```go\nfmt.Println(1)\n```\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/view?path=doc.md", nil)
	rec := httptest.NewRecorder()
	srv.handleView(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "<h1>标题</h1>") {
		t.Errorf("markdown 标题未渲染:\n%s", rec.Body.String())
	}
}

// TestHandleView_Sensitive 敏感路径拒绝展示。
func TestHandleView_Sensitive(t *testing.T) {
	srv := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/view?path=/etc/passwd", nil)
	rec := httptest.NewRecorder()
	srv.handleView(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "敏感") {
		t.Errorf("敏感路径应拒绝: %d %s", rec.Code, rec.Body.String())
	}
}

// TestResolveViewPath 路径解析：工作区/缓存内放行、工作区外非敏感放行、敏感拒绝。
func TestResolveViewPath(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if _, err := resolveViewPath(root, root, "a.md"); err != nil {
		t.Errorf("工作区内应放行: %v", err)
	}
	if abs, err := resolveViewPath(root, root, outside); err != nil || abs != outside {
		t.Errorf("工作区外非敏感应放行: %v %v", abs, err)
	}
	for _, p := range []string{"/etc/passwd", "/proc/1/status"} {
		if _, err := resolveViewPath(root, root, p); err == nil {
			t.Errorf("敏感路径 %q 应拒绝", p)
		}
	}
	if _, err := resolveViewPath(root, root, ""); err == nil {
		t.Error("空 path 应报错")
	}
}
