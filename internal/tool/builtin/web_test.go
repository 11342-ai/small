package builtin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func webArgs(url string) json.RawMessage {
	in, _ := json.Marshal(map[string]any{"url": url})
	return in
}

func TestRunWebFetch_BadArgs(t *testing.T) {
	cases := []struct {
		name string
		args json.RawMessage
	}{
		{"非法 JSON", json.RawMessage(`{`)},
		{"非 http 协议", webArgs("file:///etc/passwd")},
		{"缺 scheme", webArgs("example.com")},
	}
	for _, c := range cases {
		res, err := runWebFetch(context.Background(), c.args)
		if err != nil || !res.IsError {
			t.Fatalf("%s: 应业务失败，got res=%+v err=%v", c.name, res, err)
		}
	}
}

func TestRunWebFetch_RealFetch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><head><style>.x{}</style><title>Docs</title></head>" +
			"<body><h1>标题</h1><p>这是 &amp; 正文内容。</p><script>alert(1)</script></body></html>"))
	}))
	defer srv.Close()

	res, err := runWebFetch(context.Background(), webArgs(srv.URL))
	if err != nil || res.IsError {
		t.Fatalf("抓取失败：res=%+v err=%v", res, err)
	}
	// 标签剥离 + script 移除 + 实体解码 + 空白压缩。
	for _, want := range []string{"标题", "这是 & 正文内容。"} {
		if !strings.Contains(res.Data, want) {
			t.Fatalf("应含 %q，got %q", want, res.Data)
		}
	}
	if strings.Contains(res.Data, "alert") || strings.Contains(res.Data, "<p>") {
		t.Fatalf("script/标签应被剥离，got %q", res.Data)
	}
}

func TestRunWebFetch_Non200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer srv.Close()

	res, err := runWebFetch(context.Background(), webArgs(srv.URL))
	if err != nil || !res.IsError || !strings.Contains(res.Data, "404") {
		t.Fatalf("非 200 应失败，got res=%+v err=%v", res, err)
	}
}
