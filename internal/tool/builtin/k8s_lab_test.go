//go:build k8slab

package builtin

// 工具层真集群冒烟（tag k8slab）：验证九个工具是否真能把采集结果送到模型侧。
// 为什么单独做：fake 单测只覆盖参数校验，JSON Schema 与解码字段的对齐只能靠脚本核对，
// 而"工具 → 采集包 → apiserver"这条链路只有真集群能验（真集群曾抓出 ctx 提前取消的 bug）。
//
// 跑法（前置：minikube 已启动）：
//
//	bash Zoo/k8s-lab/smoke.sh up     # 建 diag-lab 故障场景（不建则只跑健康路径断言）
//	go test -tags k8slab ./internal/tool/builtin/ -run K8sTools -v
//	bash Zoo/k8s-lab/smoke.sh down
//
// 目标两个：故障 Pod（diag-lab/badimg，裸 Pod + ImagePullBackOff）与
// 健康且受 Deployment 管理的 Pod（kube-system 的 kube-dns），覆盖正常路径与降级路径。

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"small/internal/k8s"
	"small/internal/tool"
)

func TestK8sToolsAgainstCluster(t *testing.T) {
	ctx := context.Background()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("拿不到 HOME")
	}
	kubeconfig := filepath.Join(home, ".kube", "config")
	dir := t.TempDir()
	coll, err := k8s.New(k8s.Config{KubeConfig: kubeconfig, Dir: dir})
	if err != nil {
		t.Skipf("连不上集群，跳过: %v", err)
	}
	cs, err := labClientset(kubeconfig)
	if err != nil {
		t.Skipf("建 clientset 失败，跳过: %v", err)
	}

	healthy, err := labRunningPod(ctx, cs, "kube-system", "k8s-app=kube-dns")
	if err != nil {
		t.Skipf("找不到健康目标 Pod，跳过: %v", err)
	}
	t.Run("健康 Pod（受 Deployment 管理）", func(t *testing.T) {
		// k8s_pod：症状字段应在，且是 Running。
		data := execToolOK(t, K8sPod(coll), map[string]any{"namespace": healthy.Namespace, "pod": healthy.Name})
		if !strings.Contains(data, `"phase": "Running"`) {
			t.Errorf("k8s_pod 没给出 Running: %s", clip(data))
		}
		// k8s_workload：必须上溯到 Deployment（工具调错采集方法的回归点）。
		w := execToolOK(t, K8sWorkload(coll), map[string]any{"namespace": healthy.Namespace, "pod": healthy.Name})
		if !strings.Contains(w, `"kind": "Deployment"`) {
			t.Errorf("k8s_workload 未上溯到 Deployment: %s", clip(w))
		}
		// k8s_logs：不传 container，靠 LogTargets 自动选，必须成功且回显选中容器名。
		l := execToolOK(t, K8sLogs(coll), map[string]any{"namespace": healthy.Namespace, "pod": healthy.Name})
		if !strings.Contains(l, `"container":`) {
			t.Errorf("k8s_logs 未回显容器名: %s", clip(l))
		}
		// k8s_node：用 Pod 所在节点，必须有可分配量与剩余余量（Pending 归因的分母）。
		n := execToolOK(t, K8sNode(coll), map[string]any{"node": healthy.Spec.NodeName})
		if !strings.Contains(n, `"allocatable"`) {
			t.Errorf("k8s_node 缺 allocatable: %s", clip(n))
		}
		if !strings.Contains(n, `"free_on_node"`) {
			t.Errorf("k8s_node 缺 free_on_node（余量）: %s", clip(n))
		}
		// k8s_nodes：列全部节点（Pending 归因的入口）——标签与余量都必须在，且是 JSON 数组。
		ns := execToolOK(t, K8sNodes(coll), map[string]any{})
		if !strings.HasPrefix(strings.TrimSpace(ns), "[") {
			t.Errorf("k8s_nodes 回灌的不是数组: %s", clip(ns))
		}
		if !strings.Contains(ns, `"labels"`) || !strings.Contains(ns, `"free_on_node"`) {
			t.Errorf("k8s_nodes 缺标签或余量: %s", clip(ns))
		}
	})

	t.Run("故障 Pod（裸 Pod + ImagePullBackOff）", func(t *testing.T) {
		_, err := coll.Pod(ctx, "diag-lab", "badimg")
		if err != nil {
			t.Skipf("故障场景未就绪（先跑 smoke.sh up），跳过: %v", err)
		}
		p := execToolOK(t, K8sPod(coll), map[string]any{"namespace": "diag-lab", "pod": "badimg"})
		// 拉镜像失败会在 ErrImagePull 与 ImagePullBackOff 之间来回跳（退避重试），两者都算命中。
		if !strings.Contains(p, "ImagePullBackOff") && !strings.Contains(p, "ErrImagePull") {
			t.Errorf("k8s_pod 没给出拉镜像失败的原因: %s", clip(p))
		}
		e := execToolOK(t, K8sEvents(coll), map[string]any{"namespace": "diag-lab", "pod": "badimg"})
		if !strings.Contains(e, "Warning") {
			t.Errorf("k8s_events 没给出 Warning: %s", clip(e))
		}
		// 容器从未启动：日志读不到应当是"业务失败"（IsError=true, err=nil），不能中断循环。
		res, err := K8sLogs(coll).Execute(ctx, toolArgs(t, map[string]any{"namespace": "diag-lab", "pod": "badimg"}))
		if err != nil {
			t.Fatalf("日志失败被当成框架级错误（会中断 agent 循环）: %v", err)
		}
		if !res.IsError || !strings.Contains(res.Data, "waiting") {
			t.Errorf("容器未启动应回灌业务失败并说明 waiting，实际: %+v", res)
		}
		// 指标同理：metrics-server 没有采样时也必须是业务失败。
		if _, err := K8sMetrics(coll).Execute(ctx, toolArgs(t, map[string]any{"namespace": "diag-lab", "pod": "badimg"})); err != nil {
			t.Fatalf("指标失败被当成框架级错误: %v", err)
		}
		// 裸 Pod 无 controller owner：工作负载上溯应是业务失败而非框架错误。
		res, err = K8sWorkload(coll).Execute(ctx, toolArgs(t, map[string]any{"namespace": "diag-lab", "pod": "badimg"}))
		if err != nil {
			t.Fatalf("工作负载失败被当成框架级错误: %v", err)
		}
		if !res.IsError {
			t.Errorf("裸 Pod 上溯工作负载应回灌业务失败，实际: %+v", res)
		}
	})

	t.Run("证据包与报告落盘", func(t *testing.T) {
		if _, err := coll.Pod(ctx, "diag-lab", "badimg"); err != nil {
			t.Skipf("故障场景未就绪（先跑 smoke.sh up），跳过: %v", err)
		}
		// k8s_evidence：一次收集 + 自动落盘，回灌纯 JSON（可直接解析）。
		ev := execToolOK(t, K8sEvidence(coll), map[string]any{"namespace": "diag-lab", "pod": "badimg"})
		if !strings.Contains(ev, `"notes"`) && !strings.Contains(ev, `"pod"`) {
			t.Errorf("k8s_evidence 返回里没有证据内容: %s", clip(ev))
		}
		if _, err := os.Stat(filepath.Join(dir, "diag-lab-badimg.evidence.json")); err != nil {
			t.Errorf("证据包文件不存在: %v", err)
		}
		// k8s_report：回灌"落盘路径 + 渲染后的 md"（不是 JSON，故用宽松版断言）。
		// 证据给两条、两个来源：≥0.80 的置信度必须满足 §7.2 的硬校验（checkConfidenceRules），
		// 否则会被打回——夹具要跟规则一起走，别让"规则生效"在冒烟里表现成红灯。
		rep := execToolText(t, K8sReport(coll), map[string]any{
			"namespace":  "diag-lab",
			"pod":        "badimg",
			"symptoms":   []string{"ImagePullBackOff"},
			"root_cause": map[string]any{"summary": "镜像 tag 不存在", "category": "imagepull_tag_missing", "confidence": 0.9},
			"evidence": []map[string]any{
				{"source": "k8s_events", "ref": "badimg", "excerpt": "not found", "supports": "拉取镜像失败"},
				{"source": "k8s_pod", "ref": "badimg", "excerpt": "waiting reason=ImagePullBackOff", "supports": "容器从未启动，符合拉取失败"},
			},
			"suggestions": []map[string]any{
				{"action": "改用存在的镜像 tag"},
			},
		})
		if !strings.Contains(rep, "report.json") {
			t.Errorf("k8s_report 未回灌落盘路径: %s", rep)
		}
		md, err := os.ReadFile(filepath.Join(dir, "diag-lab-badimg.report.md"))
		if err != nil {
			t.Fatalf("报告 md 不存在: %v", err)
		}
		if !strings.Contains(string(md), "Root Cause: 镜像 tag 不存在") {
			t.Errorf("报告 md 内容不对:\n%s", md)
		}
	})
}

