package k8s

// 裁剪视图
//
// 这里是"视图即是白名单"的落点 -- 不是把原始对象序列化后再删除字段，而是构造时就只拷贝
// 排查所需字段，因此 managedFields、resourceVersion、last-applied-configuration 这类
// 噪声（单个 Pod 可达数十 KB）天然进不来（设计文档 §4.3）。
// 字段名用 snake_case JSON 标签，工具直接把它序列化回灌模型。

// PodView Pod 现状摘要：症状判定（phase / waiting reason / 上次终止原因 / 重启次数）与
// 规格线索（resources / probes / command / 挂载）都在这里，是诊断的第一手证据。
type PodView struct {
	Namespace      string `json:"namespace"`
	Name           string `json:"name"`
	UID            string `json:"uid,omitempty"`
	Phase          string `json:"phase,omitempty"`
	QOSClass       string `json:"qos_class,omitempty"`
	NodeName       string `json:"node_name,omitempty"`
	RestartPolicy  string `json:"restart_policy,omitempty"`
	ServiceAccount string `json:"service_account,omitempty"`
	// ImagePullSecrets Pod spec 上声明的拉取凭据（secret 名）。
	ImagePullSecrets []string `json:"image_pull_secrets,omitempty"`
	// ServiceAccountPullSecrets 该 Pod 的 ServiceAccount 上配置的拉取凭据：k8s 1.24 起
	// 不再复制进 Pod spec，但 kubelet 拉镜像时同样生效——只看 ImagePullSecrets 会误判"没配凭据"。
	ServiceAccountPullSecrets []string `json:"sa_pull_secrets,omitempty"`
	// ServiceAccountReadError 读 ServiceAccount 失败的原因：有值说明"凭据面没看全"，
	// 此时上面的列表为空不代表没配凭据（误导性证据的经典来源，同 Notes 的用意）。
	ServiceAccountReadError string            `json:"sa_read_error,omitempty"`
	CreatedAt               string            `json:"created_at,omitempty"`
	DeletedAt               string            `json:"deleted_at,omitempty"`
	Labels                  map[string]string `json:"labels,omitempty"`
	Annotations             map[string]string `json:"annotations,omitempty"`
	OwnerRefs               []OwnerRefView    `json:"owner_refs,omitempty"`
	Conditions              []ConditionView   `json:"conditions,omitempty"`
	Containers              []ContainerView   `json:"containers,omitempty"`
	InitContainers          []ContainerView   `json:"init_containers,omitempty"`
	Volumes                 []VolumeView      `json:"volumes,omitempty"`
	NodeSelector            map[string]string `json:"node_selector,omitempty"`
	Tolerations             []string          `json:"tolerations,omitempty"`
	// Affinity 调度亲和性（Pending 归因的"约束是否满足"那一问）。
	Affinity *AffinityView `json:"affinity,omitempty"`
}

// OwnerRefView owner 引用：由它逐级上溯到顶层工作负载（Pod→ReplicaSet→Deployment）。
type OwnerRefView struct {
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	APIVersion string `json:"api_version,omitempty"`
	Controller bool   `json:"controller,omitempty"`
}

// ConditionView 状态条件（Pod 与 Node 共用；只留排查关心的四要素）。
type ConditionView struct {
	Type               string `json:"type"`
	Status             string `json:"status"`
	Reason             string `json:"reason,omitempty"`
	Message            string `json:"message,omitempty"`
	LastTransitionTime string `json:"last_transition_time,omitempty"`
}

type VolumeView struct {
	Name   string `json:"name"`
	Source string `json:"source,omitempty"` // 如 persistentVolumeClaim/my-claim、configMap/app-config
}

