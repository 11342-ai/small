package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"small/internal/k8s"
	"small/internal/tool"
)

// K8s 只读采集工具：九个薄壳（六个原子工具 + k8s_nodes 列节点 + 证据包/报告），
// 把 internal/k8s 的裁剪视图回灌给模型。
//
// 分工（别越界）：采集、裁剪、单位换算、超时、重试都在采集包；这一层只做四件事——
// 解码参数、校验必填、转发调用、序列化回灌。
// 失败语义：参数问题 / 对象不存在 / API 报错统一按业务失败（IsError=true, err=nil）回灌，
// 让模型自己换参数或换策略；只有框架级错误（如序列化失败）才 return err。
// 权限：九个工具全只读，登记在 tool_permissions.go 为 policy.Pass（k8s_report 只写自有产物目录，同 doc_parse）。

// 参数缺省值：JSON Schema 的 default 在非 strict 模式下不保证生效，故 Go 侧兜底。
const (
	k8sLogTailLines   = 200
	k8sLogLimitBytes  = 65536
	k8sEventSinceSecs = 3600
	k8sEventLimit     = 50
)

// --- 共用助手 ---

// k8sFail 业务失败：作为结果回灌模型，不中断 agent 循环。
func k8sFail(msg string) (tool.Result, error) {
	return tool.Result{Data: msg, IsError: true}, nil
}

// k8sJSON 把视图序列化成回灌文本（两空格缩进：模型读得稳，人看也友好）。
func k8sJSON(v any) (tool.Result, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		// 视图是我们自己的结构，序列化失败，说明契约破了 -- 框架级错误才往上抛
		return tool.Result{}, fmt.Errorf("k8s tool: marshal view: %w", err)
	}
	return tool.Result{Data: string(data)}, nil
}

// podArgs namespace + pod 型工具的共用入参（抽出来避免几处漂移）。
// 字段必须带 json tag：Go 只对"字段名与键名不一致"做大小写不敏感匹配，
// pod/since_seconds 这类键没有 tag 就永远匹配不上。
type podArgs struct {
	Namespace string `json:"namespace"`
	Pod       string `json:"pod"`
}

// decodePodArgs 解码并校验 namespace/pod。
func decodePodArgs(args json.RawMessage) (podArgs, error) {
	var in podArgs
	if err := json.Unmarshal(args, &in); err != nil {
		return in, fmt.Errorf("参数错误: %v", err)
	}
	if in.Namespace == "" || in.Pod == "" {
		return in, fmt.Errorf("参数错误: namespace 与 pod 均必填")
	}
	return in, nil
}

// containerArgs namespace + pod + 可选 container。
type containerArgs struct {
	Namespace string `json:"namespace"`
	Pod       string `json:"pod"`
	Container string `json:"container"`
}

// decodePodArgsWithContainer 解码并校验 namespace/pod（container 可空）。
func decodePodArgsWithContainer(args json.RawMessage) (containerArgs, error) {
	var in containerArgs
	if err := json.Unmarshal(args, &in); err != nil {
		return in, fmt.Errorf("参数错误: %v", err)
	}
	if in.Namespace == "" || in.Pod == "" {
		return in, fmt.Errorf("参数错误: namespace 与 pod 均必填")
	}
	return in, nil
}

// --- k8s_workload ---

