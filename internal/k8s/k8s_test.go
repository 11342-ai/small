package k8s

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	corefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	metricsfake "k8s.io/metrics/pkg/client/clientset/versioned/fake"

	appsv1 "k8s.io/api/apps/v1"
)

// 单测用 fake clientset，不依赖真集群（合并门槛里能跑）；
// 真集群的端到端校验留给带 build tag 的集成测试（设计文档 §11）。

// newTestCollector 造一个接 fake 客户端的采集器（Dir 缺省用临时目录，避免污染）。
// metrics 侧用 reactor 显式应答：metrics 的 fake 生成器把资源名注册成 "pods"，
// 而 tracker 从 Kind 猜出的是 "podmetrics"，直接 NewSimpleClientset(pm) 会查不到。
func newTestCollector(t *testing.T, coreObjs []runtime.Object, podMetrics *metricsv1beta1.PodMetrics, nodeMetrics *metricsv1beta1.NodeMetrics, cfg Config) *Collector {
	t.Helper()
	if cfg.Dir == "" {
		cfg.Dir = t.TempDir()
	}
	mc := metricsfake.NewSimpleClientset()
	mc.PrependReactor("get", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if podMetrics == nil {
			return false, nil, nil // 落到 tracker → NotFound（模拟 metrics-server 不可用）
		}
		get := action.(k8stesting.GetAction)
		if get.GetNamespace() != podMetrics.Namespace || get.GetName() != podMetrics.Name {
			return true, nil, apierrors.NewNotFound(action.GetResource().GroupResource(), get.GetName())
		}
		return true, podMetrics, nil
	})
	mc.PrependReactor("get", "nodes", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if nodeMetrics == nil {
			return false, nil, nil
		}
		get := action.(k8stesting.GetAction)
		if get.GetName() != nodeMetrics.Name {
			return true, nil, apierrors.NewNotFound(action.GetResource().GroupResource(), get.GetName())
		}
		return true, nodeMetrics, nil
	})
	return newWithClients(cfg, corefake.NewSimpleClientset(coreObjs...), mc)
}

// oomPod 造一个典型的 OOMKilled CrashLoop 现场：容器处于 CrashLoopBackOff，
// 上次终止原因 OOMKilled（exit 137），limits 64Mi，重启 7 次。
func oomPod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "web-0", Namespace: "default", UID: "uid-1",
			CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Hour)),
			Labels:            map[string]string{"app": "web"},
			Annotations: map[string]string{
				"deployment.kubernetes.io/revision":                "3",
				"kubectl.kubernetes.io/last-applied-configuration": strings.Repeat("x", 5000),
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "web-abc", Controller: boolPtr(true),
			}},
		},
		Spec: corev1.PodSpec{
			NodeName: "minikube",
			Containers: []corev1.Container{{
				Name:    "app",
				Image:   "busybox:1.36",
				Command: []string{"/bin/sh"},
				Args:    []string{"-c", "start.sh"},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("32Mi")},
					Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("64Mi")},
				},
				LivenessProbe: &corev1.Probe{
					ProbeHandler:        corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromInt32(8080)}},
					InitialDelaySeconds: 5, PeriodSeconds: 10, FailureThreshold: 3,
				},
			}},
		},
		Status: corev1.PodStatus{
			Phase:    corev1.PodRunning,
			QOSClass: corev1.PodQOSBurstable,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "app", Image: "busybox:1.36", Ready: false, RestartCount: 7,
				State: corev1.ContainerState{
					Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff", Message: "back-off 5m0s restarting failed container"},
				},
				LastTerminationState: corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{
						Reason: "OOMKilled", ExitCode: 137,
						StartedAt:  metav1.NewTime(time.Now().Add(-10 * time.Minute)),
						FinishedAt: metav1.NewTime(time.Now().Add(-9 * time.Minute)),
					},
				},
			}},
		},
	}
}

// TestPodViewOOMKilled 验证 Pod 裁剪视图：症状字段齐、噪声字段（managedFields 类注解）被裁掉。
func TestPodViewOOMKilled(t *testing.T) {
	c := newTestCollector(t, []runtime.Object{oomPod()}, nil, nil, Config{})
	pv, err := c.Pod(context.Background(), "default", "web-0")
	if err != nil {
		t.Fatalf("Pod: %v", err)
	}
	if pv.Phase != "Running" || pv.NodeName != "minikube" {
		t.Errorf("phase/node 不对: %q %q", pv.Phase, pv.NodeName)
	}
	// 注解白名单：revision 保留，last-applied-configuration（5KB 噪声）必须不在。
	if pv.Annotations["deployment.kubernetes.io/revision"] != "3" {
		t.Errorf("白名单注解丢了: %v", pv.Annotations)
	}
	if _, ok := pv.Annotations["kubectl.kubernetes.io/last-applied-configuration"]; ok {
		t.Errorf("噪声注解未裁剪: %v", pv.Annotations)
	}
	if len(pv.Containers) != 1 {
		t.Fatalf("容器数不对: %d", len(pv.Containers))
	}
	ct := pv.Containers[0]
	if ct.State != "waiting" || ct.StateReason != "CrashLoopBackOff" {
		t.Errorf("当前状态不对: %s/%s", ct.State, ct.StateReason)
	}
	if ct.LastStateReason != "OOMKilled" {
		t.Errorf("上次终止原因（OOMKilled 落点）不对: %q", ct.LastStateReason)
	}
	if ct.LastStateExitCode == nil || *ct.LastStateExitCode != 137 {
		t.Errorf("上次退出码不对: %v", ct.LastStateExitCode)
	}
	if ct.RestartCount != 7 {
		t.Errorf("重启次数不对: %d", ct.RestartCount)
	}
	if ct.Limits["memory"] != "64Mi" || ct.Requests["memory"] != "32Mi" {
		t.Errorf("resources 单位未转人读: %v / %v", ct.Requests, ct.Limits)
	}
	if len(ct.Probes) != 1 || ct.Probes[0].Handler != "http HTTP /healthz:8080" {
		t.Errorf("探针摘要不对: %+v", ct.Probes)
	}
	if len(pv.OwnerRefs) != 1 || pv.OwnerRefs[0].Kind != "ReplicaSet" || !pv.OwnerRefs[0].Controller {
		t.Errorf("owner 链不对: %+v", pv.OwnerRefs)
	}
}