// ContainerView 容器摘要（规格 + 状态合体）：
// 状态字段来自 containerStatuses（含 state 与 lastState），规格字段来自 spec.containers。
// 工作负载模板复用本结构（只填规格字段），避免再造一套近似类型。
type ContainerView struct {
	Name         string `json:"name"`
	Image        string `json:"image,omitempty"`
	Ready        bool   `json:"ready,omitempty"`
	RestartCount int32  `json:"restart_count,omitempty"`

	// State 当前状态：running / waiting / terminated。
	State string `json:"state,omitempty"`
	// StateReason waiting 或 terminated 的原因——CrashLoopBackOff / ImagePullBackOff /
	// OOMKilled / Error 都在此字段，是症状判定的核心。
	StateReason  string `json:"state_reason,omitempty"`
	StateMessage string `json:"state_message,omitempty"`
	StartedAt    string `json:"started_at,omitempty"`
	FinishedAt   string `json:"finished_at,omitempty"`
	ExitCode     *int32 `json:"exit_code,omitempty"`
	Signal       *int32 `json:"signal,omitempty"`

	// LastState* 上一次运行（上次终止）：OOMKilled 最常见的位置就是 LastStateReason，
	// 只看当前状态会漏判，故单列一组字段。
	LastState           string `json:"last_state,omitempty"`
	LastStateReason     string `json:"last_state_reason,omitempty"`
	LastStateMessage    string `json:"last_state_message,omitempty"`
	LastStateExitCode   *int32 `json:"last_state_exit_code,omitempty"`
	LastStateStartedAt  string `json:"last_state_started_at,omitempty"`
	LastStateFinishedAt string `json:"last_state_finished_at,omitempty"`

	// 规格字段（工作负载模板只填这一组）。
	Requests map[string]string `json:"requests,omitempty"`
	Limits   map[string]string `json:"limits,omitempty"`
	Command  []string          `json:"command,omitempty"`
	Args     []string          `json:"args,omitempty"`
	EnvFrom  []string          `json:"env_from,omitempty"`
	// Env 环境变量的"名与来源"（不取明文值，见 format.go 的 envRefs）：
	// CreateContainerConfigError（引用的 cm/secret 少了 key）这类启动失败靠它定位。
	Env          []string    `json:"env,omitempty"`
	Probes       []ProbeView `json:"probes,omitempty"`
	VolumeMounts []string    `json:"volume_mounts,omitempty"`
}

// ProbeView 探针摘要：Probe Failed 类故障要拿它对比"失败阈值与启动耗时"。
type ProbeView struct {
	Type                string `json:"type"` // liveness / readiness / startup
	Handler             string `json:"handler,omitempty"`
	InitialDelaySeconds int32  `json:"initial_delay_seconds,omitempty"`
	PeriodSeconds       int32  `json:"period_seconds,omitempty"`
	TimeoutSeconds      int32  `json:"timeout_seconds,omitempty"`
	FailureThreshold    int32  `json:"failure_threshold,omitempty"`
	SuccessThreshold    int32  `json:"success_threshold,omitempty"`
}

// AffinityView 亲和性摘要。硬软必须分层：required 不满足就是调度失败（Pending 根因候选），
// preferred 只影响打分（不满足也照样调度）——混成一个列表会让模型把"偏好没满足"报成根因。
// 三类规则各自独立（节点亲和、Pod 亲和、Pod 反亲和），宁可空着也不合并。
type AffinityView struct {
	NodeAffinity    *AffinityRules `json:"node_affinity,omitempty"`
	PodAffinity     *AffinityRules `json:"pod_affinity,omitempty"`
	PodAntiAffinity *AffinityRules `json:"pod_anti_affinity,omitempty"`
}

// AffinityRules 一组亲和规则的渲染文本：一条规则一个元素（多项之间是 OR 的语义在文本里写清）。
type AffinityRules struct {
	// Required 对应 requiredDuringScheduling...：硬门槛，不满足即 Pending。
	Required []string `json:"required,omitempty"`
	// Preferred 对应 preferredDuringScheduling...：软偏好，带 weight。
	Preferred []string `json:"preferred,omitempty"`
}

