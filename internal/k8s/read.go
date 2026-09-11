package k8s

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// 采集方法：一类证据一个方法，全部只读（Get/List/读日志）。
// 参数与裁剪默认值在方法内收敛，工具壳只负责解码参数后转调这里。

// LogQuery 日志读取参数（零值即默认：尾部 200 行、64KB、当前容器的日志）。
type LogQuery struct {
	Container  string
	Previous   bool
	TailLines  int
	LimitBytes int64
}

func (c *Collector) Pod(ctx context.Context, ns, name string) (*PodView, error) {
	ctx, cancel := c.callCtx(ctx)
	defer cancel()
	p, err := c.core.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get pod %s/%s: %w", ns, name, err)
	}
	v := toPodView(p)
	// 拉取凭据要合两处看：Pod spec 上的 imagePullSecrets 与 ServiceAccount 上的（1.24 起后者
	// 不再复制进 Pod spec，但 kubelet 拉镜像时同样生效）。SA 读不到时把原因写进视图——
	// 空列表不能被当成"没配凭据"，否则 ImagePullBackOff 会被误判成"私有仓库缺 secret"。
	if p.Spec.ServiceAccountName != "" {
		sa, err := c.core.CoreV1().ServiceAccounts(ns).Get(ctx, p.Spec.ServiceAccountName, metav1.GetOptions{})
		if err != nil {
			v.ServiceAccountReadError = err.Error()
		} else {
			for _, s := range sa.ImagePullSecrets {
				v.ServiceAccountPullSecrets = append(v.ServiceAccountPullSecrets, s.Name)
			}
		}
	}
	return &v, nil
}

// WorkloadOf 由 Pod 的 owner 链上溯到顶层工作负载并读其规格摘要。
// ReplicaSet 只是中间层（规格真源在 Deployment），故再上溯一级；裸 Pod 直接报错。
func (c *Collector) WorkloadOf(ctx context.Context, ns, pod string) (*WorkloadView, error) {
	// 这一次 Get 用带超时的子 ctx 并立即释放；后续上溯调用必须用原始 ctx——
	// 若把子 ctx 传下去又在前面 cancel，会把还在用的 ctx 一起取消（真集群冒烟时踩到过）。
	pctx, cancel := c.callCtx(ctx)
	p, err := c.core.CoreV1().Pods(ns).Get(pctx, pod, metav1.GetOptions{})
	cancel()
	if err != nil {
		return nil, fmt.Errorf("get pod %s/%s: %w", ns, pod, err)
	}
	ref := controllerRef(p.OwnerReferences)
	if ref == nil {
		return nil, fmt.Errorf("pod %s/%s 无 controller owner（裸 Pod 或未受管）", ns, pod)
	}
	switch ref.Kind {
	case "ReplicaSet":
		rs, err := c.replicaSet(ctx, ns, ref.Name)
		if err != nil {
			return nil, err
		}
		if dr := controllerRefView(rs.OwnerRefs); dr != nil && dr.Kind == "Deployment" {
			return c.deployment(ctx, ns, dr.Name)
		}
		return rs, nil
	case "Deployment":
		return c.deployment(ctx, ns, ref.Name)
	case "StatefulSet":
		return c.statefulSet(ctx, ns, ref.Name)
	case "DaemonSet":
		return c.daemonSet(ctx, ns, ref.Name)
	case "Job":
		return c.job(ctx, ns, ref.Name)
	}
	return nil, fmt.Errorf("暂不支持的工作负载类型 %s（pod %s/%s）", ref.Kind, ns, pod)
}