// K8sWorkload 取顶层工作负载的规格真源（Pod → ReplicaSet → Deployment 等）。
func K8sWorkload(coll *k8s.Collector) tool.Tool {
	return tool.NewFunc(
		tool.Spec{
			Name: "k8s_workload",
			Description: "读取故障 Pod 所属顶层工作负载的规格真源（Pod → ReplicaSet → Deployment，或" +
				"StatefulSet/DaemonSet/Job）：模板容器的 resources/probes/image/command、replicas、strategy、" +
				"status.conditions；Job 用 job 块（completions/parallelism/active/succeeded/failed/backoff_limit）。" +
				"回答“限制与探针当初怎么配的”时调用；裸 Pod（无 controller owner）会返回业务失败。",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"namespace": {"type": "string", "description": "命名空间"},
					"pod": {"type": "string", "description": "Pod 名（由它上溯工作负载）"}
				},
				"required": ["namespace", "pod"],
				"additionalProperties": false
			}`),
		},
		func(ctx context.Context, args json.RawMessage) (tool.Result, error) {
			return runK8sWorkload(ctx, coll, args)
		},
	)
}

// runK8sWorkload 解码 → 校验 → 上溯采集 → 序列化。
func runK8sWorkload(ctx context.Context, coll *k8s.Collector, args json.RawMessage) (tool.Result, error) {
	in, err := decodePodArgs(args)
	if err != nil {
		return k8sFail(err.Error())
	}
	w, err := coll.WorkloadOf(ctx, in.Namespace, in.Pod)
	if err != nil {
		return k8sFail("读取工作负载失败: " + k8s.ExplainError(err))
	}
	return k8sJSON(w)
}

// --- k8s_pod ---

// K8sPod 读单个 Pod 的现状摘要（诊断起点）。
func K8sPod(coll *k8s.Collector) tool.Tool {
	return tool.NewFunc(
		tool.Spec{
			Name: "k8s_pod",
			Description: "读取单个 Pod 的现状摘要（诊断起点）：phase、容器当前状态与 waiting reason" +
				"（CrashLoopBackOff / ImagePullBackOff）、上次终止原因（OOMKilled 在此字段）、restartCount、" +
				"resources、探针、卷与 owner 链。需要症状字段、容器名或所在节点时先调用它。",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"namespace": {"type": "string", "description": "命名空间，如 default"},
					"pod": {"type": "string", "description": "Pod 名"}
				},
				"required": ["namespace", "pod"],
				"additionalProperties": false
			}`),
		},
		func(ctx context.Context, args json.RawMessage) (tool.Result, error) {
			return runK8sPod(ctx, coll, args)
		},
	)
}

// runK8sPod 解码 → 校验 → 采集 Pod 现状 → 序列化。
func runK8sPod(ctx context.Context, coll *k8s.Collector, args json.RawMessage) (tool.Result, error) {
	in, err := decodePodArgs(args)
	if err != nil {
		return k8sFail(err.Error())
	}
	pv, err := coll.Pod(ctx, in.Namespace, in.Pod)
	if err != nil {
		return k8sFail("读取 Pod 失败: " + k8s.ExplainError(err))
	}
	return k8sJSON(pv)
}

// --- k8s_events ---