// WorkloadView 顶层工作负载规格摘要：OOMKilled / Probe Failed 的"验证"步要回查
// limits 与探针配置是否合理，只看 Pod 看不到这些（Pod 上的值是模板渲染后的结果）。
type WorkloadView struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	// Replicas 期望副本数（Deployment / StatefulSet / ReplicaSet 有；DaemonSet 没有）。
	// Job 也没有这个概念，故不填——它的"要跑几个"是 Completions，见 Job 块。
	Replicas          int32           `json:"replicas,omitempty"`
	ReadyReplicas     int32           `json:"ready_replicas,omitempty"`
	AvailableReplicas int32           `json:"available_replicas,omitempty"`
	Unavailable       int32           `json:"unavailable_replicas,omitempty"`
	Strategy          string          `json:"strategy,omitempty"`
	CreatedAt         string          `json:"created_at,omitempty"`
	Conditions        []ConditionView `json:"conditions,omitempty"`
	OwnerRefs         []OwnerRefView  `json:"owner_refs,omitempty"`
	Template          PodTemplateView `json:"pod_template"`
	// Job 批任务专有块（Kind=Job 时才有）。
	Job *JobView `json:"job,omitempty"`
}

// JobView 批任务的进度与失败策略。这些字段在 Deployment 语境里没有对应物：
// Completions 是"总共要成功几个"、Parallelism 是"允许同时跑几个"，硬塞进 replicas 会把
// "要成功 3 次"读成"保持 3 个副本"——误导性证据比缺字段更糟，故单列一块。
type JobView struct {
	// Completions 需成功完成的任务数；未指定（缺字段）时 k8s 按 1 处理。
	Completions *int32 `json:"completions,omitempty"`
	// Parallelism 允许同时运行的 Pod 数上限；未指定（缺字段）时按 1 处理。
	Parallelism *int32 `json:"parallelism,omitempty"`
	// 三个计数不带 omitempty：0 是"确实是 0"，省掉会被读成"没采到"。
	Active    int32 `json:"active"`
	Succeeded int32 `json:"succeeded"`
	Failed    int32 `json:"failed"`
	// BackoffLimit 失败重试上限；事件里的 BackoffLimitExceeded 与它对应。
	BackoffLimit *int32 `json:"backoff_limit,omitempty"`
	// Conditions Complete / Failed：Job 失败时 Failed=True 的 Reason 直接点出真因。
	Conditions []ConditionView `json:"conditions,omitempty"`
}

// PodTemplateView 工作负载的 Pod 模板规格摘要（容器只填规格字段，不带状态）。
type PodTemplateView struct {
	Containers   []ContainerView   `json:"containers,omitempty"`
	NodeSelector map[string]string `json:"node_selector,omitempty"`
	Tolerations  []string          `json:"tolerations,omitempty"`
	Affinity     *AffinityView     `json:"affinity,omitempty"`
	Volumes      []VolumeView      `json:"volumes,omitempty"`
}

// EventView 事件摘要：调度失败（Pending）、探针失败、拉镜像失败、容器退避都在这里。
// 同 type+reason+message 的事件已合并（count 记重复次数），Warning 优先排序。
type EventView struct {
	Type      string `json:"type"`
	Reason    string `json:"reason"`
	Message   string `json:"message,omitempty"`
	Count     int32  `json:"count,omitempty"`
	FirstSeen string `json:"first_seen,omitempty"`
	LastSeen  string `json:"last_seen,omitempty"`
	Source    string `json:"source,omitempty"`
	Object    string `json:"object,omitempty"`
}

// LogView 容器日志（当前或上次）：Truncated 标记是否按字节上限取了尾部，
// 让模型知道自己看的是截断后的片段而非全量。
type LogView struct {
	Container string `json:"container"`
	Previous  bool   `json:"previous,omitempty"`
	Lines     int    `json:"lines"`
	Truncated bool   `json:"truncated,omitempty"`
	Text      string `json:"text"`
}