// Events 读事件摘要：服务端按 involvedObject.name 过滤，再做时间窗过滤、
// 去重合并（type+reason+message 相同者合并计数）与排序（Warning 优先、时间倒序），
// 最后截断到 limit 条。Warning 优先保证截断不会先丢掉最关键的告警。
func (c *Collector) Events(ctx context.Context, ns, name, uid string, since time.Duration, limit int) ([]EventView, error) {
	if since <= 0 {
		since = defaultEventSince
	}
	if limit <= 0 {
		limit = defaultEventLimit
	}
	ctx, cancel := c.callCtx(ctx)
	defer cancel()
	selector := "involvedObject.name=" + name
	if uid != "" {
		selector = "involvedObject.uid=" + uid
	}
	list, err := c.core.CoreV1().Events(ns).List(ctx, metav1.ListOptions{FieldSelector: selector})
	if err != nil {
		return nil, fmt.Errorf("list events %s/%s: %w", ns, name, err)
	}

	cutoff := time.Now().Add(-since)
	type agg struct {
		view  EventView
		first time.Time
		last  time.Time
	}
	var order []string
	byKey := map[string]*agg{}
	for i := range list.Items {
		e := &list.Items[i]
		last := eventTime(e)
		if !last.IsZero() && last.Before(cutoff) {
			continue
		}
		key := e.Type + "|" + e.Reason + "|" + e.Message
		a, ok := byKey[key]
		if !ok {
			a = &agg{view: EventView{
				Type:    e.Type,
				Reason:  e.Reason,
				Message: e.Message,
				Source:  e.Source.Component,
				Object:  e.InvolvedObject.Kind + "/" + e.InvolvedObject.Name,
			}}
			byKey[key] = a
			order = append(order, key)
		}
		// 旧版事件可能不带 count：逐条按"至少 1 次"累加。否则两条 count=0 的同源事件
		// 会被合并成"只发生过 1 次"——这是误导性证据（模型会低估重复次数）。
		n := e.Count
		if n == 0 {
			n = 1
		}
		a.view.Count += n
		if a.view.Source == "" {
			a.view.Source = e.Source.Component
		}
		first := e.FirstTimestamp.Time
		if first.IsZero() {
			first = last
		}
		if a.first.IsZero() || first.Before(a.first) {
			a.first = first
		}
		if last.After(a.last) {
			a.last = last
		}
	}

	views := make([]EventView, 0, len(order))
	for _, k := range order {
		a := byKey[k]
		a.view.FirstSeen = fmtTime(a.first)
		a.view.LastSeen = fmtTime(a.last)
		views = append(views, a.view)
	}
	// Warning 优先，其次时间倒序（同一批里最新的在最前，便于模型先看现场）。
	sort.SliceStable(views, func(i, j int) bool {
		wi, wj := views[i].Type == "Warning", views[j].Type == "Warning"
		if wi != wj {
			return wi
		}
		return views[i].LastSeen > views[j].LastSeen
	})
	if len(views) > limit {
		views = views[:limit]
	}
	return views, nil
}

// Logs 读容器日志：先按硬上限读入，再按字节上限取尾部（故障现场在尾部），
// 截断时标记 Truncated 并在文本首行注明——让模型明确知道自己看到的不是全量。
func (c *Collector) Logs(ctx context.Context, ns, pod string, q LogQuery) (*LogView, error) {
	if q.TailLines <= 0 {
		q.TailLines = defaultLogTailLines
	}
	limitBytes := q.LimitBytes
	if limitBytes <= 0 {
		limitBytes = defaultLogLimitBytes
	}
	opts := &corev1.PodLogOptions{Container: q.Container, Previous: q.Previous}
	tailLines := int64(q.TailLines)
	opts.TailLines = &tailLines
	// 日志读用自己的预算（比元数据调用宽）：流式大对象与"取一个对象"不是一个量级，
	// 共用一个预算会把慢读判成读失败并记 required_failed，等于把环境慢当成证据缺。
	ctx, cancel := c.callCtxFor(ctx, logCallTimeout)
	defer cancel()
	stream, err := c.core.CoreV1().Pods(ns).GetLogs(pod, opts).Stream(ctx)
	if err != nil {
		tag := ""
		if q.Previous {
			tag = "（previous）"
		}
		return nil, fmt.Errorf("read logs %s/%s[%s]%s: %w", ns, pod, q.Container, tag, err)
	}
	defer stream.Close()
	data, err := io.ReadAll(io.LimitReader(stream, maxLogReadBytes))
	if err != nil {
		return nil, fmt.Errorf("read logs %s/%s[%s]: %w", ns, pod, q.Container, err)
	}
	text := string(data)
	truncated := false
	if int64(len(text)) > limitBytes {
		text = text[len(text)-int(limitBytes):]
		// 从行首对齐，避免半行开头。
		if i := strings.IndexByte(text, '\n'); i >= 0 && i < len(text)-1 {
			text = text[i+1:]
		}
		truncated = true
		text = "（日志超长已截断，以下为尾部片段）\n" + text
	}
	return &LogView{
		Container: q.Container,
		Previous:  q.Previous,
		Lines:     countLines(text),
		Truncated: truncated,
		Text:      text,
	}, nil
}

// LogTargets 决定采哪些日志：优先“有异常记录的容器”（waiting reason 或上次终止记录），
// 都没有则取重启最多者，再退化为第一个容器；该容器有上次运行记录时追加 previous 日志
// （CrashLoopBackOff / OOMKilled 的根因几乎只在 previous 日志里）。
//
// 导出而非包内私有：工具层 k8s_logs 的 container 参数缺省时，用它取首个元素的容器名——
// “该采哪个容器”的判定只保留这一处实现（Collect 与工具层共用），避免两处逻辑漂移。
func LogTargets(pv *PodView) []LogQuery {
	if pv == nil {
		return nil
	}
	cands := append(append([]ContainerView{}, pv.InitContainers...), pv.Containers...)
	if len(cands) == 0 {
		return nil
	}
	pick := -1
	for i := range cands {
		if problemContainer(&cands[i]) {
			pick = i
			break
		}
	}
	if pick < 0 {
		best := int32(-1)
		for i := range cands {
			if cands[i].RestartCount > best {
				best, pick = cands[i].RestartCount, i
			}
		}
	}
	if pick < 0 {
		pick = 0
	}
	target := cands[pick]
	out := []LogQuery{{Container: target.Name}}
	// previous 仅在该容器确有上次运行记录时取，否则 API 会直接报错（无内容可读）。
	if target.RestartCount > 0 || target.LastStateReason != "" {
		out = append(out, LogQuery{Container: target.Name, Previous: true})
	}
	return out
}