// K8sEvents 读事件摘要（Warning 优先、同因合并计数）。
func K8sEvents(coll *k8s.Collector) tool.Tool {
	return tool.NewFunc(
		tool.Spec{
			Name: "k8s_events",
			Description: "读取某个 Pod 的最近事件（按 involvedObject 过滤，Warning 优先、同因合并计数）：" +
				"调度失败、拉镜像失败、探针失败、容器退避都在这里。发现症状后先看事件，判断 k8s 在哪一步卡住；" +
				"缺省最近 1 小时、最多 50 条。",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"namespace": {"type": "string", "description": "命名空间"},
					"pod": {"type": "string", "description": "Pod 名"},
					"since_seconds": {"type": "integer", "description": "时间窗（秒），缺省 3600"},
					"limit": {"type": "integer", "description": "最多返回条数，缺省 50"}
				},
				"required": ["namespace", "pod"],
				"additionalProperties": false
			}`),
		},
		func(ctx context.Context, args json.RawMessage) (tool.Result, error) {
			return runK8sEvents(ctx, coll, args)
		},
	)
}

// eventArgs namespace + pod + 可选时间窗与条数。
type eventArgs struct {
	Namespace    string `json:"namespace"`
	Pod          string `json:"pod"`
	SinceSeconds int    `json:"since_seconds"`
	Limit        int    `json:"limit"`
}

// decodeEvents 解码并校验 namespace/pod，缺省值在 Go 侧兜底（时间窗按秒，避免暴露 time.Duration 的纳秒语义）。
func decodeEvents(args json.RawMessage) (eventArgs, error) {
	var in eventArgs
	if err := json.Unmarshal(args, &in); err != nil {
		return in, fmt.Errorf("参数错误: %v", err)
	}
	if in.Namespace == "" || in.Pod == "" {
		return in, fmt.Errorf("参数错误: namespace 与 pod 均必填")
	}
	if in.SinceSeconds <= 0 {
		in.SinceSeconds = k8sEventSinceSecs
	}
	if in.Limit <= 0 {
		in.Limit = k8sEventLimit
	}
	return in, nil
}

// runK8sEvents 解码 → 先取 Pod 拿 uid → 查事件 → 序列化。
// uid 不进参数：它是采集层的过滤细节，模型只需知道 namespace/pod（由工具自己补 uid）。
func runK8sEvents(ctx context.Context, coll *k8s.Collector, args json.RawMessage) (tool.Result, error) {
	in, err := decodeEvents(args)
	if err != nil {
		return k8sFail(err.Error())
	}
	pv, err := coll.Pod(ctx, in.Namespace, in.Pod)
	if err != nil {
		return k8sFail("读取 Pod 失败（事件按它的 uid 过滤）: " + k8s.ExplainError(err))
	}
	events, err := coll.Events(ctx, in.Namespace, in.Pod, pv.UID,
		time.Duration(in.SinceSeconds)*time.Second, in.Limit)
	if err != nil {
		return k8sFail("读取事件失败: " + k8s.ExplainError(err))
	}
	return k8sJSON(events)
}

// --- k8s_logs ---

// K8sLogs 读容器日志（当前或上一次）。
func K8sLogs(coll *k8s.Collector) tool.Tool {
	return tool.NewFunc(
		tool.Spec{
			Name: "k8s_logs",
			Description: "读取容器日志（缺省尾部 200 行，超过 64KB 保留尾部并标记截断）：previous=false 读当前实例，" +
				"previous=true 读上一次已退出实例——CrashLoopBackOff / OOMKilled 的根因几乎只在 previous 里。" +
				"容器名可由 k8s_pod 的返回得到。",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"namespace": {"type": "string", "description": "命名空间"},
					"pod": {"type": "string", "description": "Pod 名"},
					"container": {"type": "string", "description": "容器名（含 init 容器）；缺省由工具自动选（异常容器优先，其次重启最多者），选了哪个见返回里的 container 字段"},
					"previous": {"type": "boolean", "description": "是否读上一次已退出实例的日志，缺省 false"},
					"tail_lines": {"type": "integer", "description": "尾部行数，缺省 200"},
					"limit_bytes": {"type": "integer", "description": "字节上限，缺省 65536"}
				},
				"required": ["namespace", "pod"],
				"additionalProperties": false
			}`),
		},
		func(ctx context.Context, args json.RawMessage) (tool.Result, error) {
			return runK8sLogs(ctx, coll, args)
		},
	)
}

// logArgs 日志工具入参：拍平成六个字段（不暴露 k8s.LogQuery 内部类型，
// 它没有 json tag，tail_lines/limit_bytes 这类键匹配不上）。
type logArgs struct {
	Namespace  string `json:"namespace"`
	Pod        string `json:"pod"`
	Container  string `json:"container"`
	Previous   bool   `json:"previous"`
	TailLines  int64  `json:"tail_lines"`
	LimitBytes int64  `json:"limit_bytes"`
}

// decodeLogArgs 解码并校验 namespace/pod（container 可空：空则由采集包的 LogTargets 自动选），
// 缺省值 Go 侧兜底。
func decodeLogArgs(args json.RawMessage) (logArgs, error) {
	var in logArgs
	if err := json.Unmarshal(args, &in); err != nil {
		return in, fmt.Errorf("参数错误: %v", err)
	}
	if in.Namespace == "" || in.Pod == "" {
		return in, fmt.Errorf("参数错误: namespace 与 pod 均必填")
	}
	if in.TailLines <= 0 {
		in.TailLines = k8sLogTailLines
	}
	if in.LimitBytes <= 0 {
		in.LimitBytes = k8sLogLimitBytes
	}
	return in, nil
}