// TestEventsMergeFilterOrder 验证事件去重合并、时间窗过滤、Warning 优先排序与条数截断。
func TestEventsMergeFilterOrder(t *testing.T) {
	now := time.Now()
	mk := func(typ, reason, msg string, count int32, age time.Duration) *corev1.Event {
		ts := metav1.NewTime(now.Add(-age))
		return &corev1.Event{
			ObjectMeta:     metav1.ObjectMeta{Name: typ + reason + age.String(), Namespace: "default"},
			InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "web-0"},
			Type:           typ, Reason: reason, Message: msg, Count: count,
			FirstTimestamp: ts, LastTimestamp: ts,
			Source: corev1.EventSource{Component: "kubelet"},
		}
	}
	objs := []runtime.Object{
		mk("Warning", "BackOff", "Back-off restarting failed container", 3, 5*time.Minute),
		mk("Warning", "BackOff", "Back-off restarting failed container", 2, time.Minute), // 同 key，应合并
		mk("Normal", "Pulled", "Successfully pulled image", 1, 30*time.Second),
		mk("Warning", "FailedScheduling", "0/1 nodes are available", 1, 3*time.Hour), // 超出 1h 窗口，应被过滤
	}
	c := newTestCollector(t, objs, nil, nil, Config{})
	events, err := c.Events(context.Background(), "default", "web-0", "uid-1", time.Hour, 10)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("事件数不对（应过滤超窗口者并合并同 key）: %d %+v", len(events), events)
	}
	if events[0].Reason != "BackOff" || events[0].Count != 5 {
		t.Errorf("Warning 未置顶或未合并计数: %+v", events[0])
	}
	if events[1].Reason != "Pulled" || events[1].Type != "Normal" {
		t.Errorf("Normal 事件排序不对: %+v", events[1])
	}
	// 截断保留 Warning：limit=1 时留下的必须是 Warning。
	one, err := c.Events(context.Background(), "default", "web-0", "uid-1", time.Hour, 1)
	if err != nil {
		t.Fatalf("Events(limit=1): %v", err)
	}
	if len(one) != 1 || one[0].Type != "Warning" {
		t.Errorf("截断未优先保留 Warning: %+v", one)
	}
}

// TestLogTargets 验证日志目标选择：优先异常容器，且仅在确有上次记录时取 previous。
func TestLogTargets(t *testing.T) {
	pv := toPodView(oomPod())
	pv.Containers = append([]ContainerView{{Name: "sidecar", State: "running"}}, pv.Containers...)
	got := LogTargets(&pv)
	if len(got) != 2 {
		t.Fatalf("目标数不对: %+v", got)
	}
	if got[0].Container != "app" || got[0].Previous {
		t.Errorf("未优先异常容器当前日志: %+v", got[0])
	}
	if got[1].Container != "app" || !got[1].Previous {
		t.Errorf("未取异常容器的 previous 日志: %+v", got[1])
	}

	// 健康容器：只取当前日志，不取 previous（无上次记录时取会直接报错）。
	healthy := toPodView(oomPod())
	healthy.Containers[0].StateReason = "running"
	healthy.Containers[0].LastStateReason = ""
	healthy.Containers[0].LastState = ""
	healthy.Containers[0].RestartCount = 0
	if g := LogTargets(&healthy); len(g) != 1 || g[0].Previous {
		t.Errorf("健康容器不应取 previous: %+v", g)
	}
}

// TestWorkloadOf_FollowsReplicaSetToDeployment 验证 owner 链上溯：Pod → ReplicaSet → Deployment。
func TestWorkloadOf_FollowsReplicaSetToDeployment(t *testing.T) {
	replicas := int32(2)
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{Name: "web-abc", Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "web", Controller: boolPtr(true)}}},
		Spec: appsv1.ReplicaSetSpec{Replicas: &replicas},
	}
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name: "app", Image: "busybox:1.36",
				Resources: corev1.ResourceRequirements{
					Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("128Mi")},
				},
			}}}},
		},
	}
	c := newTestCollector(t, []runtime.Object{oomPod(), rs, dep}, nil, nil, Config{})
	w, err := c.WorkloadOf(context.Background(), "default", "web-0")
	if err != nil {
		t.Fatalf("WorkloadOf: %v", err)
	}
	if w.Kind != "Deployment" || w.Name != "web" || w.Replicas != 2 {
		t.Errorf("未上溯到 Deployment: %+v", w)
	}
	if len(w.Template.Containers) != 1 || w.Template.Containers[0].Limits["memory"] != "128Mi" {
		t.Errorf("模板规格不对: %+v", w.Template.Containers)
	}
}

// TestPodMetricsUsagePercent 验证用量与"占 limit 百分比"的换算（内存 99% 类判断的依据）。
func TestPodMetricsUsagePercent(t *testing.T) {
	pm := &metricsv1beta1.PodMetrics{
		ObjectMeta: metav1.ObjectMeta{Name: "web-0", Namespace: "default"},
		Timestamp:  metav1.Now(),
		Containers: []metricsv1beta1.ContainerMetrics{{
			Name: "app",
			Usage: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("63Mi"),
				corev1.ResourceCPU:    resource.MustParse("120m"),
			},
		}},
	}
	c := newTestCollector(t, []runtime.Object{oomPod()}, pm, nil, Config{})
	m, err := c.PodMetrics(context.Background(), "default", "web-0")
	if err != nil {
		t.Fatalf("PodMetrics: %v", err)
	}
	if len(m.Containers) != 1 {
		t.Fatalf("容器指标数不对: %d", len(m.Containers))
	}
	cm := m.Containers[0]
	if cm.Memory != "63Mi" || cm.MemoryLimit != "64Mi" {
		t.Errorf("内存用量/限值不对: %q / %q", cm.Memory, cm.MemoryLimit)
	}
	if cm.MemoryPercent == nil || *cm.MemoryPercent < 98 || *cm.MemoryPercent > 99 {
		t.Errorf("内存使用率不对: %v", cm.MemoryPercent)
	}
	if cm.CPUPercent != nil {
		t.Errorf("无 CPU limit 时不应给出使用率: %v", cm.CPUPercent)
	}
}

