package k8s

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/version"
	fakediscovery "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/kubernetes"
	corefake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"
	metricsfake "k8s.io/metrics/pkg/client/clientset/versioned/fake"
)

// 启动期探测（preflight）的单测：三态判定用假 discovery 覆盖，
// 不可达与超时是传输层语义（fake 客户端不认拨号与 ctx 取消），只能用 httptest 假 apiserver 验。

// fakeDiscoveryCollector 造一个接假 discovery 的采集器：ServerVersion 与 API 组资源由测试给。
func fakeDiscoveryCollector(t *testing.T, cfg Config) (*Collector, *fakediscovery.FakeDiscovery) {
	t.Helper()
	cs := corefake.NewSimpleClientset()
	fd, ok := cs.Discovery().(*fakediscovery.FakeDiscovery)
	if !ok {
		t.Fatalf("fake clientset 的 Discovery 类型变了: %T", cs.Discovery())
	}
	return newWithClients(cfg, cs, metricsfake.NewSimpleClientset()), fd
}

// TestPreflight_MetricsAvailable 探测通过：版本取到、指标组可用。
func TestPreflight_MetricsAvailable(t *testing.T) {
	coll, fd := fakeDiscoveryCollector(t, Config{Context: "mk"})
	// 端点地址由 New 填（fake 构造路径不经过它），这里直填以断言 Preflight 会把它带进结果供启动输出用。
	coll.apiServer = "https://10.0.0.1:6443"
	fd.FakedServerVersion = &version.Info{GitVersion: "v1.37.0"}
	fd.Resources = []*metav1.APIResourceList{{GroupVersion: metricsGroupVersion}}

	caps, err := coll.Preflight(context.Background())
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if caps.Metrics != CapAvailable {
		t.Errorf("指标能力 = %v，期望 CapAvailable", caps.Metrics)
	}
	if caps.ServerVersion != "v1.37.0" {
		t.Errorf("版本 = %q，期望 v1.37.0", caps.ServerVersion)
	}
	if caps.Context != "mk" {
		t.Errorf("context = %q，期望 mk", caps.Context)
	}
	if caps.APIServer != "https://10.0.0.1:6443" {
		t.Errorf("端点 = %q，期望带上 apiserver 地址（启动输出要用）", caps.APIServer)
	}
	if !coll.MetricsUsable() {
		t.Error("指标可用时 MetricsUsable 应为 true")
	}
}

// TestPreflight_MetricsUnavailable 指标组不存在：判"明确不可用"（裁掉 k8s_metrics），
// 但探测本身不算失败——集群是好的，只是缺一个可选能力。
func TestPreflight_MetricsUnavailable(t *testing.T) {
	coll, fd := fakeDiscoveryCollector(t, Config{})
	fd.FakedServerVersion = &version.Info{GitVersion: "v1.37.0"}
	// Resources 留空 = 该 group-version 不存在（fake discovery 返回 NotFound）。

	caps, err := coll.Preflight(context.Background())
	if err != nil {
		t.Fatalf("可选能力缺失不该算探测失败: %v", err)
	}
	if caps.Metrics != CapUnavailable {
		t.Errorf("指标能力 = %v，期望 CapUnavailable", caps.Metrics)
	}
	if coll.MetricsUsable() {
		t.Error("指标明确不可用时 MetricsUsable 应为 false（提示注册层裁掉该工具）")
	}
}

// TestPreflight_MetricsUnknown 探测本身报错（如聚合层 503）：判"未探测到"，保留工具（fail-open）。
func TestPreflight_MetricsUnknown(t *testing.T) {
	coll, fd := fakeDiscoveryCollector(t, Config{})
	fd.FakedServerVersion = &version.Info{GitVersion: "v1.37.0"}
	fd.PrependReactor("get", "resource", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("metrics backend down")
	})

	caps, err := coll.Preflight(context.Background())
	if err != nil {
		t.Fatalf("可选能力探测出错不该算致命失败: %v", err)
	}
	if caps.Metrics != CapUnknown {
		t.Errorf("指标能力 = %v，期望 CapUnknown", caps.Metrics)
	}
	if !coll.MetricsUsable() {
		t.Error("未探测到时应保留工具（fail-open），MetricsUsable 应为 true")
	}
}

