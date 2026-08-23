package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"small/internal/config"
	"small/internal/provider"
	"small/internal/provider/retry"
	"small/internal/tool"
	"small/internal/tool/builtin"
)

// noRetry 保证测试确定性：任何状态码一律不重试。
var noRetry = &retry.Policy{
	MaxAttempts: 1,
	ShouldRetry: func(int) bool { return false },
}

// completerOnly 只实现 provider.Completer（无 Stream），用于验证降级路径。
type completerOnly struct {
	fn func(ctx context.Context, req *provider.ChatRequest) (*provider.ChatResponse, error)
}

func (c *completerOnly) Complete(ctx context.Context, req *provider.ChatRequest) (*provider.ChatResponse, error) {
	return c.fn(ctx, req)
}

// realStreamClient 构造走真实 HTTP 的 provider.Client（满足 Streamer）。
func realStreamClient(t *testing.T, srv *httptest.Server) *provider.Client {
	t.Helper()
	return provider.New(
		&config.Config{Model: "m1", APIKey: "test-key"},
		provider.WithBaseURL(srv.URL),
		provider.WithHTTPClient(srv.Client()),
		provider.WithRetryPolicy(noRetry),
	)
}

func TestProviderChat_StreamsWhenSupported(t *testing.T) {
	var got struct {
		Model    string             `json:"model"`
		Messages []provider.Message `json:"messages"`
		Stream   bool               `json:"stream"`
		Thinking *provider.Thinking `json:"thinking,omitempty"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &got); err != nil {
			t.Errorf("bad request body: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		for _, c := range []string{
			`{"choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"hidden"}}]}`,
			`{"choices":[{"index":0,"delta":{"content":"hello"}}]}`,
			`{"choices":[{"index":0,"delta":{"content":" world"}}]}`,
		} {
			io.WriteString(w, "data: "+c+"\n\n")
			flusher.Flush()
		}
		io.WriteString(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer srv.Close()

	adapter := NewProviderChat(realStreamClient(t, srv), "m1", nil)
	result, err := adapter.Complete(context.Background(), []Turn{
		{Role: "system", Content: "s"},
		{Role: "user", Content: "hi"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Reply != "hello world" {
		t.Errorf("reply = %q, want %q", result.Reply, "hello world")
	}
	if result.Thinking != "hidden" {
		t.Errorf("thinking = %q, want %q", result.Thinking, "hidden")
	}
	if !got.Stream {
		t.Error("stream should be true when client supports it")
	}
	if got.Model != "m1" {
		t.Errorf("model = %q", got.Model)
	}
	if got.Thinking != nil {
		t.Error("thinking switch is off by default, request should not carry it")
	}
	want := []provider.Message{{Role: "system", Content: "s"}, {Role: "user", Content: "hi"}}
	if !reflect.DeepEqual(got.Messages, want) {
		t.Errorf("messages = %+v, want %+v", got.Messages, want)
	}
}

func TestProviderChat_ThinkingSwitch(t *testing.T) {
	var got struct {
		Thinking *provider.Thinking `json:"thinking,omitempty"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		io.WriteString(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer srv.Close()

	adapter := NewProviderChat(realStreamClient(t, srv), "m1", nil, WithThinking(true))
	if _, err := adapter.Complete(context.Background(), []Turn{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Thinking == nil || got.Thinking.Type != "enabled" {
		t.Errorf("thinking = %+v, want enabled", got.Thinking)
	}
}

func TestProviderChat_ThinkingSwitchNonStream(t *testing.T) {
	// WithThinking(true) 在非流式路径同样生效：请求体应带 thinking。
	var got *provider.ChatRequest
	mock := &completerOnly{fn: func(_ context.Context, req *provider.ChatRequest) (*provider.ChatResponse, error) {
		got = req
		return &provider.ChatResponse{Choices: []provider.Choice{{Message: provider.Message{Content: "ok"}}}}, nil
	}}

	adapter := NewProviderChat(mock, "m1", nil, WithThinking(true))
	if _, err := adapter.Complete(context.Background(), []Turn{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Thinking == nil || got.Thinking.Type != "enabled" {
		t.Errorf("thinking = %+v, want enabled", got.Thinking)
	}
}

func TestProviderChat_StreamOnlyThinking(t *testing.T) {
	// 只返回思考内容、无 content 时，Reply 为空串，Thinking 保留。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		io.WriteString(w, `data: {"choices":[{"index":0,"delta":{"reasoning_content":"only thinking"}}]}`+"\n\n")
		flusher.Flush()
		io.WriteString(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer srv.Close()

	adapter := NewProviderChat(realStreamClient(t, srv), "m1", nil)
	result, err := adapter.Complete(context.Background(), []Turn{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Reply != "" {
		t.Errorf("reply = %q, want empty", result.Reply)
	}
	if result.Thinking != "only thinking" {
		t.Errorf("thinking = %q, want %q", result.Thinking, "only thinking")
	}
}

func TestProviderChat_FallsBackToNonStream(t *testing.T) {
	called := false
	mock := &completerOnly{fn: func(context.Context, *provider.ChatRequest) (*provider.ChatResponse, error) {
		called = true
		return &provider.ChatResponse{Choices: []provider.Choice{{Message: provider.Message{Content: "plain"}}}}, nil
	}}

	adapter := NewProviderChat(mock, "m1", nil)
	result, err := adapter.Complete(context.Background(), []Turn{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called {
		t.Error("non-stream path should be used for completer-only client")
	}
	if result.Reply != "plain" {
		t.Errorf("reply = %q, want %q", result.Reply, "plain")
	}
}

func TestProviderChat_NonStreamThinking(t *testing.T) {
	// 非流式路径也应透传 reasoning_content。
	mock := &completerOnly{fn: func(context.Context, *provider.ChatRequest) (*provider.ChatResponse, error) {
		return &provider.ChatResponse{Choices: []provider.Choice{{
			Message: provider.Message{Content: "a", ReasoningContent: "r"},
		}}}, nil
	}}

	adapter := NewProviderChat(mock, "m1", nil)
	result, err := adapter.Complete(context.Background(), []Turn{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Reply != "a" || result.Thinking != "r" {
		t.Errorf("result = %+v, want {Reply:a Thinking:r}", result)
	}
}

func TestProviderChat_ConvertsTurns(t *testing.T) {
	var got *provider.ChatRequest
	mock := &completerOnly{fn: func(_ context.Context, req *provider.ChatRequest) (*provider.ChatResponse, error) {
		got = req
		return &provider.ChatResponse{Choices: []provider.Choice{{Message: provider.Message{Content: "ok"}}}}, nil
	}}

	adapter := NewProviderChat(mock, "m1", nil)
	if _, err := adapter.Complete(context.Background(), []Turn{
		{Role: "system", Content: "s"},
		{Role: "user", Content: "hi"},
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Model != "m1" {
		t.Errorf("model = %q", got.Model)
	}
	want := []provider.Message{{Role: "system", Content: "s"}, {Role: "user", Content: "hi"}}
	if !reflect.DeepEqual(got.Messages, want) {
		t.Errorf("messages = %+v, want %+v", got.Messages, want)
	}
}

func TestProviderChat_EmptyTurns(t *testing.T) {
	calls := 0
	mock := &completerOnly{fn: func(context.Context, *provider.ChatRequest) (*provider.ChatResponse, error) {
		calls++
		return nil, nil
	}}

	adapter := NewProviderChat(mock, "m1", nil)
	// nil 与空切片都应拒绝，且不应调用后端。
	if _, err := adapter.Complete(context.Background(), nil); err == nil {
		t.Error("want error for nil turns")
	}
	if _, err := adapter.Complete(context.Background(), []Turn{}); err == nil {
		t.Error("want error for empty turns")
	}
	if calls != 0 {
		t.Errorf("backend should not be called, got %d calls", calls)
	}
}

func TestProviderChat_EmptyChoices(t *testing.T) {
	mock := &completerOnly{fn: func(context.Context, *provider.ChatRequest) (*provider.ChatResponse, error) {
		return &provider.ChatResponse{Choices: nil}, nil
	}}

	adapter := NewProviderChat(mock, "m1", nil)
	if _, err := adapter.Complete(context.Background(), []Turn{{Role: "user", Content: "hi"}}); err == nil {
		t.Error("want error for empty choices")
	}
}

func TestProviderChat_PropagatesError(t *testing.T) {
	sentinel := errors.New("upstream down")
	mock := &completerOnly{fn: func(context.Context, *provider.ChatRequest) (*provider.ChatResponse, error) {
		return nil, sentinel
	}}

	adapter := NewProviderChat(mock, "m1", nil)
	if _, err := adapter.Complete(context.Background(), []Turn{{Role: "user", Content: "hi"}}); !errors.Is(err, sentinel) {
		t.Fatalf("want sentinel error, got %v", err)
	}
}

func TestProviderChat_NoFallbackAfterStreamError(t *testing.T) {
	// 流中出错（非法 chunk）必须透传错误，绝不能回退到非流式再发一次请求。
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		io.WriteString(w, "data: {not-json}\n\n")
		flusher.Flush()
	}))
	defer srv.Close()

	adapter := NewProviderChat(realStreamClient(t, srv), "m1", nil)
	_, err := adapter.Complete(context.Background(), []Turn{{Role: "user", Content: "hi"}})
	if err == nil || !strings.Contains(err.Error(), "parse stream chunk") {
		t.Fatalf("want parse error, got %v", err)
	}
	if calls != 1 {
		t.Errorf("want exactly 1 request, got %d (must not fall back to non-stream)", calls)
	}
}

// ---- 工具调用 ----

func TestProviderChat_SendsToolDefinitions(t *testing.T) {
	var got struct {
		Tools []provider.Tool `json:"tools"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &got); err != nil {
			t.Errorf("bad request body: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		io.WriteString(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer srv.Close()

	reg := tool.New()
	if err := reg.Register(builtin.Echo()); err != nil {
		t.Fatalf("register: %v", err)
	}
	adapter := NewProviderChat(realStreamClient(t, srv), "m1", reg.List())
	if _, err := adapter.Complete(context.Background(), []Turn{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.Tools) != 1 || got.Tools[0].Function.Name != "echo" {
		t.Fatalf("request tools = %+v, want one echo", got.Tools)
	}
}

func TestProviderChat_NoToolDefinitionsWithoutRegistry(t *testing.T) {
	var got struct {
		Tools []provider.Tool `json:"tools"`
	}
	mock := &completerOnly{fn: func(_ context.Context, req *provider.ChatRequest) (*provider.ChatResponse, error) {
		raw, _ := json.Marshal(req)
		_ = json.Unmarshal(raw, &got)
		return &provider.ChatResponse{Choices: []provider.Choice{{Message: provider.Message{Content: "ok"}}}}, nil
	}}

	adapter := NewProviderChat(mock, "m1", nil)
	if _, err := adapter.Complete(context.Background(), []Turn{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.Tools) != 0 {
		t.Fatalf("tools = %+v, want none without registry", got.Tools)
	}
}

func TestProviderChat_StreamToolCalls(t *testing.T) {
	// 流式 tool_calls：ID/Name 只在首个分片，Arguments 分片需拼接还原。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		for _, c := range []string{
			`{"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"echo","arguments":""}}]}}]}`,
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"mes"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"sage\":\"hi\"}"}}]}}]}`,
		} {
			io.WriteString(w, "data: "+c+"\n\n")
			flusher.Flush()
		}
		io.WriteString(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer srv.Close()

	adapter := NewProviderChat(realStreamClient(t, srv), "m1", nil)
	result, err := adapter.Complete(context.Background(), []Turn{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.ToolCalls) != 1 {
		t.Fatalf("ToolCalls = %+v, want 1", result.ToolCalls)
	}
	c := result.ToolCalls[0]
	if c.ID != "call_1" || c.Name != "echo" || c.Args != `{"message":"hi"}` {
		t.Fatalf("ToolCall = %+v, want call_1/echo/{\"message\":\"hi\"}", c)
	}
}

func TestProviderChat_StreamToolCallsMultipleIndices(t *testing.T) {
	// 两个并行调用（index 0/1）分片交错：按 index 独立拼接，顺序稳定。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		for _, c := range []string{
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"a","type":"function","function":{"name":"one","arguments":"{\"x\":"}},{"index":1,"id":"b","type":"function","function":{"name":"two","arguments":"{\"y\":"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"2}"}},{"index":0,"function":{"arguments":"1}"}}]}}]}`,
		} {
			io.WriteString(w, "data: "+c+"\n\n")
			flusher.Flush()
		}
		io.WriteString(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer srv.Close()

	adapter := NewProviderChat(realStreamClient(t, srv), "m1", nil)
	result, err := adapter.Complete(context.Background(), []Turn{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.ToolCalls) != 2 {
		t.Fatalf("ToolCalls = %+v, want 2", result.ToolCalls)
	}
	if got := result.ToolCalls[0]; got.ID != "a" || got.Name != "one" || got.Args != `{"x":1}` {
		t.Errorf("ToolCalls[0] = %+v", got)
	}
	if got := result.ToolCalls[1]; got.ID != "b" || got.Name != "two" || got.Args != `{"y":2}` {
		t.Errorf("ToolCalls[1] = %+v", got)
	}
}

func TestProviderChat_NonStreamToolCalls(t *testing.T) {
	adapter := NewProviderChat(&completerOnly{
		fn: func(context.Context, *provider.ChatRequest) (*provider.ChatResponse, error) {
			return &provider.ChatResponse{Choices: []provider.Choice{{
				Message: provider.Message{
					ToolCalls: []provider.ToolCall{{
						ID: "call_9", Type: "function",
						Function: provider.ToolCallFunction{Name: "echo", Arguments: `{"message":"x"}`},
					}},
				},
			}}}, nil
		},
	}, "m1", nil)

	result, err := adapter.Complete(context.Background(), []Turn{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.ToolCalls) != 1 {
		t.Fatalf("ToolCalls = %+v, want 1", result.ToolCalls)
	}
	c := result.ToolCalls[0]
	if c.ID != "call_9" || c.Name != "echo" || c.Args != `{"message":"x"}` {
		t.Fatalf("ToolCall = %+v", c)
	}
}

func TestProviderChat_ToolMessagesRoundTrip(t *testing.T) {
	// 历史中的 assistant(带调用) 与 tool 消息要原样翻译进请求（隔离点职责）。
	var got []provider.Message
	mock := &completerOnly{fn: func(_ context.Context, req *provider.ChatRequest) (*provider.ChatResponse, error) {
		got = req.Messages
		return &provider.ChatResponse{Choices: []provider.Choice{{Message: provider.Message{Content: "done"}}}}, nil
	}}

	adapter := NewProviderChat(mock, "m1", nil)
	if _, err := adapter.Complete(context.Background(), []Turn{
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Name: "echo", Args: `{"message":"hi"}`}}},
		{Role: "tool", ToolCallID: "c1", Content: "hi"},
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("messages = %+v, want 2", got)
	}
	if len(got[0].ToolCalls) != 1 || got[0].ToolCalls[0].ID != "c1" || got[0].ToolCalls[0].Function.Arguments != `{"message":"hi"}` {
		t.Errorf("assistant message = %+v", got[0])
	}
	if got[1].Role != "tool" || got[1].ToolCallID != "c1" || got[1].Content != "hi" {
		t.Errorf("tool message = %+v", got[1])
	}
}