// TestCollectDegraded 验证降级可见且分类正确：来源取不到不中断采集，
// 且"必需来源失败"与"可选来源不可用"分得开——评测与断言才能只对必需项严格。
func TestCollectDegraded(t *testing.T) {
	// fake 集群里只有 Pod：无 Node、无 metrics；这个 Pod 由 ReplicaSet 托管，但 RS 不存在——
	// 那是"有 owner 却读不到"，属于必需来源失败。
	c := newTestCollector(t, []runtime.Object{oomPod()}, nil, nil, Config{})
	ev, err := c.Collect(context.Background(), "default", "web-0")
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if ev.Pod == nil || ev.Pod.Name != "web-0" {
		t.Fatalf("主目标证据缺失: %+v", ev.Pod)
	}
	if ev.Workload != nil || ev.Metrics != nil || ev.Node != nil {
		t.Errorf("fake 集群里这些来源本应取不到: %+v", ev)
	}
	req := noteText(notesOfKind(ev.Notes, NoteRequired))
	for _, want := range []string{"工作负载规格未取到", "节点信息未取到"} {
		if !strings.Contains(req, want) {
			t.Errorf("必需来源失败该记成 %s，缺 %q: %s", NoteRequired, want, noteText(ev.Notes))
		}
	}
	if !strings.Contains(noteText(notesOfKind(ev.Notes, NoteOptional)), "Pod 指标未取到") {
		t.Errorf("metrics 是可选来源，该记成 %s: %s", NoteOptional, noteText(ev.Notes))
	}
	if strings.Contains(req, "Pod 指标") {
		t.Errorf("可选来源不该混进 %s（会把环境抖动算成证据缺失）: %s", NoteRequired, req)
	}
}

// TestCollectDegradedBarePod 裸 Pod（无 controller owner）没有上层工作负载这一层：
// 该记 no_data 而不是 required_failed——后者会让模型凭空写一条 missing_evidence 并压置信度，
// 而 Pod spec 里已有全部规格（2026-09-15 真集群首轮跑场景集时踩到并修正）。
func TestCollectDegradedBarePod(t *testing.T) {
	pod := oomPod()
	pod.OwnerReferences = nil
	c := newTestCollector(t, []runtime.Object{pod}, nil, nil, Config{})
	ev, err := c.Collect(context.Background(), "default", "web-0")
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if got := noteText(notesOfKind(ev.Notes, NoteNoData)); !strings.Contains(got, "无 controller owner") {
		t.Errorf("裸 Pod 的工作负载缺失该记成 %s: %s", NoteNoData, noteText(ev.Notes))
	}
	if req := noteText(notesOfKind(ev.Notes, NoteRequired)); strings.Contains(req, "工作负载") {
		t.Errorf("裸 Pod 的工作负载缺失不该混进 %s（会把“没有这一层”算成证据缺失）: %s", NoteRequired, req)
	}
}

// TestLogFailureKinds 验证日志失败的定性：400（容器尚未启动/无上次运行）是"本来就没有"，
// 其余（无权限、超时）才算真失败。
func TestLogFailureKinds(t *testing.T) {
	badRequest := apierrors.NewBadRequest(`container "app" in pod "web-0" is waiting to start: trying and failing to pull image`)
	if got := logFailureKind(fmt.Errorf("read logs default/web-0[app]: %w", badRequest)); got != NoteNoData {
		t.Errorf("容器没起来导致的读日志失败该记成 %s，实际 %s", NoteNoData, got)
	}
	forbidden := apierrors.NewForbidden(schema.GroupResource{Resource: "pods/log"}, "web-0", errors.New("denied"))
	if got := logFailureKind(fmt.Errorf("read logs default/web-0[app]: %w", forbidden)); got != NoteRequired {
		t.Errorf("权限不足导致的读日志失败该记成 %s，实际 %s", NoteRequired, got)
	}
}

// TestFitBudget 验证预算裁剪：先砍低优先级证据并记 Note，Pod 现状永不裁。
func TestFitBudget(t *testing.T) {
	pm := &metricsv1beta1.PodMetrics{
		ObjectMeta: metav1.ObjectMeta{Name: "web-0", Namespace: "default"},
		Timestamp:  metav1.Now(),
		Containers: []metricsv1beta1.ContainerMetrics{{Name: "app", Usage: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("63Mi")}}},
	}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "minikube"},
		Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("4Gi")}}}
	nm := &metricsv1beta1.NodeMetrics{
		ObjectMeta: metav1.ObjectMeta{Name: "minikube"},
		Timestamp:  metav1.Now(),
		Usage:      corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("2Gi")},
	}
	c := newTestCollector(t, []runtime.Object{oomPod(), node}, pm, nm, Config{EvidenceMaxTokens: 200})
	ev, err := c.Collect(context.Background(), "default", "web-0")
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if ev.Pod == nil {
		t.Fatal("Pod 现状被裁掉了（不可裁证据）")
	}
	if ev.NodeMetrics != nil || ev.Node != nil || ev.Metrics != nil {
		t.Errorf("预算不足时未按优先级裁剪: node=%v nodeMetrics=%v metrics=%v", ev.Node, ev.NodeMetrics, ev.Metrics)
	}
	if len(notesOfKind(ev.Notes, NoteTrimmed)) == 0 {
		t.Errorf("裁剪未记 Note（模型无法分辨被裁与本来没有）: %v", ev.Notes)
	}
}

// TestNodeAllocationAndFree 验证节点分配账本：终态 Pod 不计、init 容器按 max 计、
// free = allocatable − requests（Pending 归因的分母）。
func TestNodeAllocationAndFree(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "minikube"},
		Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("4"),
			corev1.ResourceMemory: resource.MustParse("4Gi"),
			corev1.ResourcePods:   resource.MustParse("110"),
		}},
	}
	// 运行中：普通容器 100m/256Mi，init 容器 500m/128Mi → 每资源取 max = 500m / 256Mi
	running := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "busy", Namespace: "default"},
		Spec: corev1.PodSpec{
			NodeName: "minikube",
			InitContainers: []corev1.Container{{Name: "init", Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("128Mi")},
			}}},
			Containers: []corev1.Container{{Name: "app", Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("256Mi")},
			}}},
		},
	}
	// 已终结：不再占资源，不该计入
	done := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "job-done", Namespace: "default"},
		Spec: corev1.PodSpec{NodeName: "minikube", Containers: []corev1.Container{{Name: "c", Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("2Gi")},
		}}}},
		Status: corev1.PodStatus{Phase: corev1.PodSucceeded},
	}
	c := newTestCollector(t, []runtime.Object{node, running, done}, nil, nil, Config{})
	nv, err := c.Node(context.Background(), "minikube")
	if err != nil {
		t.Fatalf("Node: %v", err)
	}
	if nv.PodsOnNode != 1 {
		t.Errorf("终态 Pod 不应计入，PodsOnNode=%d", nv.PodsOnNode)
	}
	if nv.RequestsOnNode["cpu"] != "500m" || nv.RequestsOnNode["memory"] != "256Mi" {
		t.Errorf("requests 汇总应为 max(容器之和, init 最大): %v", nv.RequestsOnNode)
	}
	if nv.FreeOnNode["cpu"] != "3500m" || nv.FreeOnNode["memory"] != "3840Mi" {
		t.Errorf("free 应为 allocatable − requests: %v", nv.FreeOnNode)
	}
	// pods 是个数配额：free 要扣"已用 Pod 数"（1 个），而不是 requests 合计（那边恒为 0）。
	if nv.FreeOnNode["pods"] != "109" {
		t.Errorf("free_on_node 的 pods 项应按已用 Pod 数扣（110−1=109），实际: %q", nv.FreeOnNode["pods"])
	}
	if nv.AllocationError != "" {
		t.Errorf("不该有降级说明: %s", nv.AllocationError)
	}
}

