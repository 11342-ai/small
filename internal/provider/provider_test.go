package provider_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"small/internal/config"
	"small/internal/provider"
	"small/internal/provider/retry"
)

// noRetry 让测试确定性：任何状态码一律不重试。
var noRetry = &retry.Policy{
	MaxAttempts: 1,
	ShouldRetry: func(int) bool { return false },
}

// newClient 构造一个指向 srv 的测试客户端。
func newClient(t *testing.T, srv *httptest.Server) *provider.Client {
	t.Helper()
	return provider.New(
		&config.Config{Model: "test-model", APIKey: "test-key"},
		provider.WithBaseURL(srv.URL),
		provider.WithHTTPClient(srv.Client()),
		provider.WithRetryPolicy(noRetry),
	)
}

// ---- Client.Complete ----

func TestComplete_Success(t *testing.T) {
	var gotPath, gotAuth string
	var gotPayload struct {
		Model    string             `json:"model"`
		Messages []provider.Message `json:"messages"`
		Stream   bool               `json:"stream"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &gotPayload); err != nil {
			t.Errorf("bad request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"c1","object":"chat.completion","created":1700000000,"model":"test-model","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6}}`)
	}))
	defer srv.Close()

	resp, err := newClient(t, srv).Complete(context.Background(), &provider.ChatRequest{
		Model:    "test-model",
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotPath != "/chat/completions" {
		t.Errorf("path = %q", gotPath)
	}
	if gotAuth != "Bearer test-key" {
		t.Errorf("auth = %q", gotAuth)
	}
	if gotPayload.Stream {
		t.Error("non-stream request must not set stream=true")
	}
	if gotPayload.Model != "test-model" || len(gotPayload.Messages) != 1 {
		t.Errorf("payload = %+v", gotPayload)
	}
	if len(resp.Choices) != 1 || resp.Choices[0].Message.Content != "hello" {
		t.Errorf("resp = %+v", resp)
	}
	if resp.Usage == nil || resp.Usage.TotalTokens != 6 {
		t.Errorf("usage = %+v", resp.Usage)
	}
}

func TestComplete_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"message":"invalid request","type":"invalid_request_error","code":400}}`)
	}))
	defer srv.Close()

	_, err := newClient(t, srv).Complete(context.Background(), &provider.ChatRequest{})
	var apiErr *provider.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want *APIError, got %v", err)
	}
	if apiErr.Status != http.StatusBadRequest || apiErr.Message != "invalid request" {
		t.Errorf("apiErr = %+v", apiErr)
	}
}

func TestComplete_InvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `this is not json`)
	}))
	defer srv.Close()

	if _, err := newClient(t, srv).Complete(context.Background(), &provider.ChatRequest{}); err == nil {
		t.Fatal("want decode error")
	}
}

func TestComplete_RetriesOn5xx(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `oops`)
			return
		}
		fmt.Fprint(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	client := provider.New(
		&config.Config{Model: "test-model", APIKey: "test-key"},
		provider.WithBaseURL(srv.URL),
		provider.WithHTTPClient(srv.Client()),
		provider.WithRetryPolicy(&retry.Policy{
			MaxAttempts: 3,
			BaseDelay:   time.Millisecond,
			MaxDelay:    time.Millisecond,
			ShouldRetry: func(status int) bool { return status >= 500 },
		}),
	)
	resp, err := client.Complete(context.Background(), &provider.ChatRequest{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 2 {
		t.Fatalf("want 2 calls, got %d", calls)
	}
	if resp.Choices[0].Message.Content != "ok" {
		t.Errorf("content = %q", resp.Choices[0].Message.Content)
	}
}

// ---- Client.Stream ----

func TestStream_Success(t *testing.T) {
	chunks := []string{
		`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"test-model","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"think..."},"finish_reason":null}]}`,
		`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"test-model","choices":[{"index":0,"delta":{"content":"hello"},"finish_reason":null}]}`,
		`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"test-model","choices":[{"index":0,"delta":{"content":" world"},"finish_reason":"stop"}]}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
			flusher.Flush()
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer srv.Close()

	var thinking, content strings.Builder
	done := false
	err := newClient(t, srv).Stream(context.Background(), &provider.ChatRequest{}, provider.StreamCallbacks{
		OnThinking: func(s string) error { thinking.WriteString(s); return nil },
		OnContent:  func(s string) error { content.WriteString(s); return nil },
		OnDone:     func() error { done = true; return nil },
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if thinking.String() != "think..." {
		t.Errorf("thinking = %q", thinking.String())
	}
	if content.String() != "hello world" {
		t.Errorf("content = %q", content.String())
	}
	if !done {
		t.Error("OnDone not called")
	}
}

func TestStream_EOFWithoutDone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n")
		flusher.Flush()
		// 不发 [DONE]，直接结束响应。
	}))
	defer srv.Close()

	done := false
	err := newClient(t, srv).Stream(context.Background(), &provider.ChatRequest{}, provider.StreamCallbacks{
		OnContent: func(string) error { return nil },
		OnDone:    func() error { done = true; return nil },
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !done {
		t.Error("OnDone should fire on graceful EOF")
	}
}

func TestStream_InvalidChunk(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		fmt.Fprint(w, "data: {not-json}\n\n")
		flusher.Flush()
	}))
	defer srv.Close()

	err := newClient(t, srv).Stream(context.Background(), &provider.ChatRequest{}, provider.StreamCallbacks{})
	if err == nil || !strings.Contains(err.Error(), "parse stream chunk") {
		t.Fatalf("want parse error, got %v", err)
	}
}

func TestStream_CallbackErrorAborts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n")
		flusher.Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()

	boom := errors.New("boom")
	err := newClient(t, srv).Stream(context.Background(), &provider.ChatRequest{}, provider.StreamCallbacks{
		OnContent: func(string) error { return boom },
	})
	if !errors.Is(err, boom) {
		t.Fatalf("want boom, got %v", err)
	}
}

func TestStream_ContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n")
		flusher.Flush()
		<-r.Context().Done() // 挂起，保证取消先于 EOF 到达客户端
	}))
	defer srv.Close()

	done := make(chan error, 1)
	go func() {
		done <- newClient(t, srv).Stream(ctx, &provider.ChatRequest{}, provider.StreamCallbacks{
			OnContent: func(string) error {
				cancel() // 首次收到内容即取消
				return nil
			},
		})
	}()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

func TestStream_IdleTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n")
		flusher.Flush()
		<-r.Context().Done() // 挂起，不再产生任何数据
	}))
	defer srv.Close()

	client := provider.New(
		&config.Config{Model: "test-model", APIKey: "test-key"},
		provider.WithBaseURL(srv.URL),
		provider.WithHTTPClient(srv.Client()),
		provider.WithRetryPolicy(noRetry),
		provider.WithIdleTimeout(50*time.Millisecond),
	)
	err := client.Stream(context.Background(), &provider.ChatRequest{}, provider.StreamCallbacks{
		OnContent: func(string) error { return nil },
	})
	if !errors.Is(err, provider.ErrStreamIdle) {
		t.Fatalf("want ErrStreamIdle, got %v", err)
	}
}

func TestStream_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":{"message":"bad key"}}`)
	}))
	defer srv.Close()

	err := newClient(t, srv).Stream(context.Background(), &provider.ChatRequest{}, provider.StreamCallbacks{})
	var apiErr *provider.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusUnauthorized {
		t.Fatalf("want *APIError(401), got %v", err)
	}
}