// runK8sLogs 解码 →（container 缺省时自动选）→ 组装 LogQuery → 采集 → 序列化。
func runK8sLogs(ctx context.Context, coll *k8s.Collector, args json.RawMessage) (tool.Result, error) {
	in, err := decodeLogArgs(args)
	if err != nil {
		return k8sFail(err.Error())
	}
	container := in.Container
	if container == "" {
		// container 缺省：复用采集包的判定（异常容器优先 → 重启最多者 → 第一个），
		// 保证“该采哪个容器”只有一处实现（Collect 里也走 LogTargets）。
		pv, err := coll.Pod(ctx, in.Namespace, in.Pod)
		if err != nil {
			return k8sFail("读取 Pod 失败（container 缺省时要靠它选容器）: " + k8s.ExplainError(err))
		}
		targets := k8s.LogTargets(pv)
		if len(targets) == 0 {
			return k8sFail("该 Pod 没有可取日志的容器，请显式传 container")
		}
		container = targets[0].Container
	}
	lv, err := coll.Logs(ctx, in.Namespace, in.Pod, k8s.LogQuery{
		Container:  container,
		Previous:   in.Previous,
		TailLines:  in.TailLines,
		LimitBytes: in.LimitBytes,
	})
	if err != nil {
		return k8sFail("读取日志失败: " + k8s.ExplainError(err))
	}
	return k8sJSON(lv)
}

// --- k8s_metrics ---

// K8sMetrics 读 Pod 实时用量（metrics-server）。
func K8sMetrics(coll *k8s.Collector) tool.Tool {
	return tool.NewFunc(
		tool.Spec{
			Name: "k8s_metrics",
			Description: "读取 Pod 的实时用量（来自 metrics-server）：各容器 CPU/内存用量、限值，以及用量占限值的百分比。" +
				"判断是不是资源顶到限值时调用；只有当前采样（无历史），容器尚未启动或 metrics-server 未就绪会返回业务失败。",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"namespace": {"type": "string", "description": "命名空间"},
					"pod": {"type": "string", "description": "Pod 名"},
					"container": {"type": "string", "description": "只看某个容器（可空，缺省返回全部）"}
				},
				"required": ["namespace", "pod"],
				"additionalProperties": false
			}`),
		},
		func(ctx context.Context, args json.RawMessage) (tool.Result, error) {
			return runK8sMetrics(ctx, coll, args)
		},
	)
}

// runK8sMetrics 解码 → 采集用量 →（可选）按容器过滤 → 序列化。
func runK8sMetrics(ctx context.Context, coll *k8s.Collector, args json.RawMessage) (tool.Result, error) {
	in, err := decodePodArgsWithContainer(args)
	if err != nil {
		return k8sFail(err.Error())
	}
	m, err := coll.PodMetrics(ctx, in.Namespace, in.Pod)
	if err != nil {
		return k8sFail("读取指标失败: " + k8s.ExplainError(err))
	}
	if in.Container != "" {
		kept := make([]k8s.ContainerMetricsView, 0, 1)
		for _, cm := range m.Containers {
			if cm.Name == in.Container {
				kept = append(kept, cm)
			}
		}
		if len(kept) == 0 {
			return k8sFail(fmt.Sprintf("容器 %q 不在该 Pod 的指标里（容器名可由 k8s_pod 的返回得到）", in.Container))
		}
		m.Containers = kept
	}
	return k8sJSON(m)
}

// --- k8s_node ---

// K8sNode 读节点摘要（Pending 归因要对比节点可分配余量与 taints）。
func K8sNode(coll *k8s.Collector) tool.Tool {
	return tool.NewFunc(
		tool.Spec{
			Name: "k8s_node",
			Description: "读取节点摘要：Ready 与压力类 conditions、可分配量（allocatable）、taints、拓扑标签、" +
				"已有 Pod 的 requests 合计与剩余可分配量（free_on_node = allocatable − requests，判“还放得下吗”看它）、" +
				"以及该节点上占用资源的 Pod 数。怀疑某个节点资源/污点问题时调用；节点名来自 k8s_pod 返回的 node_name——" +
				"Pod 还在 Pending 时没有 node_name，那种情况用 k8s_nodes 列全部节点。",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"node": {"type": "string", "description": "节点名，如 minikube"}
				},
				"required": ["node"],
				"additionalProperties": false
			}`),
		},
		func(ctx context.Context, args json.RawMessage) (tool.Result, error) {
			return runK8sNode(ctx, coll, args)
		},
	)
}

