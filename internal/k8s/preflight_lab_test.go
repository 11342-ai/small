//go:build k8slab

package k8s

// 真集群探测冒烟：Preflight 的传输语义（拨号、鉴权、discovery 的 /version 与 /apis 往返）
// 只有真 apiserver 能验，fake 客户端不认这些（对齐 lab_test.go 的取舍）。
//
// 跑法：go test -tags k8slab ./internal/k8s/ -run TestPreflightAgainstCluster -count=1 -v
// 集群不可达时 Skip 而不是失败——没起集群的人跑这个 tag 不该被卡住。

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestPreflightAgainstCluster 真集群探测：必须有版本与 context；指标能力按集群实情分别对待——
// 开了 metrics-server 要判"明确可用"（探测链路真的能判可用），没开就 Skip 该断言（环境缺件，
// 不是代码错），而"未探测到"要失败（探测本身出问题，说明 discovery 路径有毛病）。
func TestPreflightAgainstCluster(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("跳过：取不到 home 目录（%v）", err)
	}
	c, err := New(Config{KubeConfig: filepath.Join(home, ".kube", "config"), Dir: t.TempDir()})
	if err != nil {
		t.Skipf("跳过：连不上集群（%v）", err)
	}
	caps, err := c.Preflight(context.Background())
	if err != nil {
		// 集群没起时 Skip（对齐 lab_test.go 的约定：环境缺件不该算代码错）；只有"探测报错
		// 但普通调用能通"才说明探测本身有毛病——那种情况必须失败，否则这个测试就白设了。
		rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, perr := c.core.CoreV1().Namespaces().List(rctx, metav1.ListOptions{Limit: 1}); perr != nil {
			t.Skipf("跳过：集群不可达（%v）", err)
		}
		t.Fatalf("探测失败但普通调用能通，说明 Preflight 本身有问题: %v", err)
	}
	if caps.ServerVersion == "" {
		t.Error("真集群应探测到 apiserver 版本")
	}
	if caps.Context == "" {
		t.Error("真集群应归一化出 context 名（kubeconfig 的 current-context）")
	}
	switch caps.Metrics {
	case CapAvailable:
		// 期望路径：本集群带 metrics-server（CLAUDE.md 的 minikube addons enable metrics-server）。
	case CapUnavailable:
		t.Skip("跳过指标断言：本集群没开 metrics-server（属于环境缺件）")
	default:
		t.Errorf("指标能力 = %v：探测本身报错说明 discovery 路径有问题（真集群不该落未知）", caps.Metrics)
	}
	if !c.MetricsUsable() {
		t.Error("指标可用（或未探测到）时 MetricsUsable 应为 true")
	}
}