// TestNodes 验证"列全部节点"这条路径（Pending 归因的证据面）：账本要按节点分组算，
// 未调度的 Pod（Pending、nodeName 为空）不属于任何节点，终态 Pod 同样不计；
// 列 Pod 失败时降级必须可见（节点自身的标签/taints/allocatable 仍是有效证据）。
func TestNodes(t *testing.T) {
	nodeA := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-a"},
		Status:     corev1.NodeStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4")}},
	}
	// 标签是 nodeSelector 归因的判据：Pending 的 Pod 没有 node_name，只能靠列节点看这一侧。
	nodeB := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-b", Labels: map[string]string{"lab-node-role": "worker"}},
		Status:     corev1.NodeStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")}},
	}
	onB := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "on-b", Namespace: "default"},
		Spec: corev1.PodSpec{NodeName: "node-b", Containers: []corev1.Container{{
			Name:      "c",
			Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")}},
		}}},
	}
	unscheduled := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "waiting", Namespace: "default"}}
	c := newTestCollector(t, []runtime.Object{nodeB, nodeA, onB, unscheduled}, nil, nil, Config{})
	views, err := c.Nodes(context.Background())
	if err != nil {
		t.Fatalf("Nodes: %v", err)
	}
	if len(views) != 2 || views[0].Name != "node-a" || views[1].Name != "node-b" {
		t.Fatalf("节点列表应按名称排序且不重不漏: %+v", views)
	}
	if views[0].PodsOnNode != 0 {
		t.Errorf("未调度的 Pod 不该算在节点上: %+v", views[0])
	}
	if views[1].Labels["lab-node-role"] != "worker" {
		t.Errorf("节点标签缺失（nodeSelector 归因的判据）: %+v", views[1].Labels)
	}
	if views[1].PodsOnNode != 1 || views[1].RequestsOnNode["cpu"] != "500m" || views[1].FreeOnNode["cpu"] != "1500m" {
		t.Errorf("node-b 账本不对: pods=%d requests=%v free=%v", views[1].PodsOnNode, views[1].RequestsOnNode, views[1].FreeOnNode)
	}

	// Pod 列不出来：节点摘要照旧返回，但每条的 allocation_error 都要写出——空余量不能被当成"没有占用"。
	c2 := newTestCollector(t, []runtime.Object{nodeA, nodeB, onB}, nil, nil, Config{})
	c2.core.(*corefake.Clientset).PrependReactor("list", "pods",
		func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, errors.New("boom") })
	vs, err := c2.Nodes(context.Background())
	if err != nil {
		t.Fatalf("Nodes（降级路径）: %v", err)
	}
	if len(vs) != 2 {
		t.Fatalf("降级时仍应返回全部节点: %+v", vs)
	}
	for _, v := range vs {
		if v.AllocationError == "" {
			t.Errorf("%s 缺 allocation_error（余量不可信必须显式可见）: %+v", v.Name, v)
		}
	}
}

// TestPodViewPullSecrets 验证拉取凭据来自两处（Pod spec 与 ServiceAccount），
// 且 SA 读不到时必须留下原因——空列表不能被当成"没配凭据"。
func TestPodViewPullSecrets(t *testing.T) {
	pod := oomPod()
	pod.Spec.ServiceAccountName = "default"
	pod.Spec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: "pod-secret"}}
	sa := &corev1.ServiceAccount{
		ObjectMeta:       metav1.ObjectMeta{Name: "default", Namespace: "default"},
		ImagePullSecrets: []corev1.LocalObjectReference{{Name: "sa-secret"}},
	}

	c := newTestCollector(t, []runtime.Object{pod, sa}, nil, nil, Config{})
	pv, err := c.Pod(context.Background(), "default", "web-0")
	if err != nil {
		t.Fatalf("Pod: %v", err)
	}
	if len(pv.ImagePullSecrets) != 1 || pv.ImagePullSecrets[0] != "pod-secret" {
		t.Errorf("Pod spec 上的凭据丢了: %v", pv.ImagePullSecrets)
	}
	if len(pv.ServiceAccountPullSecrets) != 1 || pv.ServiceAccountPullSecrets[0] != "sa-secret" {
		t.Errorf("ServiceAccount 上的凭据丢了: %v", pv.ServiceAccountPullSecrets)
	}
	if pv.ServiceAccountReadError != "" {
		t.Errorf("不该有读失败: %s", pv.ServiceAccountReadError)
	}

	// SA 不存在（或权限不足）：列表为空但必须给出原因。
	c2 := newTestCollector(t, []runtime.Object{pod}, nil, nil, Config{})
	pv2, err := c2.Pod(context.Background(), "default", "web-0")
	if err != nil {
		t.Fatalf("Pod: %v", err)
	}
	if len(pv2.ServiceAccountPullSecrets) != 0 || pv2.ServiceAccountReadError == "" {
		t.Errorf("SA 读不到时应留下原因: %+v", pv2)
	}
}

