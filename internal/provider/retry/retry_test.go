package retry

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// newResp 构造一个带可关闭 body 的测试响应。
func newResp(status int) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(`{}`)),
	}
}

func TestDo_SuccessFirstTry(t *testing.T) {
	p := Default()
	calls := 0
	resp, err := p.Do(context.Background(), func() (*http.Response, error) {
		calls++
		return newResp(http.StatusOK), nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	resp.Body.Close()
	if calls != 1 {
		t.Fatalf("want 1 call, got %d", calls)
	}
}

func TestDo_RetryThenSuccess(t *testing.T) {
	p := Default()
	p.BaseDelay = time.Millisecond
	p.MaxDelay = 2 * time.Millisecond

	calls := 0
	resp, err := p.Do(context.Background(), func() (*http.Response, error) {
		calls++
		if calls == 1 {
			return newResp(http.StatusInternalServerError), nil
		}
		return newResp(http.StatusOK), nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	resp.Body.Close()
	if calls != 2 {
		t.Fatalf("want 2 calls, got %d", calls)
	}
}

func TestDo_NonRetryable(t *testing.T) {
	p := Default()
	calls := 0
	resp, err := p.Do(context.Background(), func() (*http.Response, error) {
		calls++
		return newResp(http.StatusBadRequest), nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	resp.Body.Close()
	if calls != 1 {
		t.Fatalf("want 1 call, got %d", calls)
	}
}

func TestDo_TransportErrorNotRetried(t *testing.T) {
	p := Default()
	sentinel := errors.New("dial tcp: connection refused")
	calls := 0
	_, err := p.Do(context.Background(), func() (*http.Response, error) {
		calls++
		return nil, sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("want sentinel error, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("want 1 call, got %d", calls)
	}
}

func TestDo_ExhaustedReturnsStatus(t *testing.T) {
	p := Default()
	p.BaseDelay = time.Millisecond
	p.MaxDelay = 2 * time.Millisecond

	calls := 0
	_, err := p.Do(context.Background(), func() (*http.Response, error) {
		calls++
		return newResp(http.StatusServiceUnavailable), nil
	})
	var ee *ExhaustedError
	if !errors.As(err, &ee) {
		t.Fatalf("want ExhaustedError, got %v", err)
	}
	if ee.Status != http.StatusServiceUnavailable {
		t.Fatalf("want status 503, got %d", ee.Status)
	}
	if calls != p.MaxAttempts {
		t.Fatalf("want %d calls, got %d", p.MaxAttempts, calls)
	}
}

func TestDo_RetryAfterCappedByMaxDelay(t *testing.T) {
	p := Default()
	p.BaseDelay = time.Millisecond
	p.MaxDelay = 5 * time.Millisecond

	start := time.Now()
	calls := 0
	_, err := p.Do(context.Background(), func() (*http.Response, error) {
		calls++
		resp := newResp(http.StatusTooManyRequests)
		resp.Header.Set("Retry-After", "3600") // 1 小时
		return resp, nil
	})
	if err == nil {
		t.Fatal("want error")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Retry-After not capped by MaxDelay, took %v", elapsed)
	}
	if calls != p.MaxAttempts {
		t.Fatalf("want %d calls, got %d", p.MaxAttempts, calls)
	}
}

func TestDo_CancelledDuringBackoff(t *testing.T) {
	p := Default()
	p.BaseDelay = time.Second
	p.MaxDelay = time.Second

	// 退避上限 1s 远大于 30ms 超时：无论全抖动取到什么值，ctx 都会先到期。
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	_, err := p.Do(ctx, func() (*http.Response, error) {
		return newResp(http.StatusInternalServerError), nil
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want context.DeadlineExceeded, got %v", err)
	}
}

func TestBackoff_Bounds(t *testing.T) {
	p := Default()
	p.BaseDelay = 10 * time.Millisecond
	p.MaxDelay = 50 * time.Millisecond

	for attempt := 1; attempt <= 6; attempt++ {
		upper := p.BaseDelay
		for i := 1; i < attempt; i++ {
			upper *= 2
		}
		if upper > p.MaxDelay {
			upper = p.MaxDelay
		}
		for i := 0; i < 200; i++ {
			if d := p.backoff(attempt); d < 0 || d > upper {
				t.Fatalf("attempt %d: backoff %v out of [0, %v]", attempt, d, upper)
			}
		}
	}
}