// nodeArgs 节点类工具入参（字段必须带 tag：node 与 Name 不做 tag 永远匹配不上）。
type nodeArgs struct {
	Node string `json:"node"`
}

// decodeNodeArgs 解码并校验 node。
func decodeNodeArgs(args json.RawMessage) (nodeArgs, error) {
	var in nodeArgs
	if err := json.Unmarshal(args, &in); err != nil {
		return in, fmt.Errorf("参数错误: %v", err)
	}
	if in.Node == "" {
		return in, fmt.Errorf("参数错误: node 必填")
	}
	return in, nil
}

// runK8sNode 解码 → 校验 → 节点摘要 →（附加）节点上的 Pod 数 → 序列化。
func runK8sNode(ctx context.Context, coll *k8s.Collector, args json.RawMessage) (tool.Result, error) {
	in, err := decodeNodeArgs(args)
	if err != nil {
		return k8sFail(err.Error())
	}
	nv, err := coll.Node(ctx, in.Node)
	if err != nil {
		return k8sFail("读取节点失败: " + k8s.ExplainError(err))
	}
	// 分配汇总失败不报错：节点自身摘要仍有用，原因已在视图的 allocation_error 字段里说明
	// （与 Collect 的降级口径一致）。
	return k8sJSON(nv)
}

// --- k8s_nodes ---

// K8sNodes 列全部节点摘要。为什么与 k8s_node 分开而不是把 node 参数改成可选：
// 前者按节点名取（答案唯一、输出小），后者是"扫一遍候选"（Pending 归因的入口）。
// 合成一个工具会让"没给名字"变成含糊语义，而这两件事的调用时机本来就不同。
func K8sNodes(coll *k8s.Collector) tool.Tool {
	return tool.NewFunc(
		tool.Spec{
			Name: "k8s_nodes",
			Description: "列出全部节点的摘要（名称、Ready 与压力类 conditions、可分配量 allocatable、taints、标签、" +
				"已有 Pod 的 requests 合计、剩余可分配量 free_on_node、节点上的 Pod 数）。" +
				"Pod 处于 Pending 时用它：Pending 的 Pod 没有 node_name（没被调度就不可能绑定节点），" +
				"k8s_node 无从下手，而判“nodeSelector/affinity 要的标签在不在”“taint 有没有被容忍”" +
				"“requests 是否超可分配”都要节点这一侧的数字。节点少时整表返回，" +
				"节点多时先用它挑出可疑节点，再用 k8s_node 看单个节点。",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {},
				"additionalProperties": false
			}`),
		},
		func(ctx context.Context, args json.RawMessage) (tool.Result, error) {
			return runK8sNodes(ctx, coll, args)
		},
	)
}

// runK8sNodes 采集 → 序列化（无参数，故不与 decode* 一族同形）。
func runK8sNodes(ctx context.Context, coll *k8s.Collector, _ json.RawMessage) (tool.Result, error) {
	views, err := coll.Nodes(ctx)
	if err != nil {
		return k8sFail("读取节点列表失败: " + k8s.ExplainError(err))
	}
	return k8sJSON(views)
}

// --- k8s_evidence ---

// K8sEvidence 一次收集六类证据并落盘（诊断起点）。
func K8sEvidence(coll *k8s.Collector) tool.Tool {
	return tool.NewFunc(
		tool.Spec{
			Name: "k8s_evidence",
			Description: "一次收集某个 Pod 的全部证据并落盘：Pod 现状、工作负载规格、最近事件、容器日志（含上一次运行）、" +
				"实时用量、所在节点、Pod 引用的存储申请（PVC，含 claim 自己的事件）；" +
				"已按 token 预算裁剪，返回里的 notes 会列出取不到的来源（降级说明）。" +
				"诊断开始时先调用它，之后再用 k8s_logs / k8s_metrics / k8s_node 等按需深挖。",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"namespace": {"type": "string", "description": "命名空间"},
					"pod": {"type": "string", "description": "Pod 名"}
				},
				"required": ["namespace", "pod"],
				"additionalProperties": false
			}`),
		},
		func(ctx context.Context, args json.RawMessage) (tool.Result, error) {
			return runK8sEvidence(ctx, coll, args)
		},
	)
}