// TestCollectPVCs 验证子对象采集：Pod 引用的 claim 逐个取回（非 PVC 卷不掺进来）、
// 引用它的卷名回填、claim 自己的事件单独取一次。
// 注意 fake clientset 不实现 field selector，事件过滤只在真集群生效（见 lab 测试）。
func TestCollectPVCs(t *testing.T) {
	pod := oomPod()
	pod.Spec.Volumes = []corev1.Volume{
		{Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data-claim"}}},
		{Name: "logs", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data-claim"}}},
		{Name: "cfg", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "app-config"}}}},
		{Name: "scratch", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "scratch-claim"}}},
	}
	sc := "standard"
	bound := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "data-claim", Namespace: "default", UID: "pvc-1"},
		Spec: corev1.PersistentVolumeClaimSpec{
			VolumeName:       "pv-1",
			StorageClassName: &sc,
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
			},
		},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
	// 未绑定这块不写 storageClassName：正是"没有现成 PV、又没配 storageClass"的形态，
	// 与上面那块（有 storageClass）构成 phase + storage_class 的两组组合。
	pending := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "scratch-claim", Namespace: "default", UID: "pvc-2"},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("512Mi")},
			},
		},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimPending},
	}
	evt := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "scratch-claim.1", Namespace: "default"},
		InvolvedObject: corev1.ObjectReference{Kind: "PersistentVolumeClaim", Name: "scratch-claim", UID: "pvc-2"},
		Type:           corev1.EventTypeWarning,
		Reason:         "ProvisioningFailed",
		Message:        "no persistent volumes available for this claim and no storage class is set",
		Source:         corev1.EventSource{Component: "persistentvolume-controller"},
		Count:          4,
		LastTimestamp:  metav1.Now(),
	}

	c := newTestCollector(t, []runtime.Object{pod, bound, pending, evt}, nil, nil, Config{})
	ev, err := c.Collect(context.Background(), "default", "web-0")
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(ev.PVCs) != 2 {
		t.Fatalf("应取回两个 claim（configMap 卷不算）: %+v", ev.PVCs)
	}
	got := ev.PVCs[0]
	if got.Name != "data-claim" || got.Phase != "Bound" {
		t.Errorf("首个 claim 应为 data-claim/Bound（顺序要按卷列表，证据包要可对照）: %+v", got)
	}
	if got.VolumeName != "pv-1" || got.StorageClass != "standard" || got.RequestedStorage != "1024Mi" {
		t.Errorf("绑定信息不全: %+v", got)
	}
	if len(got.AccessModes) != 1 || got.AccessModes[0] != "ReadWriteOnce" {
		t.Errorf("access_modes 丢了: %+v", got)
	}
	// 同一块 claim 被两个卷引用：两个卷名都要在。
	if strings.Join(got.UsedByVolumes, ",") != "data,logs" {
		t.Errorf("引用关系不对: %v", got.UsedByVolumes)
	}
	pend := ev.PVCs[1]
	if pend.Name != "scratch-claim" || pend.Phase != "Pending" || pend.StorageClass != "" {
		t.Errorf("未绑定 claim 的定性字段不对: %+v", pend)
	}
	if pend.RequestedStorage != "512Mi" {
		t.Errorf("申请容量不对: %+v", pend)
	}
	var warned bool
	for _, e := range pend.Events {
		if e.Reason == "ProvisioningFailed" && e.Count == 4 {
			warned = true
		}
	}
	if !warned {
		t.Errorf("claim 自己的事件没取到: %+v", pend.Events)
	}
	if pend.EventsError != "" {
		t.Errorf("不该有事件读取失败: %s", pend.EventsError)
	}
	if notes := noteText(notesOfKind(ev.Notes, NoteRequired)); strings.Contains(notes, "PVC") {
		t.Errorf("取到证据时不该有 PVC 降级说明: %s", notes)
	}
}

// TestCollectPVCDegraded 验证两处降级可见：claim 取不到不能静默少一项，
// 事件读失败不能表现成"没有事件"。
func TestCollectPVCDegraded(t *testing.T) {
	pod := oomPod()
	pod.Spec.Volumes = []corev1.Volume{{
		Name:         "ghost",
		VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "ghost-claim"}},
	}}
	c := newTestCollector(t, []runtime.Object{pod}, nil, nil, Config{})
	ev, err := c.Collect(context.Background(), "default", "web-0")
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(ev.PVCs) != 0 {
		t.Fatalf("claim 不存在时应留空: %+v", ev.PVCs)
	}
	notes := noteText(ev.Notes)
	if !strings.Contains(notes, "PVC ghost-claim 未取到") {
		t.Errorf("claim 取不到必须记 Note（空与不存在是两回事）: %s", notes)
	}
	// 且要归到"必需来源失败"：Pod 引用的存储证据缺失，结论得少一分底气。
	if !strings.Contains(noteText(notesOfKind(ev.Notes, NoteRequired)), "PVC ghost-claim") {
		t.Errorf("claim 取不到该记成 %s: %s", NoteRequired, notes)
	}

	// 事件列不出来（权限/连接问题）：PVC 摘要仍在，但必须带 events_error。
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "ghost-claim", Namespace: "default", UID: "pvc-9"},
		Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimPending},
	}
	c2 := newTestCollector(t, []runtime.Object{pod, pvc}, nil, nil, Config{})
	c2.core.(*corefake.Clientset).PrependReactor("list", "events",
		func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, errors.New("boom") })
	ev2, err := c2.Collect(context.Background(), "default", "web-0")
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(ev2.PVCs) != 1 || ev2.PVCs[0].Phase != "Pending" {
		t.Fatalf("事件失败不该影响 PVC 摘要: %+v", ev2.PVCs)
	}
	if ev2.PVCs[0].EventsError == "" {
		t.Errorf("事件读失败必须留原因（Events 为空不等于没有事件）: %+v", ev2.PVCs[0])
	}
	if len(ev2.PVCs[0].Events) != 0 {
		t.Errorf("事件失败时不该有事件: %+v", ev2.PVCs[0].Events)
	}
}

// TestEnvRefs 验证 env 只落"名与来源"：明文值不入证据（值可能是机密），
// 四种 valueFrom 各按来源渲染。
func TestEnvRefs(t *testing.T) {
	envs := []corev1.EnvVar{
		{Name: "PLAIN", Value: "s3cr3t-value"},
		{Name: "FROM_CM", ValueFrom: &corev1.EnvVarSource{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "app-config"}, Key: "log_level"}}},
		{Name: "FROM_SECRET", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "db-secret"}, Key: "password"}}},
		{Name: "POD_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}}},
		{Name: "MEM_LIMIT", ValueFrom: &corev1.EnvVarSource{ResourceFieldRef: &corev1.ResourceFieldSelector{Resource: "limits.memory"}}},
	}
	got := strings.Join(envRefs(envs), "\n")
	for _, want := range []string{
		"PLAIN",
		"FROM_CM<-configMapRef/app-config",
		"FROM_SECRET<-secretRef/db-secret",
		"POD_NAME<-fieldRef/metadata.name",
		"MEM_LIMIT<-resourceFieldRef/limits.memory",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("缺少 %q: %s", want, got)
		}
	}
	if strings.Contains(got, "s3cr3t-value") {
		t.Errorf("明文值不该进证据: %s", got)
	}

	// 容器视图接线（工作负载模板复用同一个转换函数）。
	cv := toContainerView(&corev1.Container{Name: "app", Env: envs}, nil)
	if len(cv.Env) != len(envs) {
		t.Errorf("ContainerView.Env 没接上: %+v", cv.Env)
	}
}

