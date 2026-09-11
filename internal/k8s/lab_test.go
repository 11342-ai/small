//go:build k8slab

package k8s

// 真集群冒烟测试：断言"采集层在真集群上拿到的东西对不对"，不涉 LLM（端到端打分另做，见设计文档 §11）。
//
// 默认不参与 go test（build tag k8slab），因为合并门槛必须在无集群环境可跑：
//
//	bash Zoo/k8s-lab/smoke.sh up     # 建场景（namespace diag-lab + 两个故障目标）
//	bash Zoo/k8s-lab/smoke.sh test   # 即 go test -tags k8slab ./internal/k8s/ -run Lab -v
//	bash Zoo/k8s-lab/smoke.sh down   # 清场景
//
// 集群或场景缺失时 t.Skip 而不是失败：没起集群的人跑这个 tag 不该被卡住。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// labNamespace 冒烟场景命名空间（与 Zoo/k8s-lab/smoke.yaml 对齐）。
const labNamespace = "diag-lab"

// labCollector 连本地集群（kubeconfig 走缺省 ~/.kube/config），并确认场景已就绪。
func labCollector(t *testing.T) *Collector {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("跳过：取不到 home 目录（%v）", err)
	}
	c, err := New(Config{KubeConfig: filepath.Join(home, ".kube", "config"), Dir: t.TempDir()})
	if err != nil {
		t.Skipf("跳过：连不上集群（%v）", err)
	}
	if _, err := c.core.CoreV1().Namespaces().Get(context.Background(), labNamespace, metav1.GetOptions{}); err != nil {
		t.Skipf("跳过：冒烟场景未就绪，先跑 bash Zoo/k8s-lab/smoke.sh up（%v）", err)
	}
	return c
}

// TestLabHealthyPod 正常 Pod：八类证据应全部取到（采集链路的正向基线）。
// 顺带守住一个隐性回归：证据齐全时不该出现降级 Note。
func TestLabHealthyPod(t *testing.T) {
	c := labCollector(t)
	ctx := context.Background()
	// 用标签选 kube-dns（Deployment 管理，有 owner 链、日志与指标都稳定），而不是取 kube-system
	// 列表首项：列表顺序无保障，可能选中静态 Pod——静态 Pod 无 controller owner，工作负载必然取不到。
	list, err := c.core.CoreV1().Pods("kube-system").List(ctx, metav1.ListOptions{LabelSelector: "k8s-app=kube-dns"})
	if err != nil || len(list.Items) == 0 {
		t.Skipf("跳过：kube-system 下没有 kube-dns Pod（%v）", err)
	}
	target := list.Items[0]
	ev, err := c.Collect(ctx, target.Namespace, target.Name)
	if err != nil {
		t.Fatalf("Collect(%s/%s): %v", target.Namespace, target.Name, err)
	}
	if ev.Pod == nil || ev.Pod.Name != target.Name {
		t.Fatalf("Pod 证据缺失: %+v", ev.Pod)
	}
	// 不断言"事件非空"：kube-apiserver 的事件 TTL 默认 1 小时（--event-ttl），而这里选的健康 Pod
	// 通常已运行数小时，事件早已回收——此处的空是正常现象，不是采集缺陷。
	// "事件非空"留在故障目标那条测试上（TestLabImagePullBackOff），那边的事件是刚生成的。
	if len(ev.Logs) == 0 {
		t.Errorf("日志为空：Running 容器应能读到日志")
	}
	if ev.Metrics == nil {
		t.Errorf("metrics 为空：metrics-server 就绪时应能取到用量")
	}
	if ev.Node == nil || ev.Node.Name != target.Spec.NodeName {
		t.Errorf("节点证据不对（期望 %q）: %+v", target.Spec.NodeName, ev.Node)
	}
	if ev.Target.Workload == "" {
		t.Errorf("未解析出顶层工作负载: %+v", ev.Target)
	}
	// 只对"必需来源失败"严格：可选源（metrics 类）在环境抖动时会降级，把那种降级
	// 判成采集缺陷正是 Note 分级要解决的问题（2026-09-12 的 metrics-server 重启窗口）。
	if req := notesOfKind(ev.Notes, NoteRequired); len(req) > 0 {
		t.Errorf("正常目标不该有必需来源失败: %v", req)
	}
	if len(ev.Notes) > 0 {
		t.Logf("本次采集的降级说明（可选源允许）: %v", ev.Notes)
	}
	if _, err := c.Save(ev); err != nil {
		t.Errorf("Save: %v", err)
	}
}