// runK8sEvidence 解码 → 收集 → 落盘 → 回灌（落盘路径 + 证据包 JSON）。
func runK8sEvidence(ctx context.Context, coll *k8s.Collector, args json.RawMessage) (tool.Result, error) {
	in, err := decodePodArgs(args)
	if err != nil {
		return k8sFail(err.Error())
	}
	ev, err := coll.Collect(ctx, in.Namespace, in.Pod)
	if err != nil {
		return k8sFail("收集证据失败: " + k8s.ExplainError(err))
	}
	// 先落盘再回灌：模型拿到的是完整证据，文件用于回放与人工核对（路径由命名约定确定）。
	if _, err := coll.Save(ev); err != nil {
		// 落盘失败按业务失败回灌：模型可提示用户，不必中断整轮循环。
		return k8sFail("证据包落盘失败: " + err.Error())
	}
	data, err := ev.JSON()
	if err != nil {
		// 视图是我们自己的结构，序列化失败属契约破坏 → 框架级错误
		return tool.Result{}, fmt.Errorf("k8s tool: marshal evidence: %w", err)
	}
	// 回纯 JSON：证据包是模型要逐字段读的东西，掺一句中文前缀会让整段不再是合法 JSON
	// （评测脚本与任何下游解析都会被迫先剥前缀）。落点由文件名约定给出（<ns>-<pod>.evidence.json）。
	return tool.Result{Data: data}, nil
}

// --- k8s_report ---

// reportParams k8s_report 的参数契约模板：两处 %s 由 internal/k8s 的词表渲染填入
// （根因类别的 enum 与图例）。词表只维护在采集包一处，这里不复制，避免两处各自漂移。
const reportParams = `{
				"type": "object",
				"properties": {
					"namespace": {"type": "string", "description": "命名空间"},
					"pod": {"type": "string", "description": "Pod 名"},
					"symptoms": {"type": "array", "minItems": 1, "items": {"type": "string"},
						"description": "症状列表，如 OOMKilled / CrashLoopBackOff / ImagePullBackOff"},
					"root_cause": {
						"type": "object",
						"properties": {
							"summary": {"type": "string", "description": "一句话根因"},
							"category": {"type": "string", "enum": %s,
								"description": "根因类别（必填，只能取下列值）。图例：%s"},
							"confidence": {"type": "number", "minimum": 0, "maximum": 1, "description": "置信度 0-1"}
						},
						"required": ["summary", "category", "confidence"],
						"additionalProperties": false
					},
					"evidence": {
						"type": "array", "minItems": 1,
						"items": {
							"type": "object",
							"properties": {
								"source": {"type": "string", "description": "来源工具名，如 k8s_logs"},
								"ref": {"type": "string", "description": "对象/字段/时间"},
								"excerpt": {"type": "string", "description": "原文片段（日志行/事件 message）"},
								"supports": {"type": "string", "description": "这条证据支持什么判断"}
							},
							"required": ["source", "supports"],
							"additionalProperties": false
						}
					},
					"ruled_out": {
						"type": "array",
						"items": {
							"type": "object",
							"properties": {"summary": {"type": "string"}, "reason": {"type": "string"}},
							"required": ["summary", "reason"], "additionalProperties": false
						}
					},
					"missing_evidence": {
						"type": "array",
						"items": {
							"type": "object",
							"properties": {"item": {"type": "string"}, "impact": {"type": "string"}},
							"required": ["item", "impact"], "additionalProperties": false
						}
					},
					"suggestions": {
						"type": "array", "minItems": 1,
						"items": {
							"type": "object",
							"properties": {
								"action": {"type": "string"},
								"rationale": {"type": "string"},
								"risk": {"type": "string"}
							},
							"required": ["action"], "additionalProperties": false
						}
					},
					"alternatives": {
						"type": "array",
						"items": {
							"type": "object",
							"properties": {"summary": {"type": "string"}, "confidence": {"type": "number", "minimum": 0, "maximum": 1}},
							"required": ["summary", "confidence"], "additionalProperties": false
						}
					}
				},
				"required": ["namespace", "pod", "symptoms", "root_cause", "evidence", "suggestions"],
				"additionalProperties": false
			}`