// problemContainer 判断容器是否处于异常态（启动类 waiting 原因或上次异常终止）。
func problemContainer(c *ContainerView) bool {
	switch c.StateReason {
	case "CrashLoopBackOff", "ImagePullBackOff", "ErrImagePull", "RunContainerError",
		"CreateContainerConfigError", "CreateContainerError", "InvalidImageName", "Error", "OOMKilled":
		return true
	}
	return c.LastStateReason != ""
}

// PodMetrics 读 Pod 实时用量：metrics 只给用量，不给限值，故额外 Get 一次 Pod 取 limits
// 并当场算出使用率（内存 99% 这类判断依赖它；节点侧的分母则是 allocatable）。
func (c *Collector) PodMetrics(ctx context.Context, ns, pod string) (*PodMetricsView, error) {
	ctx, cancel := c.callCtx(ctx)
	defer cancel()
	m, err := c.metrics.MetricsV1beta1().PodMetricses(ns).Get(ctx, pod, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get pod metrics %s/%s（metrics-server 是否就绪？）: %w", ns, pod, err)
	}
	limits := map[string]corev1.ResourceList{}
	if p, err := c.core.CoreV1().Pods(ns).Get(ctx, pod, metav1.GetOptions{}); err == nil {
		for i := range p.Spec.Containers {
			limits[p.Spec.Containers[i].Name] = p.Spec.Containers[i].Resources.Limits
		}
	}
	v := &PodMetricsView{Namespace: ns, Pod: pod, Timestamp: fmtTime(m.Timestamp.Time)}
	for i := range m.Containers {
		cm := &m.Containers[i]
		lim := limits[cm.Name]
		v.Containers = append(v.Containers, ContainerMetricsView{
			Name:          cm.Name,
			CPU:           fmtCPU(cm.Usage[corev1.ResourceCPU]),
			Memory:        fmtMemory(cm.Usage[corev1.ResourceMemory]),
			CPULimit:      fmtCPU(lim[corev1.ResourceCPU]),
			MemoryLimit:   fmtMemory(lim[corev1.ResourceMemory]),
			CPUPercent:    percent(cm.Usage[corev1.ResourceCPU], lim[corev1.ResourceCPU]),
			MemoryPercent: percent(cm.Usage[corev1.ResourceMemory], lim[corev1.ResourceMemory]),
		})
	}
	return v, nil
}

// NodeMetrics 读节点实时用量，分母用 allocatable（节点无 limits）。
func (c *Collector) NodeMetrics(ctx context.Context, node string) (*NodeMetricsView, error) {
	ctx, cancel := c.callCtx(ctx)
	defer cancel()
	m, err := c.metrics.MetricsV1beta1().NodeMetricses().Get(ctx, node, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get node metrics %s（metrics-server 是否就绪？）: %w", node, err)
	}
	var alloc corev1.ResourceList
	if n, err := c.core.CoreV1().Nodes().Get(ctx, node, metav1.GetOptions{}); err == nil {
		alloc = n.Status.Allocatable
	}
	return &NodeMetricsView{
		Name:       node,
		Timestamp:  fmtTime(m.Timestamp.Time),
		CPU:        fmtCPU(m.Usage[corev1.ResourceCPU]),
		Memory:     fmtMemory(m.Usage[corev1.ResourceMemory]),
		CPUPercent: percent(m.Usage[corev1.ResourceCPU], alloc[corev1.ResourceCPU]),
		MemPercent: percent(m.Usage[corev1.ResourceMemory], alloc[corev1.ResourceMemory]),
	}, nil
}

// Node 读节点摘要（Pending 归因要对比"节点余量 vs Pod requests"与 taints）。
func (c *Collector) Node(ctx context.Context, name string) (*NodeView, error) {
	ctx, cancel := c.callCtx(ctx)
	defer cancel()
	n, err := c.core.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get node %s: %w", name, err)
	}
	v := nodeView(n)
	// 分配账本：把"节点上已有 Pod 的 requests 汇总 + 剩余可分配"算好（Pending 归因的核心算术）。
	// 失败只降级（节点自身摘要仍有用），原因写进视图字段——空值不能被当成"没有占用"。
	pods, reqs, lims, err := c.nodeAllocation(ctx, name)
	if err != nil {
		v.AllocationError = err.Error()
		return v, nil
	}
	applyLedger(v, n.Status.Allocatable, pods, reqs, lims)
	return v, nil
}