// NodeView 节点摘要：Pending 归因要算"节点可分配余量 vs Pod requests"，
// 也要看 taints 与 conditions（节点压力类故障的表现）。
type NodeView struct {
	Name           string            `json:"name"`
	Ready          string            `json:"ready,omitempty"`
	KubeletVersion string            `json:"kubelet_version,omitempty"`
	CreatedAt      string            `json:"created_at,omitempty"`
	Conditions     []ConditionView   `json:"conditions,omitempty"`
	Allocatable    map[string]string `json:"allocatable,omitempty"`
	Capacity       map[string]string `json:"capacity,omitempty"`
	Taints         []string          `json:"taints,omitempty"`
	Labels         map[string]string `json:"labels,omitempty"`
	// PodsOnNode 该节点上占用资源的 Pod 数（终态 Succeeded/Failed 不计）。
	PodsOnNode int `json:"pods_on_node,omitempty"`
	// RequestsOnNode 已有 Pod 的 requests 合计——调度只看 requests，这是判"还放得下吗"的分母。
	RequestsOnNode map[string]string `json:"requests_on_node,omitempty"`
	// LimitsOnNode 已有 Pod 的 limits 合计（仅参考：调度不按 limits）。
	LimitsOnNode map[string]string `json:"limits_on_node,omitempty"`
	// FreeOnNode 剩余可分配 = allocatable − 已用（requests 合计；其中 pods 项按节点上非终态 Pod 数扣——
	// 它是个数配额，不是 requests 资源）。Pending 归因的核心数字；
	// 采集侧算好，避免模型自己加几十个 Pod 的 requests——既贵又容易错。
	FreeOnNode map[string]string `json:"free_on_node,omitempty"`
	// AllocationError 分配汇总失败的原因：有值时上面四项不可信（与 SA 读失败同款"降级可见"）。
	AllocationError string `json:"allocation_error,omitempty"`
}

// ContainerMetricsView 容器用量与限值：使用率（usage/limit）已在采集侧算好，
// 免去模型自己做单位换算与比值（内存 99% 这类判断依赖它）。
type ContainerMetricsView struct {
	Name          string   `json:"name"`
	CPU           string   `json:"cpu,omitempty"`
	Memory        string   `json:"memory,omitempty"`
	CPULimit      string   `json:"cpu_limit,omitempty"`
	MemoryLimit   string   `json:"memory_limit,omitempty"`
	CPUPercent    *float64 `json:"cpu_percent_of_limit,omitempty"`
	MemoryPercent *float64 `json:"memory_percent_of_limit,omitempty"`
}

// PodMetricsView Pod 实时用量（metrics.k8s.io，来自 metrics-server）。
type PodMetricsView struct {
	Namespace  string                 `json:"namespace"`
	Pod        string                 `json:"pod"`
	Timestamp  string                 `json:"timestamp,omitempty"`
	Containers []ContainerMetricsView `json:"containers,omitempty"`
}

// NodeMetricsView 节点实时用量：分母用 allocatable（节点没有 limit）。
type NodeMetricsView struct {
	Name       string   `json:"name"`
	Timestamp  string   `json:"timestamp,omitempty"`
	CPU        string   `json:"cpu,omitempty"`
	Memory     string   `json:"memory,omitempty"`
	CPUPercent *float64 `json:"cpu_percent_of_allocatable,omitempty"`
	MemPercent *float64 `json:"memory_percent_of_allocatable,omitempty"`
}

// Target 诊断目标标识（证据包与报告共用，保证两者能对齐同一对象）。
type Target struct {
	Context   string `json:"context,omitempty"`
	Namespace string `json:"namespace"`
	Pod       string `json:"pod"`
	// Workload 顶层工作负载标识，形如 Deployment/my-app；解析不到时为空。
	Workload string `json:"workload,omitempty"`
}