// ---- 工具调用 ----

func TestComplete_WithTools(t *testing.T) {
	var got struct {
		Tools []provider.Tool `json:"tools"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &got); err != nil {
			t.Errorf("bad request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"echo","arguments":"{\"message\":\"hi\"}"}}]},"finish_reason":"tool_calls"}]}`)
	}))
	defer srv.Close()

	resp, err := newClient(t, srv).Complete(context.Background(), &provider.ChatRequest{
		Model:    "m",
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
		Tools: []provider.Tool{{
			Type:     "function",
			Function: provider.ToolFunction{Name: "echo", Description: "echo", Parameters: json.RawMessage(`{"type":"object"}`)},
		}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.Tools) != 1 || got.Tools[0].Function.Name != "echo" {
		t.Errorf("request tools = %+v, want one echo", got.Tools)
	}
	if len(resp.Choices) != 1 || len(resp.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("response tool_calls missing: %+v", resp.Choices)
	}
	tc := resp.Choices[0].Message.ToolCalls[0]
	if tc.ID != "call_1" || tc.Function.Name != "echo" || tc.Function.Arguments != `{"message":"hi"}` {
		t.Errorf("tool call = %+v", tc)
	}
}

func TestStream_ToolCallDeltas(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		for _, c := range []string{
			`{"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"echo","arguments":""}}]}}]}`,
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"message\":"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"hi\"}"}}]}}]}`,
		} {
			fmt.Fprintf(w, "data: %s\n\n", c)
			flusher.Flush()
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer srv.Close()

	var deltas []provider.ToolCallDelta
	err := newClient(t, srv).Stream(context.Background(), &provider.ChatRequest{}, provider.StreamCallbacks{
		OnToolCall: func(d provider.ToolCallDelta) error { deltas = append(deltas, d); return nil },
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(deltas) != 3 {
		t.Fatalf("deltas = %+v, want 3", deltas)
	}
	if deltas[0].Index != 0 || deltas[0].ID != "call_1" || deltas[0].Name != "echo" {
		t.Errorf("first delta = %+v", deltas[0])
	}
	if deltas[1].Arguments != `{"message":` || deltas[2].Arguments != `"hi"}` {
		t.Errorf("argument fragments = %q, %q", deltas[1].Arguments, deltas[2].Arguments)
	}
}
