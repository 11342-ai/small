package k8s

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
)

// timeoutErr 满足 net.Error 且 Timeout()=true 的桩：模拟拨号超时（结构化判据那条路径）。
type timeoutErr struct{}

func (timeoutErr) Error() string   { return "dial tcp 10.0.0.1:6443: i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

// TestExplainError 运行期失败归因：连接类给结论短语，业务类原样回灌（不吞细节）。
func TestExplainError(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		wantSub   string // 期望包含；留空表示期望原样返回
		unchanged bool
	}{
		{"nil", nil, "", true},
		{"连接被拒（client-go 原文）", errors.New("dial tcp 192.168.49.2:8443: connect: connection refused"), "集群不可达（connection refused）", false},
		{"连接被拒（被 fmt.Errorf 包过一层）", fmt.Errorf("get pod default/web-0: %w",
			errors.New("dial tcp 192.168.49.2:8443: connect: connection refused")), "集群不可达", false},
		{"拨号超时（原文含 dial tcp 与 i/o timeout）", errors.New("dial tcp 10.0.0.1:6443: i/o timeout"), "调用超时（i/o timeout）", false},
		{"超时（net.Error 结构化判据）", timeoutErr{}, "调用超时", false},
		{"ctx 超时", context.DeadlineExceeded, "调用超时（context deadline exceeded）", false},
		{"ctx 超时（被包过）", fmt.Errorf("list events: %w", context.DeadlineExceeded), "调用超时", false},
		{"DNS 解析失败", &net.DNSError{Err: "no such host", Name: "api.cluster.local"}, "集群不可达（no such host）", false},
		{"路由不可达", errors.New("dial tcp 10.1.2.3:6443: connect: no route to host"), "集群不可达（no route to host）", false},
		// 业务错误原样回灌：原文已经说清了，包一层反而丢细节。
		{"对象不存在（业务错误）", errors.New(`pods "web-0" not found`), "", true},
		{"权限不足（业务错误）", errors.New(`pods "web-0" is forbidden: User "u" cannot get resource "pods"`), "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ExplainError(tc.err)
			if tc.unchanged {
				want := ""
				if tc.err != nil {
					want = tc.err.Error()
				}
				if got != want {
					t.Errorf("非连接类失败应原样返回:\n got=%q\nwant=%q", got, want)
				}
				return
			}
			if !strings.Contains(got, tc.wantSub) {
				t.Errorf("文案缺结论 %q，实得 %q", tc.wantSub, got)
			}
		})
	}
}

// TestExplainError_Shape 短语形态：只给结论（含分类依据），不带建议、不带句号——
// 建议语由调用方按受众各拼一次（§17.3），这样同一句短语能同时进工具回灌与证据包 notes。
func TestExplainError_Shape(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"连接被拒", errors.New("dial tcp 10.0.0.1:6443: connect: connection refused"), "集群不可达（connection refused）"},
		{"ctx 超时", context.DeadlineExceeded, "调用超时（context deadline exceeded）"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExplainError(tc.err); got != tc.want {
				t.Errorf("文案 = %q，期望 %q（短语形态：无建议、无句号）", got, tc.want)
			}
		})
	}
}

// TestRedactCredentials 凭据脱敏：URL 里的 userinfo 一律抹掉（net/http 只掩码 password，用户名会露出来），
// 普通文本里的 @ 不许误伤。
func TestRedactCredentials(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"无凭据的 URL 原样", `Get "https://10.0.0.1:6443/version": dial tcp`, `Get "https://10.0.0.1:6443/version": dial tcp`},
		{"user:pass 抹成 ***", `Get "https://user:pass@api.internal:6443/version": EOF`, `Get "https://***@api.internal:6443/version": EOF`},
		{"只有 user 也抹", `https://user@api.internal/x`, `https://***@api.internal/x`},
		{"带路径的 URL", `https://u:p@gw.corp/k8s/prod/health`, `https://***@gw.corp/k8s/prod/health`},
		{"非 URL 的 @ 不误伤", `pods "web@0" not found`, `pods "web@0" not found`},
		{"空串", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RedactCredentials(tc.in); got != tc.want {
				t.Errorf("RedactCredentials(%q) = %q，期望 %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestExplainError_RedactsCredentials 原文分支也要脱敏：错误文本里可能带回整个 URL（含 userinfo）。
func TestExplainError_RedactsCredentials(t *testing.T) {
	err := errors.New(`unexpected response from https://user:pass@api.internal:6443/health`)
	got := ExplainError(err)
	if strings.Contains(got, "user:pass") || !strings.Contains(got, "https://***@api.internal:6443") {
		t.Errorf("原文分支未脱敏: %q", got)
	}
}

// TestIsConnFailure 要不要补用户向建议：只有连接类为真；业务错误与 nil 为假
// （否则"Pod 不存在"也会被建议去确认集群可达，给出走不通的下一步）。
func TestIsConnFailure(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"连接被拒", errors.New("dial tcp 10.0.0.1:6443: connect: connection refused"), true},
		{"ctx 超时", context.DeadlineExceeded, true},
		{"DNS 解析失败", &net.DNSError{Err: "no such host", Name: "api.cluster.local"}, true},
		{"对象不存在（业务错误）", errors.New(`pods "web-0" not found`), false},
		{"权限不足（业务错误）", errors.New(`pods "web-0" is forbidden`), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsConnFailure(tc.err); got != tc.want {
				t.Errorf("IsConnFailure = %v，期望 %v", got, tc.want)
			}
		})
	}
}