// PVCView 存储申请摘要：Pending 的"未绑定 PVC"分支靠它——phase 与 storage_class 组合能区分
// "供给失败"（Pending + 有 storageClass）与"没有可用 PV 也没法动态供给"（Pending + 无 storageClass）。
// UsedByVolumes 与 Events 由 Collect 填充（前者要知道 Pod 的卷名，后者是子对象事件的来源）。
type PVCView struct {
	Name             string   `json:"name"`
	UID              string   `json:"uid,omitempty"`
	Phase            string   `json:"phase,omitempty"` // Pending / Bound / Lost
	VolumeName       string   `json:"volume_name,omitempty"`
	StorageClass     string   `json:"storage_class,omitempty"`
	RequestedStorage string   `json:"requested_storage,omitempty"`
	AccessModes      []string `json:"access_modes,omitempty"`
	// UsedByVolumes 该 claim 被 Pod 的哪些卷引用（多个卷复用同一 claim 时能看出来）。
	UsedByVolumes []string `json:"used_by_volumes,omitempty"`
	// Events PVC 自己的事件：子对象事件不在"按 Pod 的 involvedObject"过滤范围内，
	// 必须单独按 uid 取一次，否则"这块 PVC 为什么没绑上"永远看不到。
	Events []EventView `json:"events,omitempty"`
	// EventsError 取 PVC 事件失败的原因（有值时 Events 为空不代表"没有事件"）。
	EventsError string `json:"events_error,omitempty"`
}

// NoteKind 降级说明的类别。分级只为一件事：把"环境抖动/可选源不可用"与"证据真缺"分开——
// 纯文本混在一起时，测试与评测只能整体宽松或整体严格：宽松会放过真缺口，严格会把
// metrics-server 重启窗口记成采集缺陷（2026-09-12 真踩过）。
// 口径：
//   - required_failed：必需来源失败，证据不完整——结论要写进 missing_evidence 并压置信度
//   - optional_unavailable：可选来源不可用（metrics 类）——照样能诊断，不压置信度
//   - budget_trimmed：证据本来有，被 token 预算砍掉了（与"本来就没有"是两回事）
//   - no_data：来源正常但没有数据（如容器从未启动所以没有日志）——不是失败
type NoteKind string

const (
	NoteRequired NoteKind = "required_failed"
	NoteOptional NoteKind = "optional_unavailable"
	NoteTrimmed  NoteKind = "budget_trimmed"
	NoteNoData   NoteKind = "no_data"
)

// NoteView 一条降级说明（Kind 决定它算不算"缺失证据"）。
type NoteView struct {
	Kind    NoteKind `json:"kind"`
	Message string   `json:"message"`
}

// Evidence 证据包（Context Builder 的产物）：一次诊断看到的全部裁剪后证据。
// Notes 记录采集期的降级（如 metrics-server 不可用、无上次日志）——降级必须显式可见，
// 它是模型判定"缺失证据"与压低置信度的依据，故每条都带类别（见 NoteKind）。
type Evidence struct {
	CollectedAt string           `json:"collected_at"`
	Target      Target           `json:"target"`
	Pod         *PodView         `json:"pod"`
	Workload    *WorkloadView    `json:"workload,omitempty"`
	Events      []EventView      `json:"events,omitempty"`
	Logs        []LogView        `json:"logs,omitempty"`
	Metrics     *PodMetricsView  `json:"metrics,omitempty"`
	Node        *NodeView        `json:"node,omitempty"`
	NodeMetrics *NodeMetricsView `json:"node_metrics,omitempty"`
	// PVCs Pod 引用的存储申请（含各自的子对象事件）：Pending 的"未绑定"分支与
	// CrashLoop 里的"挂载/写盘失败"都靠它，挂在 Pod 上的事件看不到 claim 侧的进展。
	PVCs  []PVCView  `json:"pvcs,omitempty"`
	Notes []NoteView `json:"notes,omitempty"`
}
