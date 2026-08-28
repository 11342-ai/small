package builtin

import (
	"context"
	"encoding/json"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"small/internal/tool"
)

// webFetchTimeout 抓取超时（tool-extend.md 边界②：先接受阻塞，超时兜底）。
const webFetchTimeout = 15 * time.Second

// webFetchSizeLimit 响应体读取上限（字节）：防大页面拖垮上下文。
const webFetchSizeLimit = 200 << 10

// scriptStyleRe 移除 script/style 块内容（不区分大小写、跨行；RE2 不支持反向引用，故拆两段）。
var scriptStyleRe = regexp.MustCompile(`(?is)<(script|style)[^>]*>.*?</(script|style)>`)

// htmlTagRe 剥离剩余 HTML 标签（最小版：不引 x/net/html 依赖，够查在线文档用）。
var htmlTagRe = regexp.MustCompile(`(?s)<[^>]*>`)

// WebFetch 抓取网页转纯文本（只支持 http/https；超时 + 大小上限 + 标签剥离）。
func WebFetch() tool.Tool {
	return tool.NewFunc(
		tool.Spec{
			Name:        "web_fetch",
			Description: "抓取指定 http/https URL 的网页并转为纯文本（超时 15 秒，超长截断，剥离 HTML 标签——复杂页面排版可能失真）。当用户提供链接并希望了解其内容、或需要在线文档/README/API 参考的原文时使用——模型无法凭记忆获取实时网页内容。获取后如用户要求记住，可用 memory_save 存档要点。",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"url": {"type": "string", "description": "完整的 http/https URL"}
				},
				"required": ["url"]
			}`),
		},
		func(ctx context.Context, args json.RawMessage) (tool.Result, error) {
			return runWebFetch(ctx, args)
		},
	)
}

// runWebFetch 网页抓取执行逻辑（外置具名函数，可独立单测）。
func runWebFetch(ctx context.Context, args json.RawMessage) (tool.Result, error) {
	var in struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return tool.Result{Data: "参数错误: " + err.Error(), IsError: true}, nil
	}
	raw := strings.TrimSpace(in.URL)
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return tool.Result{Data: "参数错误: 仅支持 http/https URL", IsError: true}, nil
	}

	// 超时：配置固定 15s（tool-extend.md 边界② 现状：接受阻塞）。
	runCtx, cancel := context.WithTimeout(ctx, webFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(runCtx, http.MethodGet, u.String(), nil)
	if err != nil {
		return tool.Result{Data: "请求构造失败: " + err.Error(), IsError: true}, nil
	}
	req.Header.Set("User-Agent", "small-agent/0.1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		if runCtx.Err() == context.DeadlineExceeded {
			return tool.Result{Data: "web_fetch 超时（超过 " + webFetchTimeout.String() + "）", IsError: true}, nil
		}
		return tool.Result{Data: "抓取失败: " + err.Error(), IsError: true}, nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return tool.Result{Data: "抓取失败: HTTP " + resp.Status, IsError: true}, nil
	}

	// 大小上限 + 标签剥离 + 实体解码 + 压缩空白。
	body, err := io.ReadAll(io.LimitReader(resp.Body, webFetchSizeLimit))
	if err != nil {
		return tool.Result{Data: "读取失败: " + err.Error(), IsError: true}, nil
	}
	text := scriptStyleRe.ReplaceAllString(string(body), " ")
	text = htmlTagRe.ReplaceAllString(text, " ")
	text = html.UnescapeString(text)
	text = collapseSpace(text)
	return tool.Result{Data: truncateOutput(strings.TrimSpace(text))}, nil
}

// collapseSpace 把连续空白压缩为单个空格（标签剥离后常见）。
func collapseSpace(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	space := false
	for _, r := range s {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			if !space {
				b.WriteByte(' ')
				space = true
			}
			continue
		}
		b.WriteRune(r)
		space = false
	}
	return b.String()
}
