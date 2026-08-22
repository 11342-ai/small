// Package retry 提供"建连阶段"的带重试 HTTP 执行器。
//
// 业务约束：
//   - 重试只覆盖建连（发起请求 → 收到响应头），一旦拿到 2xx 响应体即停止重试，
//     避免流式场景下已吐出的 token 被重复发送；
//   - 退避采用有界指数退避 + 全抖动（Full Jitter）：
//     sleep = random(0, min(BaseDelay * 2^(attempt-1), MaxDelay))；
//   - 尊重服务端 Retry-After 头（秒数），且受 MaxDelay 封顶；
//   - 退避等待期间可被 ctx 取消。
package retry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strconv"
	"time"
)

// Policy 定义重试策略。
type Policy struct {
	// MaxAttempts 总尝试次数（含首次请求）。
	MaxAttempts int
	// BaseDelay 指数退避的基数。
	BaseDelay time.Duration
	// MaxDelay 单次退避的上限，任何退避（含 Retry-After）都不超过它。
	MaxDelay time.Duration
	// ShouldRetry 根据 HTTP 状态码决定是否值得重试。
	ShouldRetry func(status int) bool
}

// Default 返回默认策略：4 次尝试、500ms 起退、单次最长 30s。
// 可重试 408/429 以及全部 5xx——529 等 Cloudflare 私有扩展码天然落在 5xx 区间内。
func Default() *Policy {
	return &Policy{
		MaxAttempts: 4,
		BaseDelay:   500 * time.Millisecond,
		MaxDelay:    30 * time.Second,
		ShouldRetry: func(status int) bool {
			return status == http.StatusRequestTimeout || // 408
				status == http.StatusTooManyRequests || // 429
				status >= 500 // 5xx，含 529
		},
	}
}

// backoff 计算第 attempt 次重试（attempt 从 1 开始）的退避时长。
func (p *Policy) backoff(attempt int) time.Duration {
	cap := p.BaseDelay
	for i := 1; i < attempt; i++ {
		cap *= 2
		if cap >= p.MaxDelay {
			cap = p.MaxDelay
			break
		}
	}
	if cap > p.MaxDelay {
		cap = p.MaxDelay
	}
	if cap <= 0 {
		return 0
	}
	return time.Duration(rand.Int63n(int64(cap)))
}

// ExhaustedError 表示重试已耗尽，携带最后一次响应的状态码。
type ExhaustedError struct {
	Status int
}

func (e *ExhaustedError) Error() string {
	return fmt.Sprintf("retry: exhausted, last status %d", e.Status)
}

// Do 在 ctx 控制下执行 fn 并按其返回的响应状态码决定是否重试。
//
// 约定：
//   - fn 返回非 nil error 视为不可重试错误，立即返回（网络/传输错误不重试，
//     重试条件严格限定为 ShouldRetry 判定的状态码）；
//   - 状态码经 ShouldRetry 判定为可重试时，排空并关闭响应体后退避重试；
//   - 其余状态码直接返回响应，由调用方检查是否为 2xx 并负责关闭 Body。
func (p *Policy) Do(ctx context.Context, fn func() (*http.Response, error)) (*http.Response, error) {
	for attempt := 1; attempt <= p.MaxAttempts; attempt++ {
		resp, err := fn()
		if err != nil {
			return nil, err
		}
		if resp == nil {
			return nil, errors.New("retry: fn returned nil response")
		}
		if !p.ShouldRetry(resp.StatusCode) {
			return resp, nil
		}
		if attempt >= p.MaxAttempts {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
			_ = resp.Body.Close()
			return nil, &ExhaustedError{Status: resp.StatusCode}
		}

		// 排空并关闭响应体，便于连接复用。
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()

		delay := p.backoff(attempt)
		if ra := resp.Header.Get("Retry-After"); ra != "" {
			if sec, err := strconv.Atoi(ra); err == nil {
				if d := time.Duration(sec) * time.Second; d > delay {
					delay = d
				}
			}
		}
		if delay > p.MaxDelay {
			delay = p.MaxDelay
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
	}
	return nil, errors.New("retry: unreachable")
}