// Nodes 列出全部节点摘要。为什么要单列一条路径：Pending 的 Pod 没有 node_name（没被调度就不可能
// 绑定节点），Node 那条"按 Pod 的 node_name 取节点"对它完全失效——不列节点，模型就看不到节点标签
// （"nodeSelector/affinity 要求的标签在不在"无从证实）与余量（"requests 是否超可分配"只能听事件里的
// 半句话）。账本一次列全量 Pod 后在内存里按节点分组，避免逐节点 list（N 次全量扫描）。
func (c *Collector) Nodes(ctx context.Context) ([]NodeView, error) {
	ctx, cancel := c.callCtx(ctx)
	defer cancel()
	list, err := c.core.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	views := make([]NodeView, 0, len(list.Items))
	for i := range list.Items {
		views = append(views, *nodeView(&list.Items[i]))
	}
	byNode, err := c.podsByNode(ctx)
	if err != nil {
		// 账本失败只降级：标签/taints/allocatable 仍是 Pending 归因要用的证据，余量不可信而已。
		for i := range views {
			views[i].AllocationError = err.Error()
		}
		return views, nil
	}
	for i := range views {
		pods, reqs, lims := ledger(byNode[views[i].Name])
		applyLedger(&views[i], list.Items[i].Status.Allocatable, pods, reqs, lims)
	}
	sort.Slice(views, func(i, j int) bool { return views[i].Name < views[j].Name })
	return views, nil
}

// nodeView 节点摘要里不依赖集群状态的部分（Get 与 List 两条路径共用，避免两处漂移）。
func nodeView(n *corev1.Node) *NodeView {
	v := &NodeView{
		Name:           n.Name,
		KubeletVersion: n.Status.NodeInfo.KubeletVersion,
		CreatedAt:      rfc3339(n.CreationTimestamp),
		Allocatable:    fmtResourceList(n.Status.Allocatable),
		Capacity:       fmtResourceList(n.Status.Capacity),
		Labels:         nodeLabels(n.Labels),
	}
	for _, cond := range n.Status.Conditions {
		if cond.Type == corev1.NodeReady {
			v.Ready = string(cond.Status)
		}
		v.Conditions = append(v.Conditions, ConditionView{
			Type: string(cond.Type), Status: string(cond.Status),
			Reason: cond.Reason, Message: cond.Message, LastTransitionTime: rfc3339(cond.LastTransitionTime),
		})
	}
	for _, t := range n.Spec.Taints {
		v.Taints = append(v.Taints, fmtToleration(corev1.Toleration{
			Key: t.Key, Value: t.Value, Effect: t.Effect,
		}))
	}
	return v
}

// PVC 读存储申请摘要。Pending 的"未绑定 PVC"分支靠 phase 与 storage_class 的组合定性：
// Pending + 有 storageClass 是供给失败（provisioner 报错/容量不足），Pending + 无 storageClass
// 是"既没有匹配的现成 PV，也没法动态供给"——两种根因与建议完全不同，少一个字段就只能猜。
// 它自己的事件不在这里取（事件是独立来源，失败也不该让 PVC 摘要一起丢，见 Collect）。
func (c *Collector) PVC(ctx context.Context, ns, name string) (*PVCView, error) {
	ctx, cancel := c.callCtx(ctx)
	defer cancel()
	p, err := c.core.CoreV1().PersistentVolumeClaims(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get pvc %s/%s: %w", ns, name, err)
	}
	return toPVCView(p), nil
}

// nodeAllocation 汇总节点上已有 Pod 的数量与 requests/limits 合计。
func (c *Collector) nodeAllocation(ctx context.Context, node string) (int, corev1.ResourceList, corev1.ResourceList, error) {
	list, err := c.core.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		FieldSelector: "spec.nodeName=" + node,
	})
	if err != nil {
		return 0, nil, nil, fmt.Errorf("list pods on node %s: %w", node, err)
	}
	pods, reqs, lims := ledger(list.Items)
	return pods, reqs, lims, nil
}

// podsByNode 一次列全量 Pod 并按 spec.nodeName 分组（含终态，计不计入由 ledger 决定）；
// 未调度的 Pod（Pending、nodeName 为空）不属于任何节点，直接丢掉——它们还没占资源。
func (c *Collector) podsByNode(ctx context.Context) (map[string][]corev1.Pod, error) {
	list, err := c.core.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list pods: %w", err)
	}
	byNode := make(map[string][]corev1.Pod, len(list.Items))
	for _, p := range list.Items {
		if p.Spec.NodeName == "" {
			continue
		}
		byNode[p.Spec.NodeName] = append(byNode[p.Spec.NodeName], p)
	}
	return byNode, nil
}

// applyLedger 把账本写进视图，并算好剩余可分配。
// free = allocatable − 已用；pods 这一项要特殊处理：它是个数配额，不是 requests 账本
// （没人给 "pods" 资源写 requests），直接按 requests 去减会永远算出"一个都没占"。
func applyLedger(v *NodeView, allocatable corev1.ResourceList, pods int, reqs, lims corev1.ResourceList) {
	v.PodsOnNode = pods
	v.RequestsOnNode = fmtResourceList(reqs)
	v.LimitsOnNode = fmtResourceList(lims)
	v.FreeOnNode = fmtResourceList(freeResources(allocatable, usedResources(reqs, pods)))
}