// execToolOK 执行工具并断言"成功 + 回灌文本是合法 JSON"，返回回灌文本。
// 名字与 kb_test.go 的 execTool 区分开（那个签名的参数是字符串，语义也不同）。
func execToolOK(t *testing.T, tl tool.Tool, args map[string]any) string {
	t.Helper()
	res, err := tl.Execute(context.Background(), toolArgs(t, args))
	if err != nil {
		t.Fatalf("%s 返回框架级错误: %v", tl.Spec().Name, err)
	}
	if res.IsError {
		t.Fatalf("%s 业务失败: %s", tl.Spec().Name, res.Data)
	}
	var v any
	if err := json.Unmarshal([]byte(res.Data), &v); err != nil {
		t.Fatalf("%s 回灌文本不是合法 JSON: %v", tl.Spec().Name, err)
	}
	return res.Data
}

// execToolText 执行工具并只断言"成功"，返回回灌文本（给回灌形态为纯文本的工具用，如 k8s_report）。
func execToolText(t *testing.T, tl tool.Tool, args map[string]any) string {
	t.Helper()
	res, err := tl.Execute(context.Background(), toolArgs(t, args))
	if err != nil {
		t.Fatalf("%s 返回框架级错误: %v", tl.Spec().Name, err)
	}
	if res.IsError {
		t.Fatalf("%s 业务失败: %s", tl.Spec().Name, res.Data)
	}
	if strings.TrimSpace(res.Data) == "" {
		t.Fatalf("%s 回灌文本为空", tl.Spec().Name)
	}
	return res.Data
}

// toolArgs 把参数图编码成 json.RawMessage。
func toolArgs(t *testing.T, args map[string]any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("参数编码失败: %v", err)
	}
	return b
}

// labClientset 建一个客户端，只用于在测试里定位目标 Pod（生产代码里这件事由采集包做）。
func labClientset(kubeconfig string) (kubernetes.Interface, error) {
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(cfg)
}

// labRunningPod 按标签选一个 Running 的 Pod。
func labRunningPod(ctx context.Context, cs kubernetes.Interface, ns, selector string) (*corev1.Pod, error) {
	sel, err := labels.Parse(selector)
	if err != nil {
		return nil, err
	}
	list, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: sel.String()})
	if err != nil {
		return nil, err
	}
	for i := range list.Items {
		if list.Items[i].Status.Phase == corev1.PodRunning {
			return &list.Items[i], nil
		}
	}
	return nil, errors.New("没有 Running 的 Pod")
}

// clip 截断长文本，让失败信息可读。
func clip(s string) string {
	if len(s) > 400 {
		return s[:400] + "...(截断)"
	}
	return s
}