// TestAffinityView 验证亲和性摘要：硬（required）与软（preferred）分层、topologyKey 与选择器齐全、
// 没配就不占字段，且 PodView 与 PodTemplateView 两处都接上。
func TestAffinityView(t *testing.T) {
	aff := &corev1.Affinity{
		NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{{
					MatchExpressions: []corev1.NodeSelectorRequirement{
						{Key: "kubernetes.io/os", Operator: corev1.NodeSelectorOpIn, Values: []string{"linux"}},
						{Key: "node-role.kubernetes.io/worker", Operator: corev1.NodeSelectorOpExists},
					},
				}},
			},
			PreferredDuringSchedulingIgnoredDuringExecution: []corev1.PreferredSchedulingTerm{{
				Weight: 50,
				Preference: corev1.NodeSelectorTerm{MatchExpressions: []corev1.NodeSelectorRequirement{
					{Key: "topology.kubernetes.io/zone", Operator: corev1.NodeSelectorOpIn, Values: []string{"az-a"}},
				}},
			}},
		},
		PodAntiAffinity: &corev1.PodAntiAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
				TopologyKey:   "kubernetes.io/hostname",
				LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web", "tier": "frontend"}},
				Namespaces:    []string{"default"},
			}},
		},
	}
	v := affinityView(aff)
	if v == nil || v.NodeAffinity == nil || v.PodAntiAffinity == nil {
		t.Fatalf("亲和性摘要缺失: %+v", v)
	}
	if v.PodAffinity != nil {
		t.Errorf("没配 podAffinity 不该造出空壳: %+v", v.PodAffinity)
	}
	if req := v.NodeAffinity.Required; len(req) != 1 ||
		!strings.Contains(req[0], "kubernetes.io/os In [linux]") || !strings.Contains(req[0], " AND ") {
		t.Errorf("硬性节点条件渲染不对（term 内多条件是 AND）: %v", req)
	}
	if pref := v.NodeAffinity.Preferred; len(pref) != 1 || !strings.HasPrefix(pref[0], "weight=50: ") {
		t.Errorf("软性偏好要带 weight（硬软分层是这项的全部意义）: %v", pref)
	}
	anti := v.PodAntiAffinity.Required
	if len(anti) != 1 {
		t.Fatalf("反亲和条件缺失: %+v", v.PodAntiAffinity)
	}
	// 选择器 map 要排序（顺序不稳定则证据包无法逐字对照），topologyKey 必须在。
	if !strings.Contains(anti[0], "topologyKey=kubernetes.io/hostname") ||
		!strings.Contains(anti[0], "pod app=web,tier=frontend") {
		t.Errorf("反亲和条件渲染不对（选择器要按 key 排序、topologyKey 要留）: %s", anti[0])
	}
	if !strings.Contains(anti[0], "namespaces=[default]") {
		t.Errorf("命名空间范围丢了: %s", anti[0])
	}
	if affinityView(nil) != nil {
		t.Error("未配置 affinity 应返回 nil（不占字段）")
	}

	// 两处接线：Pod 现状是"实际生效的约束"，工作负载模板是"发布配置真源"。
	pod := oomPod()
	pod.Spec.Affinity = aff
	c := newTestCollector(t, []runtime.Object{pod}, nil, nil, Config{})
	pv, err := c.Pod(context.Background(), "default", "web-0")
	if err != nil {
		t.Fatalf("Pod: %v", err)
	}
	if pv.Affinity == nil || pv.Affinity.NodeAffinity == nil {
		t.Errorf("PodView 没接上 affinity: %+v", pv.Affinity)
	}
	tpl := toPodTemplate(&corev1.PodTemplateSpec{Spec: corev1.PodSpec{Affinity: aff}})
	if tpl.Affinity == nil || tpl.Affinity.PodAntiAffinity == nil {
		t.Errorf("PodTemplateView 没接上 affinity: %+v", tpl.Affinity)
	}
}

// TestWorkloadJobSemantics 验证 Job 的语义修正：Completions/Parallelism 不再冒充 replicas，
// 进度与失败策略落在 job 块里（重试类失败要能对上事件里的 BackoffLimitExceeded）。
func TestWorkloadJobSemantics(t *testing.T) {
	pod := oomPod()
	pod.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "batch/v1", Kind: "Job", Name: "batch-1", Controller: boolPtr(true),
	}}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "batch-1", Namespace: "default"},
		Spec: batchv1.JobSpec{
			Completions:  int32Ptr(3),
			Parallelism:  int32Ptr(2),
			BackoffLimit: int32Ptr(4),
		},
		Status: batchv1.JobStatus{
			Active: 1, Succeeded: 2, Failed: 1,
			Conditions: []batchv1.JobCondition{{
				Type: batchv1.JobFailed, Status: corev1.ConditionTrue,
				Reason: "BackoffLimitExceeded", Message: "Job has reached the specified backoff limit",
			}},
		},
	}
	c := newTestCollector(t, []runtime.Object{pod, job}, nil, nil, Config{})
	w, err := c.WorkloadOf(context.Background(), "default", "web-0")
	if err != nil {
		t.Fatalf("WorkloadOf: %v", err)
	}
	if w.Kind != "Job" || w.Job == nil {
		t.Fatalf("Job 视图缺失: %+v", w)
	}
	if w.Replicas != 0 {
		t.Errorf("Job 没有副本数语义，不该填: %d", w.Replicas)
	}
	// 序列化后也不该出现 replicas 字段（omitempty 掉模型才看不到）——这才是"修好了"的判定。
	raw, err := json.Marshal(w)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), `"replicas"`) {
		t.Errorf("Job 的 JSON 里仍有 replicas（会被读成副本数）: %s", raw)
	}
	j := w.Job
	if j.Completions == nil || *j.Completions != 3 || j.Parallelism == nil || *j.Parallelism != 2 {
		t.Errorf("completions/parallelism 不对: %+v", j)
	}
	if j.Active != 1 || j.Succeeded != 2 || j.Failed != 1 {
		t.Errorf("进度计数不对: %+v", j)
	}
	if j.BackoffLimit == nil || *j.BackoffLimit != 4 {
		t.Errorf("backoff_limit 丢了（要与 BackoffLimitExceeded 对得上）: %+v", j)
	}
	if len(j.Conditions) != 1 || j.Conditions[0].Type != "Failed" || j.Conditions[0].Reason != "BackoffLimitExceeded" {
		t.Errorf("Job conditions 没接上: %+v", j.Conditions)
	}

	// 未指定与"写了 0"必须能分开：前者留空（k8s 按 1 处理），后者是真实计数。
	job.Spec.Completions, job.Spec.Parallelism = nil, nil
	job.Status.Active, job.Status.Succeeded, job.Status.Failed = 0, 0, 0
	c2 := newTestCollector(t, []runtime.Object{pod, job}, nil, nil, Config{})
	w2, err := c2.WorkloadOf(context.Background(), "default", "web-0")
	if err != nil {
		t.Fatalf("WorkloadOf: %v", err)
	}
	if w2.Job.Completions != nil || w2.Job.Parallelism != nil {
		t.Errorf("未指定该留空，不能填 0: %+v", w2.Job)
	}
	raw2, err := json.Marshal(w2.Job)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw2), `"failed":0`) {
		t.Errorf("计数为 0 要显式出现（省掉会被读成没采到）: %s", raw2)
	}
}