// ledger 把一批 Pod 归成"非终态数量 + requests/limits 合计"。
// 调度口径：终态（Succeeded/Failed）Pod 不再占资源；每个 Pod 的每项资源按
// max(普通容器之和, init 容器最大值) 计入（k8s 对 init 容器取 max 而非求和；
// 重启型 sidecar 的叠加效应不在此展开，它属于边角情形）。
func ledger(pods []corev1.Pod) (int, corev1.ResourceList, corev1.ResourceList) {
	sumReq, sumLim := corev1.ResourceList{}, corev1.ResourceList{}
	n := 0
	for i := range pods {
		p := &pods[i]
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		n++
		reqs, lims := podSchedulingResources(p)
		addResourceList(sumReq, reqs)
		addResourceList(sumLim, lims)
	}
	return n, sumReq, sumLim
}

// podSchedulingResources 取一个 Pod 的调度口径 requests（limits 仅作参考值返回）。
func podSchedulingResources(p *corev1.Pod) (corev1.ResourceList, corev1.ResourceList) {
	reqs, lims := corev1.ResourceList{}, corev1.ResourceList{}
	for i := range p.Spec.Containers {
		addResourceList(reqs, p.Spec.Containers[i].Resources.Requests)
		addResourceList(lims, p.Spec.Containers[i].Resources.Limits)
	}
	// init 容器按"每个资源取最大值"参与调度计算，不是求和。
	initMax := corev1.ResourceList{}
	for i := range p.Spec.InitContainers {
		for name, q := range p.Spec.InitContainers[i].Resources.Requests {
			if cur, ok := initMax[name]; !ok || cur.Cmp(q) < 0 {
				initMax[name] = q
			}
		}
	}
	for name, q := range initMax {
		if cur, ok := reqs[name]; !ok || cur.Cmp(q) < 0 {
			reqs[name] = q
		}
	}
	return reqs, lims
}

// addResourceList 把 src 按资源名累加进 dst。
func addResourceList(dst, src corev1.ResourceList) {
	for name, q := range src {
		if cur, ok := dst[name]; ok {
			sum := cur.DeepCopy()
			sum.Add(q)
			dst[name] = sum
			continue
		}
		dst[name] = q.DeepCopy()
	}
}

// usedResources 在 requests 合计之上补上"Pod 个数"这一项：allocatable 里的 pods 是个数配额，
// 它的"已用"是节点上的非终态 Pod 数（与 kubelet 判 maxPods 的口径一致），不是 requests 求和。
// 漏掉这一项，free_on_node 的 pods 值就会等于总额度，看着像"还能放满一整批"。
func usedResources(reqs corev1.ResourceList, podsOnNode int) corev1.ResourceList {
	used := corev1.ResourceList{}
	addResourceList(used, reqs)
	used[corev1.ResourcePods] = *resource.NewQuantity(int64(podsOnNode), resource.DecimalSI)
	return used
}

// freeResources 计算 allocatable − used（同资源名相减；差值可能为负，说明该资源已被超卖，
// 保留负值比截断成 0 更有信息量）。used 由调用方给全（见 usedResources 对 pods 的特殊处理）。
func freeResources(allocatable, used corev1.ResourceList) corev1.ResourceList {
	if len(allocatable) == 0 {
		return nil
	}
	out := corev1.ResourceList{}
	for name, q := range allocatable {
		free := q.DeepCopy()
		if u, ok := used[name]; ok {
			free.Sub(u)
		}
		out[name] = free
	}
	return out
}

// --- 内部：对象 → 视图的转换 ---

// toPodView 把 Pod 对象转成裁剪视图（只拷贝白名单字段，见 view.go 说明）。
func toPodView(p *corev1.Pod) PodView {
	v := PodView{
		Namespace:      p.Namespace,
		Name:           p.Name,
		UID:            string(p.UID),
		Phase:          string(p.Status.Phase),
		QOSClass:       string(p.Status.QOSClass),
		NodeName:       p.Spec.NodeName,
		RestartPolicy:  string(p.Spec.RestartPolicy),
		ServiceAccount: p.Spec.ServiceAccountName,
		CreatedAt:      rfc3339(p.CreationTimestamp),
		DeletedAt:      rfc3339Ptr(p.DeletionTimestamp),
		Labels:         p.Labels,
		Annotations:    pickAnnotations(p.Annotations),
		NodeSelector:   p.Spec.NodeSelector,
		Affinity:       affinityView(p.Spec.Affinity),
	}
	for _, s := range p.Spec.ImagePullSecrets {
		v.ImagePullSecrets = append(v.ImagePullSecrets, s.Name)
	}
	for _, o := range p.OwnerReferences {
		v.OwnerRefs = append(v.OwnerRefs, ownerRefView(o))
	}
	for _, cond := range p.Status.Conditions {
		v.Conditions = append(v.Conditions, ConditionView{
			Type: string(cond.Type), Status: string(cond.Status),
			Reason: cond.Reason, Message: cond.Message, LastTransitionTime: rfc3339(cond.LastTransitionTime),
		})
	}
	for i := range p.Spec.Containers {
		spec := &p.Spec.Containers[i]
		v.Containers = append(v.Containers, toContainerView(spec, statusOf(p.Status.ContainerStatuses, spec.Name)))
	}
	for i := range p.Spec.InitContainers {
		spec := &p.Spec.InitContainers[i]
		v.InitContainers = append(v.InitContainers, toContainerView(spec, statusOf(p.Status.InitContainerStatuses, spec.Name)))
	}
	for _, t := range p.Spec.Tolerations {
		v.Tolerations = append(v.Tolerations, fmtToleration(t))
	}
	for _, vol := range p.Spec.Volumes {
		if s := volumeSource(vol); s != "" {
			v.Volumes = append(v.Volumes, VolumeView{Name: vol.Name, Source: s})
		}
	}
	return v
}