// K8sReport 提交结构化诊断报告并落盘（report.json + report.md）。
func K8sReport(coll *k8s.Collector) tool.Tool {
	return tool.NewFunc(
		tool.Spec{
			Name: "k8s_report",
			Description: "提交结构化诊断报告并落盘：根因（含置信度）、证据链（每条必须带来源）、被否候选、缺失证据、" +
				"建议动作、备选根因。证据不足时把缺什么写进 missing_evidence 并压低 root_cause.confidence；" +
				"工具会校验：confidence ≥0.80 需要至少两条独立来源的证据，不满足会被打回（需补证据或降档）；" +
				"提交成功返回落盘路径，便于用户查看 report.md。",
			Parameters: json.RawMessage(fmt.Sprintf(reportParams,
				k8s.CategoryValuesJSON(), k8s.CategoryLegend())),
		},
		func(ctx context.Context, args json.RawMessage) (tool.Result, error) {
			return runK8sReport(ctx, coll, args)
		},
	)
}

// reportArgs 报告入参：嵌入 k8s.ReportView，让报告字段只有一处定义（schema 与结构不会漂移）；
// 外面只补 namespace/pod 两个定位参数——报告里的 Target 由工具填，模型不用管。
type reportArgs struct {
	Namespace string `json:"namespace"`
	Pod       string `json:"pod"`
	k8s.ReportView
}

// decodeReportArgs 解码并只校结构与取值域：证据完备度的"规则"（见 checkConfidenceRules）不在这一层，
// 分开是为了让"格式不对"与"结论不成立"两类回灌各自说清；两处都不做静默修正——报告里的数字必须与
// 模型声明一致（jjj §10.8 决策 4）。
func decodeReportArgs(args json.RawMessage) (reportArgs, error) {
	var in reportArgs
	if err := json.Unmarshal(args, &in); err != nil {
		return in, fmt.Errorf("参数错误: %v", err)
	}
	switch {
	case in.Namespace == "" || in.Pod == "":
		return in, fmt.Errorf("参数错误: namespace 与 pod 均必填")
	case len(in.Symptoms) == 0:
		return in, fmt.Errorf("参数错误: symptoms 至少一条")
	case in.RootCause.Summary == "":
		return in, fmt.Errorf("参数错误: root_cause.summary 不能为空")
	case !k8s.IsRootCauseCategory(in.RootCause.Category):
		return in, fmt.Errorf("参数错误: root_cause.category 必须取自参数说明里的词表（收到 %q）；"+
			"确实归不进任何一类时用 other，并在 summary 里写清是什么、缺哪条证据", in.RootCause.Category)
	case in.RootCause.Confidence < 0 || in.RootCause.Confidence > 1:
		return in, fmt.Errorf("参数错误: root_cause.confidence 需在 0-1 之间")
	case len(in.Evidence) == 0:
		return in, fmt.Errorf("参数错误: evidence 至少一条（没有证据的结论不应提交）")
	case len(in.Suggestions) == 0:
		return in, fmt.Errorf("参数错误: suggestions 至少一条")
	}
	for i, e := range in.Evidence {
		if e.Source == "" {
			return in, fmt.Errorf("参数错误: evidence[%d].source 不能为空（每条证据要能溯源）", i)
		}
	}
	return in, nil
}