// TestLabImagePullBackOff 拉镜像失败：症状字段、事件、日志降级都要对。
func TestLabImagePullBackOff(t *testing.T) {
	c := labCollector(t)
	ev, err := c.Collect(context.Background(), labNamespace, "badimg")
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if ev.Pod == nil || len(ev.Pod.Containers) == 0 {
		t.Fatalf("Pod/容器证据缺失: %+v", ev.Pod)
	}
	ct := ev.Pod.Containers[0]
	// ErrImagePull 与 ImagePullBackOff 是同一故障的前后两个阶段，都算命中。
	if !strings.Contains(ct.StateReason, "ImagePull") {
		t.Errorf("症状不对: %q（期望 ImagePullBackOff 或 ErrImagePull）", ct.StateReason)
	}
	if ct.StateMessage == "" {
		t.Errorf("state_message 为空：registry 报错原文是这条故障的关键证据")
	}
	// 不断言具体文案：拉取失败的 message 随环境变（tag 不存在是 404 not found，registry 抖动是
	// EOF/timeout），只断言"kubelet 报了这次拉取失败"——具体的失败类型由模型按 message 判。
	// 大小写不敏感：事件文案是 "Failed to pull image ..."（首字母大写）。
	var warned bool
	for _, e := range ev.Events {
		if e.Type == "Warning" && strings.Contains(strings.ToLower(e.Message), "failed to pull image") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("事件里没有拉取失败的 Warning: %+v", ev.Events)
	}
	if len(ev.Logs) != 0 {
		t.Errorf("容器未启动，不该有日志: %+v", ev.Logs)
	}
	// 容器从未启动 → 日志本来就不存在，kubelet 用 400 回这种请求，该定性为 no_data
	// 而不是"采集失败"（真集群对这条分类的验证就落在这里）。
	if !strings.Contains(noteText(notesOfKind(ev.Notes, NoteNoData)), "日志未取到") {
		t.Errorf("容器未启动时的读日志失败该记成 %s: %v", NoteNoData, ev.Notes)
	}
}

// TestLabTerminalPodNotCounted 计数口径回归：终态 Pod（smoke.yaml 里那个一次性 Job 的 Succeeded Pod）
// 会出现在 kubectl get pods 里，但不该计入 pods_on_node，也不该让 free_on_node 的 pods 项虚高。
func TestLabTerminalPodNotCounted(t *testing.T) {
	c := labCollector(t)
	ctx := context.Background()
	list, err := c.core.CoreV1().Pods(labNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list pods: %v", err)
	}
	var terminal *corev1.Pod
	for i := range list.Items {
		if p := &list.Items[i]; p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			terminal = p
			break
		}
	}
	if terminal == nil {
		t.Skip("跳过：场景里没有终态 Pod（先跑 smoke.sh up，它会创建一个一次性 Job）")
	}

	// 含终态的原始计数应严格大于我们的 pods_on_node（后者排除终态）。
	all, err := c.core.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		FieldSelector: "spec.nodeName=" + terminal.Spec.NodeName,
	})
	if err != nil {
		t.Fatalf("list pods on node: %v", err)
	}
	nv, err := c.Node(ctx, terminal.Spec.NodeName)
	if err != nil {
		t.Fatalf("Node: %v", err)
	}
	if nv.PodsOnNode >= len(all.Items) {
		t.Errorf("终态 Pod 未被排除：pods_on_node=%d，节点上 Pod 总数（含终态）=%d", nv.PodsOnNode, len(all.Items))
	}
	// free 的 pods 项必须扣掉已用 Pod 数（漏扣时它会等于总额度，看起来"一个都没占"）。
	if nv.Allocatable["pods"] != "" && nv.FreeOnNode["pods"] == nv.Allocatable["pods"] {
		t.Errorf("free_on_node[pods] 未扣除已用数量：free=%q allocatable=%q",
			nv.FreeOnNode["pods"], nv.Allocatable["pods"])
	}
}

// TestLabWorkloadChain Deployment 链：Pod → ReplicaSet → Deployment 上溯 + 模板规格可读。
func TestLabWorkloadChain(t *testing.T) {
	c := labCollector(t)
	ctx := context.Background()
	list, err := c.core.CoreV1().Pods(labNamespace).List(ctx, metav1.ListOptions{LabelSelector: "app=badweb"})
	if err != nil || len(list.Items) == 0 {
		t.Skipf("跳过：badweb 场景未就绪（%v）", err)
	}
	pod := list.Items[0]
	ev, err := c.Collect(ctx, pod.Namespace, pod.Name)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if ev.Target.Workload != "Deployment/badweb" {
		t.Errorf("未上溯到 Deployment: %q", ev.Target.Workload)
	}
	if ev.Workload == nil || ev.Workload.Kind != "Deployment" {
		t.Fatalf("工作负载证据缺失: %+v", ev.Workload)
	}
	if len(ev.Workload.Template.Containers) == 0 || !strings.Contains(ev.Workload.Template.Containers[0].Image, "busybox") {
		t.Errorf("模板容器规格不对: %+v", ev.Workload.Template.Containers)
	}
	if ev.Workload.Replicas != 1 {
		t.Errorf("副本数不对: %d", ev.Workload.Replicas)
	}
}