// toPVCView 把 PVC 转成裁剪视图：只留"绑没绑上、谁供给的、要多大"这几个定性字段。
// UsedByVolumes 与 Events 由 Collect 补齐（它们要知道 Pod 的卷名与额外的采集动作）。
func toPVCView(p *corev1.PersistentVolumeClaim) *PVCView {
	v := &PVCView{
		Name:             p.Name,
		UID:              string(p.UID),
		Phase:            string(p.Status.Phase),
		VolumeName:       p.Spec.VolumeName,
		RequestedStorage: fmtMemory(p.Spec.Resources.Requests[corev1.ResourceStorage]),
	}
	// storageClassName 是指针：nil 与空串都表示"不指定"，而"不指定"在有无默认 StorageClass
	// 的集群里语义相反，故统一留空由模型结合 events 判断（无法在采集侧区分默认类是否存在）。
	if p.Spec.StorageClassName != nil {
		v.StorageClass = *p.Spec.StorageClassName
	}
	for _, m := range p.Spec.AccessModes {
		v.AccessModes = append(v.AccessModes, string(m))
	}
	return v
}

// toContainerView 合并容器规格与容器状态（状态可能缺失：容器尚未启动时）。
func toContainerView(spec *corev1.Container, st *corev1.ContainerStatus) ContainerView {
	v := ContainerView{
		Name:         spec.Name,
		Image:        spec.Image,
		Requests:     fmtResourceList(spec.Resources.Requests),
		Limits:       fmtResourceList(spec.Resources.Limits),
		Command:      spec.Command,
		Args:         spec.Args,
		EnvFrom:      envFromRefs(spec.EnvFrom),
		Env:          envRefs(spec.Env),
		VolumeMounts: volumeMounts(spec.VolumeMounts),
	}
	for _, p := range []*ProbeView{
		probeView("liveness", spec.LivenessProbe),
		probeView("readiness", spec.ReadinessProbe),
		probeView("startup", spec.StartupProbe),
	} {
		if p != nil {
			v.Probes = append(v.Probes, *p)
		}
	}
	if st == nil {
		return v
	}
	v.Ready = st.Ready
	v.RestartCount = st.RestartCount
	switch {
	case st.State.Running != nil:
		v.State = "running"
		v.StartedAt = fmtTime(st.State.Running.StartedAt.Time)
	case st.State.Waiting != nil:
		v.State = "waiting"
		v.StateReason = st.State.Waiting.Reason
		v.StateMessage = st.State.Waiting.Message
	case st.State.Terminated != nil:
		v.State = "terminated"
		v.StateReason = st.State.Terminated.Reason
		v.StateMessage = st.State.Terminated.Message
		v.ExitCode = &st.State.Terminated.ExitCode
		v.Signal = &st.State.Terminated.Signal
		v.StartedAt = fmtTime(st.State.Terminated.StartedAt.Time)
		v.FinishedAt = fmtTime(st.State.Terminated.FinishedAt.Time)
	}
	// 上次运行记录：OOMKilled 最常出现在这里，单独铺平便于模型直接读到。
	switch {
	case st.LastTerminationState.Terminated != nil:
		t := st.LastTerminationState.Terminated
		v.LastState = "terminated"
		v.LastStateReason = t.Reason
		v.LastStateMessage = t.Message
		v.LastStateExitCode = &t.ExitCode
		v.LastStateStartedAt = fmtTime(t.StartedAt.Time)
		v.LastStateFinishedAt = fmtTime(t.FinishedAt.Time)
	case st.LastTerminationState.Waiting != nil:
		w := st.LastTerminationState.Waiting
		v.LastState = "waiting"
		v.LastStateReason = w.Reason
		v.LastStateMessage = w.Message
	}
	return v
}