// TestPreflight_Unreachable apiserver 不可达：Preflight 报错（调用方据此走 nil 退化），
// 但不能顺手把指标能力判成"不可用"——那是两件事。
func TestPreflight_Unreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	host := srv.URL
	srv.Close() // 关掉 = 连接被拒，立即失败（不等满预算）

	coll := collectorForHost(t, host, 2*time.Second)
	if _, err := coll.Preflight(context.Background()); err == nil {
		t.Fatal("apiserver 不可达时 Preflight 应报错")
	}
	if !coll.MetricsUsable() {
		t.Error("探测失败不该把指标判成不可用（fail-open）")
	}
}

// TestPreflight_Timeout 黑洞地址（用睡着的假 apiserver 模拟）：2s 是整次探测的总预算，
// 不是每个请求各自一份——ServerVersion 就把预算耗完，不会再多打一次组探测。
func TestPreflight_Timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
	}))
	defer srv.Close()

	coll := collectorForHost(t, srv.URL, 50*time.Millisecond)
	start := time.Now()
	_, err := coll.Preflight(context.Background())
	if err == nil {
		t.Fatal("探测超时该报错")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("错误应能判定为超时（errors.Is context.DeadlineExceeded），实际 %v", err)
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Errorf("预算没生效：耗时 %v，期望约 50ms（未等满 handler 的 300ms）", elapsed)
	}
}

// TestNew_ResolvesEffectiveContext 默认场景（kube_context 未配置）要把 kubeconfig 的
// current-context 归一化进 cfg.Context：否则证据包/报告的 target.context 是空串，
// 回放时分辨不出证据来自哪个集群（§9、§16.3 末）。
func TestNew_ResolvesEffectiveContext(t *testing.T) {
	path := writeKubeconfig(t, "mk")

	coll, err := New(Config{KubeConfig: path})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if coll.cfg.Context != "mk" {
		t.Errorf("未配置 kube_context 时应取 kubeconfig 的 current-context，实际 %q", coll.cfg.Context)
	}

	// 显式指定优先于文件里的 current-context。
	coll2, err := New(Config{KubeConfig: path, Context: "other"})
	if err != nil {
		t.Fatalf("New（显式 context）: %v", err)
	}
	if coll2.cfg.Context != "other" {
		t.Errorf("显式 kube_context 应优先，实际 %q", coll2.cfg.Context)
	}
}

// collectorForHost 造一个真 HTTP 客户端指向给定地址的采集器（探测失败/超时的传输语义只能这么验）。
func collectorForHost(t *testing.T, host string, timeout time.Duration) *Collector {
	t.Helper()
	core, err := kubernetes.NewForConfig(&rest.Config{Host: host})
	if err != nil {
		t.Fatalf("build clientset: %v", err)
	}
	return newWithClients(Config{PreflightTimeout: timeout}, core, metricsfake.NewSimpleClientset())
}

// writeKubeconfig 写一份最小可用的 kubeconfig（两个 context：mk / other），返回文件路径。
// New 只读文件不拨号，所以 server 指向哪里都行。
func writeKubeconfig(t *testing.T, current string) string {
	t.Helper()
	const tpl = `apiVersion: v1
kind: Config
current-context: %s
clusters:
- name: c1
  cluster:
    server: https://127.0.0.1:6443
contexts:
- name: mk
  context:
    cluster: c1
    user: u1
- name: other
  context:
    cluster: c1
    user: u1
users:
- name: u1
  user:
    token: dummy
`
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte(fmt.Sprintf(tpl, current)), 0o600); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}
	return path
}