// TestWorkloadConditions 验证 StatefulSet / DaemonSet 的 conditions 都接上（原因常写在 Reason 里）。
func TestWorkloadConditions(t *testing.T) {
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "default"},
		Status: appsv1.StatefulSetStatus{Conditions: []appsv1.StatefulSetCondition{{
			// 这两类没有导出的 condition 常量（apps/v1 只定义了类型），字面量即 API 里的取值。
			Type: "ReplicaFailure", Status: corev1.ConditionTrue,
			Reason: "FailedCreate", Message: "pods \"db-0\" is forbidden: exceeded quota",
		}}},
	}
	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "default"},
		Status: appsv1.DaemonSetStatus{Conditions: []appsv1.DaemonSetCondition{{
			Type: "ReplicaFailure", Status: corev1.ConditionTrue,
			Reason: "FailedCreate", Message: "pods \"agent-x\" is forbidden",
		}}},
	}
	c := newTestCollector(t, []runtime.Object{sts, ds}, nil, nil, Config{})
	sv, err := c.statefulSet(context.Background(), "default", "db")
	if err != nil {
		t.Fatalf("statefulSet: %v", err)
	}
	if len(sv.Conditions) != 1 || sv.Conditions[0].Reason != "FailedCreate" || sv.Conditions[0].Message == "" {
		t.Errorf("StatefulSet conditions 没接上: %+v", sv.Conditions)
	}
	dv, err := c.daemonSet(context.Background(), "default", "agent")
	if err != nil {
		t.Fatalf("daemonSet: %v", err)
	}
	if len(dv.Conditions) != 1 || dv.Conditions[0].Reason != "FailedCreate" {
		t.Errorf("DaemonSet conditions 没接上: %+v", dv.Conditions)
	}
}

// TestSaveArtifacts 验证产物落盘：命名规则、内容可反序列化、路径清洗防注入。
func TestSaveArtifacts(t *testing.T) {
	dir := t.TempDir()
	c := newTestCollector(t, []runtime.Object{oomPod()}, nil, nil, Config{Dir: dir})
	ev, err := c.Collect(context.Background(), "default", "web-0")
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	path, err := c.Save(ev)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if filepath.Base(path) != "default-web-0.evidence.json" {
		t.Errorf("证据包命名不对: %s", path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读回产物: %v", err)
	}
	var back Evidence
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("产物不是合法 JSON: %v", err)
	}
	if back.Pod == nil || back.Pod.Containers[0].LastStateReason != "OOMKilled" {
		t.Errorf("落盘内容不完整: %+v", back.Pod)
	}
	jp, mp, err := c.SaveReport(ev.Target, []byte(`{"schema_version":1}`), "# 报告\n")
	if err != nil {
		t.Fatalf("SaveReport: %v", err)
	}
	if filepath.Base(jp) != "default-web-0.report.json" || filepath.Base(mp) != "default-web-0.report.md" {
		t.Errorf("报告命名不对: %s / %s", jp, mp)
	}
}

// TestSafeName 验证文件名清洗（目标名来自用户输入，必须防路径注入）。
func TestSafeName(t *testing.T) {
	cases := map[string]string{
		"web-0":            "web-0",
		"../../etc/passwd": "etc-passwd",
		"a/b c":            "a-b-c",
		"":                 "unknown",
		"..":               "unknown",
	}
	for in, want := range cases {
		if got := safeName(in); got != want {
			t.Errorf("safeName(%q) = %q, 期望 %q", in, got, want)
		}
	}
}

// TestFormatHelpers 验证单位换算与百分比（模型看到的是这些字符串，错了会直接误导归因）。
func TestFormatHelpers(t *testing.T) {
	if got := fmtCPU(resource.MustParse("1")); got != "1" {
		t.Errorf("fmtCPU(1) = %q", got)
	}
	if got := fmtCPU(resource.MustParse("250m")); got != "250m" {
		t.Errorf("fmtCPU(250m) = %q", got)
	}
	if got := fmtMemory(resource.MustParse("64Mi")); got != "64Mi" {
		t.Errorf("fmtMemory(64Mi) = %q", got)
	}
	if got := fmtMemory(resource.MustParse("1000")); got != "1000B" {
		t.Errorf("fmtMemory(1000) = %q", got)
	}
	p := percent(resource.MustParse("63Mi"), resource.MustParse("64Mi"))
	if p == nil || *p < 98 || *p > 99 {
		t.Errorf("percent(63Mi/64Mi) = %v", p)
	}
	if p := percent(resource.MustParse("1Gi"), resource.Quantity{}); p != nil {
		t.Errorf("无 limit 应返回 nil: %v", p)
	}
}

// TestTailAndCountLines 验证截断取尾部与行数统计（截断方向错了会丢掉故障现场）。
func TestTailAndCountLines(t *testing.T) {
	if got := tail("aaa\nbbb\nccc\n", 8); got != "ccc\n" {
		t.Errorf("tail 未对齐行首或未取尾部: %q", got)
	}
	if got := countLines("a\nb"); got != 2 {
		t.Errorf("countLines = %d", got)
	}
	if got := countLines(""); got != 0 {
		t.Errorf("countLines(空) = %d", got)
	}
}

// boolPtr 造 bool 指针（OwnerReference.Controller 是可选字段）。
func boolPtr(b bool) *bool { return &b }

func int32Ptr(v int32) *int32 { return &v }

// noteText 把降级说明拍平成可搜文本（断言用，带类别前缀便于定位）。
func noteText(notes []NoteView) string {
	var b strings.Builder
	for _, n := range notes {
		b.WriteString(string(n.Kind) + ": " + n.Message + "\n")
	}
	return b.String()
}

// notesOfKind 按类别取降级说明——分级就是为了让断言能"对必需严格、对可选宽容"。
func notesOfKind(notes []NoteView, kind NoteKind) []NoteView {
	var out []NoteView
	for _, n := range notes {
		if n.Kind == kind {
			out = append(out, n)
		}
	}
	return out
}