// toPodTemplate 转工作负载的 Pod 模板规格（只填规格字段，不带状态）。
func toPodTemplate(t *corev1.PodTemplateSpec) PodTemplateView {
	v := PodTemplateView{NodeSelector: t.Spec.NodeSelector, Affinity: affinityView(t.Spec.Affinity)}
	for i := range t.Spec.Containers {
		v.Containers = append(v.Containers, toContainerView(&t.Spec.Containers[i], nil))
	}
	for _, tol := range t.Spec.Tolerations {
		v.Tolerations = append(v.Tolerations, fmtToleration(tol))
	}
	for _, vol := range t.Spec.Volumes {
		if s := volumeSource(vol); s != "" {
			v.Volumes = append(v.Volumes, VolumeView{Name: vol.Name, Source: s})
		}
	}
	return v
}

// statusOf 按容器名找状态（Pod 状态列表与规格列表顺序不保证一致）。
func statusOf(statuses []corev1.ContainerStatus, name string) *corev1.ContainerStatus {
	for i := range statuses {
		if statuses[i].Name == name {
			return &statuses[i]
		}
	}
	return nil
}

// controllerRef 取 controller=true 的 owner（真正的管理者，非 controller 的 owner 只是标记）。
func controllerRef(refs []metav1.OwnerReference) *metav1.OwnerReference {
	for i := range refs {
		if refs[i].Controller != nil && *refs[i].Controller {
			return &refs[i]
		}
	}
	return nil
}

// controllerRefView 在 owner 引用视图里找 controller=true 者（ReplicaSet → Deployment 上溯用）。
func controllerRefView(refs []OwnerRefView) *OwnerRefView {
	for i := range refs {
		if refs[i].Controller {
			return &refs[i]
		}
	}
	return nil
}

// ownerRefView 转 owner 引用视图。
func ownerRefView(o metav1.OwnerReference) OwnerRefView {
	return OwnerRefView{
		Kind:       o.Kind,
		Name:       o.Name,
		APIVersion: o.APIVersion,
		Controller: o.Controller != nil && *o.Controller,
	}
}

// volumeMounts 渲染挂载点（"卷名:路径"），用于定位路径/只读类失败。
func volumeMounts(ms []corev1.VolumeMount) []string {
	var out []string
	for _, m := range ms {
		s := m.Name + ":" + m.MountPath
		if m.ReadOnly {
			s += " (ro)"
		}
		out = append(out, s)
	}
	return out
}

// nodeLabels 保留节点的全部标签。为什么不做白名单（2026-09-15 改）：nodeSelector 与 affinity
// 可以引用任意自定义键（disktype、gpu、自定义拓扑域），一旦按"拓扑/机型"白名单裁剪，
// "要求的标签在不在"就永远无法证实——而节点标签体量很小（通常十几个），裁掉的风险远大于省下的 token。
func nodeLabels(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// --- 内部：工作负载读取 ---

func (c *Collector) deployment(ctx context.Context, ns, name string) (*WorkloadView, error) {
	ctx, cancel := c.callCtx(ctx)
	defer cancel()
	d, err := c.core.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get deployment %s/%s: %w", ns, name, err)
	}
	v := &WorkloadView{
		Kind:              "Deployment",
		Namespace:         ns,
		Name:              name,
		Replicas:          int32Or(d.Spec.Replicas, 0),
		ReadyReplicas:     d.Status.ReadyReplicas,
		AvailableReplicas: d.Status.AvailableReplicas,
		Unavailable:       d.Status.UnavailableReplicas,
		Strategy:          string(d.Spec.Strategy.Type),
		CreatedAt:         rfc3339(d.CreationTimestamp),
		Conditions:        deploymentConditions(d.Status.Conditions),
		Template:          toPodTemplate(&d.Spec.Template),
	}
	return v, nil
}

func (c *Collector) replicaSet(ctx context.Context, ns, name string) (*WorkloadView, error) {
	ctx, cancel := c.callCtx(ctx)
	defer cancel()
	rs, err := c.core.AppsV1().ReplicaSets(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get replicaset %s/%s: %w", ns, name, err)
	}
	// 用 OwnerReferences 视图即可（还需再上溯到 Deployment），规格不从这里取。
	return &WorkloadView{
		Kind:          "ReplicaSet",
		Namespace:     ns,
		Name:          name,
		Replicas:      int32Or(rs.Spec.Replicas, 0),
		ReadyReplicas: rs.Status.ReadyReplicas,
		CreatedAt:     rfc3339(rs.CreationTimestamp),
		OwnerRefs:     ownerRefViews(rs.OwnerReferences),
	}, nil
}

