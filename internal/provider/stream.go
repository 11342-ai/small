package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"
)

// ErrStreamIdle 流式读取超过空闲时限被中断。
var ErrStreamIdle = errors.New("provider: stream idle timeout")

// streamChunk 对应流式响应中的单个 chunk（data: {...}），
// 字段与 DeepSeek 兼容接口的 chat.completion.chunk 一致。
type streamChunk struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Role             string `json:"role,omitempty"`
			Content          string `json:"content,omitempty"`
			ReasoningContent string `json:"reasoning_content,omitempty"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id,omitempty"`
				Type     string `json:"type,omitempty"`
				Function struct {
					Name      string `json:"name,omitempty"`
					Arguments string `json:"arguments,omitempty"`
				} `json:"function,omitempty"`
			} `json:"tool_calls,omitempty"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason,omitempty"`
	} `json:"choices"`
}

const (
	dataPrefix = "data:"
	doneMarker = "[DONE]"
	// maxStreamLine 单条 SSE 行的读取上限，防御异常超大 chunk。
	maxStreamLine = 1 << 20
)

// Stream 实现 Streamer。
//
// 重试策略：retry.Do 只覆盖"建连"（build → send → 状态检查）。
// 拿到 2xx 响应体进入流中之后绝不再重试，否则会重复已吐出的 token。
//
// 超时策略：刻意使用 Timeout=0 的 http.Client 克隆（去掉整请求超时，
// 否则几分钟的长生成会被误杀），改由两层兜底：
//  1. idleTimeout：每次成功读取后重置的"读空闲超时"；
//  2. ctx：streamCtx 继承调用方 ctx，外部取消仍可立即中断。
func (c *Client) Stream(ctx context.Context, req *ChatRequest, cbs StreamCallbacks) error {
	body, err := json.Marshal(chatPayload{ChatRequest: *req, Stream: true})
	if err != nil {
		return fmt.Errorf("provider: marshal request: %w", err)
	}

	// 流式专用 client：去掉整请求超时（见上方注释），保留其余字段。
	streamClient := cloneClientWithoutTimeout(c.httpClient)

	// 流式专用 ctx：贯穿建连与流中，请求必须绑定它——
	// 这样空闲超时/外部取消才能中断 transport 的读，而非只改一个没人听的 context。
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	resp, err := c.retryPolicy.Do(streamCtx, func() (*http.Response, error) {
		httpReq, err := http.NewRequestWithContext(streamCtx, http.MethodPost, c.baseURL+chatCompletionsPath, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		c.setHeaders(httpReq)
		return streamClient.Do(httpReq)
	})
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return parseAPIError(resp)
	}

	// ---- 进入流中：不再有任何重试 ----
	// 空闲超时：每次读取数据后重置计时器，触发则取消整个流上下文。
	var idleExpired atomic.Bool
	idleTimer := time.AfterFunc(c.idleTimeout, func() {
		idleExpired.Store(true)
		cancel()
	})
	defer idleTimer.Stop()

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64<<10), maxStreamLine)

	finished := false
	for scanner.Scan() {
		line := scanner.Bytes()
		idleTimer.Reset(c.idleTimeout)
		if len(line) == 0 {
			continue
		}
		// SSE 事件行以 "data:" 开头；忽略注释行（":" 开头）等其余内容。
		if !bytes.HasPrefix(line, []byte(dataPrefix)) {
			continue
		}
		payload := bytes.TrimSpace(line[len(dataPrefix):])
		if bytes.Equal(payload, []byte(doneMarker)) {
			finished = true
			if cbs.OnDone != nil {
				if err := cbs.OnDone(); err != nil {
					return fmt.Errorf("provider: OnDone: %w", err)
				}
			}
			break
		}

		var chunk streamChunk
		if err := json.Unmarshal(payload, &chunk); err != nil {
			return fmt.Errorf("provider: parse stream chunk: %w", err)
		}
		for _, choice := range chunk.Choices {
			if d := choice.Delta.ReasoningContent; d != "" && cbs.OnThinking != nil {
				if err := cbs.OnThinking(d); err != nil {
					return fmt.Errorf("provider: OnThinking: %w", err)
				}
			}
			if d := choice.Delta.Content; d != "" && cbs.OnContent != nil {
				if err := cbs.OnContent(d); err != nil {
					return fmt.Errorf("provider: OnContent: %w", err)
				}
			}
			for _, tc := range choice.Delta.ToolCalls {
				if cbs.OnToolCall == nil {
					continue
				}
				if err := cbs.OnToolCall(ToolCallDelta{
					Index:     tc.Index,
					ID:        tc.ID,
					Name:      tc.Function.Name,
					Arguments: tc.Function.Arguments,
				}); err != nil {
					return fmt.Errorf("provider: OnToolCall: %w", err)
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		if idleExpired.Load() {
			return ErrStreamIdle
		}
		if streamCtx.Err() != nil {
			// 外部 ctx 取消导致的读中断。
			return streamCtx.Err()
		}
		return fmt.Errorf("provider: read stream: %w", err)
	}

	// 服务端优雅关闭（EOF）但未收到 [DONE]：视为正常结束。
	if !finished && cbs.OnDone != nil {
		if err := cbs.OnDone(); err != nil {
			return fmt.Errorf("provider: OnDone: %w", err)
		}
	}
	return nil
}