// TestCallCtxBudgets 超时预算的接线：元数据调用用配置里的 Timeout，显式传的用传进来的
// （日志走这条，值更大），传 0/负值回落 defaultCallTimeout——不能出现"没预算 = 永不超时"。
// 为什么测这个而不是测"日志读真的等更久"：deadline 挂在 ctx 上，fake clientset 走的是
// in-process 调用、不认 ctx 取消（连 Stream 都只是把 reactor 的结果包成响应），
// 行为级断言在这里做不出来，只能锁预算本身；真实传输语义留给真集群冒烟。
func TestCallCtxBudgets(t *testing.T) {
	c := newTestCollector(t, nil, nil, nil, Config{Timeout: 5 * time.Second})
	cases := []struct {
		name string
		call func(ctx context.Context) (context.Context, context.CancelFunc)
		want time.Duration
	}{
		{"元数据调用用 cfg.Timeout", c.callCtx, 5 * time.Second},
		{"显式预算按传入值（日志走这条）", func(ctx context.Context) (context.Context, context.CancelFunc) {
			return c.callCtxFor(ctx, logCallTimeout)
		}, logCallTimeout},
		{"零值先回落配置里的预算", func(ctx context.Context) (context.Context, context.CancelFunc) {
			return c.callCtxFor(ctx, 0)
		}, 5 * time.Second},
		{"手搓 Collector（配置也没配）时回落包内默认", func(ctx context.Context) (context.Context, context.CancelFunc) {
			return (&Collector{}).callCtxFor(ctx, 0)
		}, defaultCallTimeout},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := tc.call(context.Background())
			defer cancel()
			dl, ok := ctx.Deadline()
			if !ok {
				t.Fatalf("没有 deadline：等于永不超时")
			}
			if got := time.Until(dl); got > tc.want || got < tc.want-time.Second {
				t.Errorf("预算 %v，期望约 %v", got, tc.want)
			}
		})
	}
	if logCallTimeout <= defaultCallTimeout {
		t.Errorf("日志预算该比元数据预算宽：log=%v default=%v", logCallTimeout, defaultCallTimeout)
	}
}

// TestReadOnlyGuard 只读守卫：真跑一遍采集，断言实际发出的每个 API 动作都是读语义。
//
// 为什么需要它（设计文档 §2 承诺"严格只读"）：grep 一遍"没有写 verb"是一次性结论，不是不变量——
// 将来谁在采集路径里加一句 Update/Patch，评审未必看得见。这里给两个 fake clientset 各装一个记录型
// reactor，把每个 action 的 verb/resource/subresource 收下来做白名单校验：验的不是"源码里没写"，
// 而是"跑起来也确实只读了"。
//
// 覆盖范围：Collect 依次走 Pod → workload → events → PVC → logs → metrics → node → node metrics，
// 再把本包唯一没被它触发的 Nodes() 补调一次（它只服务 Pending 归因的 k8s_nodes），于是所有会碰
// 客户端的导出方法都在这条路径上。覆盖不到的：reactor 只拦得住 fake clientset，绕过 typed client
// 的裸 HTTP 不在其内（包内没有这类代码，靠静态审计兜）。
func TestReadOnlyGuard(t *testing.T) {
	pod := oomPod()
	pod.Spec.NodeName = "node-1"
	pod.Spec.ServiceAccountName = "default"
	pod.Spec.Volumes = []corev1.Volume{{
		Name:         "data",
		VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data-claim"}},
	}}
	objs := []runtime.Object{
		pod,
		&appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
			Name: "web-abc", Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "Deployment", Name: "web", Controller: boolPtr(true),
			}},
		}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"}},
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "default"}},
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "data-claim", Namespace: "default", UID: "pvc-1"}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}},
	}
	podMetrics := &metricsv1beta1.PodMetrics{ObjectMeta: metav1.ObjectMeta{Name: "web-0", Namespace: "default"}}
	nodeMetrics := &metricsv1beta1.NodeMetrics{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}}

	var mu sync.Mutex
	verbs, resources := map[string]bool{}, map[string]bool{}
	logRead := false
	record := func(a k8stesting.Action) {
		mu.Lock()
		defer mu.Unlock()
		verbs[a.GetVerb()] = true
		resources[a.GetResource().Resource] = true
		if a.GetSubresource() == "log" {
			logRead = true
		}
	}

	core := corefake.NewSimpleClientset(objs...)
	// Prepend 是往队首插，所以记录器要最后 Prepend 才会最先执行；返回 handled=false 让动作继续
	// 落到默认 tracker（fake 的 tracker 支持 Get/List，写动作会在这里被记下来）。
	core.PrependReactor("*", "*", func(a k8stesting.Action) (bool, runtime.Object, error) {
		record(a)
		if a.GetSubresource() == "log" {
			// 日志是流式读，默认 reactor 给不出正文；给一段假日志，让这条分支也真跑一遍。
			return true, &runtime.Unknown{Raw: []byte("boom\n")}, nil
		}
		return false, nil, nil
	})

	mc := metricsfake.NewSimpleClientset()
	mc.PrependReactor("get", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, podMetrics, nil
	})
	mc.PrependReactor("get", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nodeMetrics, nil
	})
	mc.PrependReactor("*", "*", func(a k8stesting.Action) (bool, runtime.Object, error) {
		record(a)
		return false, nil, nil
	})

	c := newWithClients(Config{Dir: t.TempDir()}, core, mc)
	if _, err := c.Collect(context.Background(), "default", "web-0"); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if _, err := c.Nodes(context.Background()); err != nil {
		t.Fatalf("Nodes: %v", err)
	}

	// 1) 白名单：只放行读语义动作。watch 也不放行——本模块刻意用一次性拉取（见 §4.4），要引入
	//    长连接得先来改这条断言，守卫的意义就是让这类改动必须显式发生。
	for v := range verbs {
		if v != "get" && v != "list" {
			t.Errorf("采集路径出现了非只读动作 %q——严格只读是硬承诺，写操作必须离开这条链路", v)
		}
	}
	// 2) 反向自检：记录器必须真收到东西，否则上一条是空转的假绿。
	for _, res := range []string{"pods", "events", "nodes", "serviceaccounts", "persistentvolumeclaims", "replicasets", "deployments"} {
		if !resources[res] {
			t.Errorf("没记录到对 %s 的读——对应采集分支没走到，守卫形同虚设", res)
		}
	}
	if !logRead {
		t.Errorf("没记录到 pods/log 的读——日志分支没走到")
	}
}