func (c *Collector) statefulSet(ctx context.Context, ns, name string) (*WorkloadView, error) {
	ctx, cancel := c.callCtx(ctx)
	defer cancel()
	s, err := c.core.AppsV1().StatefulSets(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get statefulset %s/%s: %w", ns, name, err)
	}
	return &WorkloadView{
		Kind:              "StatefulSet",
		Namespace:         ns,
		Name:              name,
		Replicas:          int32Or(s.Spec.Replicas, 0),
		ReadyReplicas:     s.Status.ReadyReplicas,
		AvailableReplicas: s.Status.AvailableReplicas,
		Strategy:          string(s.Spec.UpdateStrategy.Type),
		CreatedAt:         rfc3339(s.CreationTimestamp),
		Conditions:        statefulSetConditions(s.Status.Conditions),
		Template:          toPodTemplate(&s.Spec.Template),
	}, nil
}

func (c *Collector) daemonSet(ctx context.Context, ns, name string) (*WorkloadView, error) {
	ctx, cancel := c.callCtx(ctx)
	defer cancel()
	d, err := c.core.AppsV1().DaemonSets(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get daemonset %s/%s: %w", ns, name, err)
	}
	return &WorkloadView{
		Kind:          "DaemonSet",
		Namespace:     ns,
		Name:          name,
		ReadyReplicas: d.Status.NumberReady,
		Unavailable:   d.Status.NumberUnavailable,
		Strategy:      string(d.Spec.UpdateStrategy.Type),
		CreatedAt:     rfc3339(d.CreationTimestamp),
		Conditions:    daemonSetConditions(d.Status.Conditions),
		Template:      toPodTemplate(&d.Spec.Template),
	}, nil
}

func (c *Collector) job(ctx context.Context, ns, name string) (*WorkloadView, error) {
	ctx, cancel := c.callCtx(ctx)
	defer cancel()
	j, err := c.core.BatchV1().Jobs(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get job %s/%s: %w", ns, name, err)
	}
	// Job 不填 Replicas：Completions 与"副本数"是两套语义（见 JobView 注释），
	// 复用会把"要成功 3 次"读成"保持 3 个副本"，属于误导性证据。
	v := &WorkloadView{
		Kind:      "Job",
		Namespace: ns,
		Name:      name,
		CreatedAt: rfc3339(j.CreationTimestamp),
		Template:  toPodTemplate(&j.Spec.Template),
		Job: &JobView{
			Completions:  j.Spec.Completions,
			Parallelism:  j.Spec.Parallelism,
			Active:       j.Status.Active,
			Succeeded:    j.Status.Succeeded,
			Failed:       j.Status.Failed,
			BackoffLimit: j.Spec.BackoffLimit,
			Conditions:   jobConditions(j.Status.Conditions),
		},
	}
	return v, nil
}

// condView 单条条件的公共转换：各工作负载的 condition 类型不同但字段同形（type/status/
// reason/message/lastTransitionTime），逐个类型复制一遍搬运逻辑没有意义，故收敛到这一个地方。
func condView(typ, status, reason, message string, t metav1.Time) ConditionView {
	return ConditionView{
		Type: typ, Status: status, Reason: reason, Message: message,
		LastTransitionTime: rfc3339(t),
	}
}

// deploymentConditions 转 Deployment 条件（Progressing / Available 的 Reason 常点出真因）。
func deploymentConditions(conds []appsv1.DeploymentCondition) []ConditionView {
	var out []ConditionView
	for _, c := range conds {
		out = append(out, condView(string(c.Type), string(c.Status), c.Reason, c.Message, c.LastTransitionTime))
	}
	return out
}

// statefulSetConditions 转 StatefulSet 条件（常见的是 ReplicaFailure）。
func statefulSetConditions(conds []appsv1.StatefulSetCondition) []ConditionView {
	var out []ConditionView
	for _, c := range conds {
		out = append(out, condView(string(c.Type), string(c.Status), c.Reason, c.Message, c.LastTransitionTime))
	}
	return out
}

// daemonSetConditions 转 DaemonSet 条件。
func daemonSetConditions(conds []appsv1.DaemonSetCondition) []ConditionView {
	var out []ConditionView
	for _, c := range conds {
		out = append(out, condView(string(c.Type), string(c.Status), c.Reason, c.Message, c.LastTransitionTime))
	}
	return out
}

// jobConditions 转 Job 条件：Failed=True 的 Reason（如 BackoffLimitExceeded）就是根因本身。
func jobConditions(conds []batchv1.JobCondition) []ConditionView {
	var out []ConditionView
	for _, c := range conds {
		out = append(out, condView(string(c.Type), string(c.Status), c.Reason, c.Message, c.LastTransitionTime))
	}
	return out
}

// ownerRefViews 批量转 owner 引用视图。
func ownerRefViews(refs []metav1.OwnerReference) []OwnerRefView {
	var out []OwnerRefView
	for _, r := range refs {
		out = append(out, ownerRefView(r))
	}
	return out
}

// int32Or 解引用可选副本数（nil 视为缺省值）。
func int32Or(p *int32, def int32) int32 {
	if p == nil {
		return def
	}
	return *p
}

// countLines 数行数（无换行结尾时补一行）。
func countLines(s string) int {
	if s == "" {
		return 0
	}
	n := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}

// fmtTime 格式化时间（零值返回空串）。
func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}
