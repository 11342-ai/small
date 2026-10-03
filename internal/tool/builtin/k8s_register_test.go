package builtin

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"small/internal/k8s"
	"small/internal/tool"
)

// k8sToolsAlways 采集器就绪即注册的八个工具（指标那个是唯一的可裁剪项）。
var k8sToolsAlways = []string{"k8s_pod", "k8s_workload", "k8s_events", "k8s_logs",
	"k8s_node", "k8s_nodes", "k8s_evidence", "k8s_report"}

// TestRegisterBuiltins_K8sMetricsByCapability 注册一致性（k8s-diagnosis.md §16.5）：
// 集群明确没有 metrics.k8s.io/v1beta1 时 k8s_metrics 不进注册表，其余八个照常在。
//
// 为什么用 httptest 假 apiserver 而不是假客户端：能力判定发生在 Preflight，而能力字段是
// 采集包的私有状态（没有测试专用注入口），所以只能走"真 HTTP 语义"这条贴近生产的路径。
func TestRegisterBuiltins_K8sMetricsByCapability(t *testing.T) {
	cases := []struct {
		name       string
		metricsOK  bool
		wantMetric bool
	}{
		{"指标可用", true, true},
		{"指标组不存在（明确不可用）", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			coll := collectorAfterPreflight(t, tc.metricsOK)
			reg := tool.New()
			if err := RegisterBuiltins(reg, Deps{K8s: coll}); err != nil {
				t.Fatalf("RegisterBuiltins: %v", err)
			}
			for _, name := range k8sToolsAlways {
				if _, ok := reg.Get(name); !ok {
					t.Errorf("%s 应始终注册（能力裁剪只作用于指标工具）", name)
				}
			}
			if _, ok := reg.Get("k8s_metrics"); ok != tc.wantMetric {
				t.Errorf("k8s_metrics 注册状态 = %v，期望 %v", ok, tc.wantMetric)
			}
		})
	}
}

// collectorAfterPreflight 用假 apiserver 造一个"跑过启动期探测"的采集器：
// /version 给版本，metrics 组按用例返回 404（明确不可用）或正常资源列表（可用）。
func collectorAfterPreflight(t *testing.T, metricsOK bool) *k8s.Collector {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/version":
			fmt.Fprint(w, `{"major":"1","minor":"37","gitVersion":"v1.37.0"}`)
		case "/apis/metrics.k8s.io/v1beta1":
			if !metricsOK {
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprint(w, `{"kind":"Status","apiVersion":"v1","status":"Failure","code":404,"reason":"NotFound","message":"the server could not find the requested resource"}`)
				return
			}
			fmt.Fprint(w, `{"kind":"APIResourceList","apiVersion":"v1","groupVersion":"metrics.k8s.io/v1beta1","resources":[{"name":"pods","namespaced":true,"kind":"PodMetrics","verbs":["get"]}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	path := filepath.Join(t.TempDir(), "kubeconfig")
	cfg := fmt.Sprintf("apiVersion: v1\nkind: Config\ncurrent-context: fake\n"+
		"clusters:\n- name: c1\n  cluster:\n    server: %s\n"+
		"contexts:\n- name: fake\n  context:\n    cluster: c1\n    user: u1\n"+
		"users:\n- name: u1\n  user:\n    token: dummy\n", srv.URL)
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}
	coll, err := k8s.New(k8s.Config{KubeConfig: path})
	if err != nil {
		t.Fatalf("k8s.New: %v", err)
	}
	if _, err := coll.Preflight(context.Background()); err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	return coll
}