// highConfidenceMin 是"需要至少两条独立证据"的置信度门槛（设计文档 §7.2 的第一档）。
const highConfidenceMin = 0.80

// checkConfidenceRules 校验 §7.2 里代码判得了的那部分：置信度 ≥0.80 时，至少要两条独立证据
// （条数 ≥2 且来源去重 ≥2）。不通过就返回错误，由调用方以业务失败回灌给模型自己改——
// 不静默把数字改小：报告里的置信度要么是模型自己的判断，要么这份报告不成立。
//
// 边界为什么只到这儿（2026-09-15 拿 14 份满分报告实测后定的）：
//   - "含一条决定性证据"要读语义，代码判不了；留给提示词，并靠评测的置信度档位项扣分兜底。
//   - "关键证据缺失时上限 0.79"里的"关键"同样判不了。实测 14 份报告里 13 份都申报了缺失证据
//     （模型自己标注"不影响结论"）且置信度 ≥0.80；按"有缺失就压到 0.79"硬套会把这 13 份正确、
//     如实的报告全部打回——把"如实申报"当成错误，正好与"降级可见"的原则相反。
func checkConfidenceRules(in reportArgs) error {
	if in.RootCause.Confidence < highConfidenceMin {
		return nil
	}
	sources := map[string]bool{}
	for _, e := range in.Evidence {
		sources[e.Source] = true
	}
	if len(in.Evidence) >= 2 && len(sources) >= 2 {
		return nil
	}
	return fmt.Errorf("置信度 %.2f 但证据只有 %d 条、来自 %d 个来源：按规则 ≥0.80 的结论需要至少两条"+
		"独立证据。请补证据（用原子工具取，别把同一条证据拆成两条写），或把置信度降到 0.80 以下"+
		"并说明还缺什么", in.RootCause.Confidence, len(in.Evidence), len(sources))
}

// runK8sReport 解码 → 填目标 → 落盘 → 回灌落盘路径与摘要（不回灌全文，省 token）。
// ctx 未用：落盘是本地文件操作，不需要集群调用。
func runK8sReport(_ context.Context, coll *k8s.Collector, args json.RawMessage) (tool.Result, error) {
	in, err := decodeReportArgs(args)
	if err != nil {
		return k8sFail(err.Error())
	}
	// 结论层面的规则校验：不通过就回灌给模型自己改（不替它改数字）。
	if err := checkConfidenceRules(in); err != nil {
		return k8sFail(err.Error())
	}
	report := in.ReportView
	// Target 以工具收到的 namespace/pod 为准；context 与 schema_version/generated_at 由
	// SaveReportView 补齐（模型既不该给也拿不到），故这里先清零后两项。
	report.Target = k8s.Target{Namespace: in.Namespace, Pod: in.Pod}
	report.SchemaVersion, report.GeneratedAt = 0, ""

	jsonPath, mdPath, err := coll.SaveReportView(report)
	if err != nil {
		return k8sFail("报告落盘失败: " + err.Error())
	}
	// 回灌"落盘路径 + 渲染后的 md"：模型可直接把这段交给用户，格式与落盘文件一致（不需自己重排）。
	return tool.Result{Data: fmt.Sprintf(
		"报告已落盘: %s\n人读版: %s\n\n%s",
		jsonPath, mdPath, k8s.RenderReportMD(report))}, nil
}
