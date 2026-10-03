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
	Namespace      string `json:"namespace"`                 // 命名空间
	Name           string `json:"name"`                      // Pod 名
	UID            string `json:"uid,omitempty"`             // 对象 UID（事件按它过滤）
	Phase          string `json:"phase,omitempty"`           // Pending / Running / Succeeded / Failed / Unknown
	QOSClass       string `json:"qos_class,omitempty"`       // Guaranteed / Burstable / BestEffort
	NodeName       string `json:"node_name,omitempty"`       // 绑定的节点（Pending 时为空）
	RestartPolicy  string `json:"restart_policy,omitempty"`  // Always / OnFailure / Never
	ServiceAccount string `json:"service_account,omitempty"` // 使用的 ServiceAccount
	// ImagePullSecrets Pod spec 上声明的拉取凭据（secret 名）。
	ImagePullSecrets []string `json:"image_pull_secrets,omitempty"`
	// ServiceAccountPullSecrets 该 Pod 的 ServiceAccount 上配置的拉取凭据：k8s 1.24 起
	// 不再复制进 Pod spec，但 kubelet 拉镜像时同样生效——只看 ImagePullSecrets 会误判"没配凭据"。
	ServiceAccountPullSecrets []string `json:"sa_pull_secrets,omitempty"`
	// ServiceAccountReadError 读 ServiceAccount 失败的原因：有值说明"凭据面没看全"，
	// 此时上面的列表为空不代表没配凭据（误导性证据的经典来源，同 Notes 的用意）。
	// 连接类失败已归因（集群不可达/调用超时），见 k8s-diagnosis.md §17.2。
	ServiceAccountReadError string            `json:"sa_read_error,omitempty"`
	CreatedAt               string            `json:"created_at,omitempty"`      // 创建时间（RFC3339）
	DeletedAt               string            `json:"deleted_at,omitempty"`      // 删除时间（正在删除中才有值）
	Labels                  map[string]string `json:"labels,omitempty"`          // 标签
	Annotations             map[string]string `json:"annotations,omitempty"`     // 白名单注解（见 pickAnnotations）
	OwnerRefs               []OwnerRefView    `json:"owner_refs,omitempty"`      // 归属链（Pod→ReplicaSet→Deployment）
	Conditions              []ConditionView   `json:"conditions,omitempty"`      // 状态条件（PodScheduled / Ready …）
	Containers              []ContainerView   `json:"containers,omitempty"`      // 主容器（规格 + 状态）
	InitContainers          []ContainerView   `json:"init_containers,omitempty"` // 初始化容器
	Volumes                 []VolumeView      `json:"volumes,omitempty"`         // 卷来源
	NodeSelector            map[string]string `json:"node_selector,omitempty"`   // 节点标签选择（全部命中才可调度）
	Tolerations             []string          `json:"tolerations,omitempty"`     // 容忍（对抗节点 taints）
	// Affinity 调度亲和性（Pending 归因的"约束是否满足"那一问）。
	Affinity *AffinityView `json:"affinity,omitempty"`
}

// OwnerRefView owner 引用：由它逐级上溯到顶层工作负载（Pod→ReplicaSet→Deployment）。
type OwnerRefView struct {
	Kind       string `json:"kind"`                  // 类型（Deployment / ReplicaSet / StatefulSet …）
	Name       string `json:"name"`                  // 名称
	APIVersion string `json:"api_version,omitempty"` // API 版本（apps/v1 等）
	Controller bool   `json:"controller,omitempty"`  // 是否为控制器（上溯只看它）
}

// ConditionView 状态条件（Pod 与 Node 共用；只留排查关心的四要素）。
type ConditionView struct {
	Type               string `json:"type"`                           // 条件类型（Ready / PodScheduled / MemoryPressure …）
	Status             string `json:"status"`                         // True / False / Unknown
	Reason             string `json:"reason,omitempty"`               // 机器可读的原因
	Message            string `json:"message,omitempty"`              // 人读说明
	LastTransitionTime string `json:"last_transition_time,omitempty"` // 最近一次变更时间
}

type VolumeView struct {
	Name   string `json:"name"`             // 卷名
	Source string `json:"source,omitempty"` // 如 persistentVolumeClaim/my-claim、configMap/app-config
}

// ContainerView 容器摘要（规格 + 状态合体）：
// 状态字段来自 containerStatuses（含 state 与 lastState），规格字段来自 spec.containers。
// 工作负载模板复用本结构（只填规格字段），避免再造一套近似类型。
type ContainerView struct {
	Name         string `json:"name"`                    // 容器名
	Image        string `json:"image,omitempty"`         // 镜像
	Ready        bool   `json:"ready,omitempty"`         // 就绪（readiness 通过）
	RestartCount int32  `json:"restart_count,omitempty"` // 重启次数

	// State 当前状态：running / waiting / terminated。
	State string `json:"state,omitempty"`
	// StateReason waiting 或 terminated 的原因——CrashLoopBackOff / ImagePullBackOff /
	// OOMKilled / Error 都在此字段，是症状判定的核心。
	StateReason  string `json:"state_reason,omitempty"`
	StateMessage string `json:"state_message,omitempty"` // 状态说明（waiting 的 message 常含真因）
	StartedAt    string `json:"started_at,omitempty"`    // 本次启动时间
	FinishedAt   string `json:"finished_at,omitempty"`   // 本次结束时间（terminated 才有）
	ExitCode     *int32 `json:"exit_code,omitempty"`     // 退出码（terminated 才有）
	Signal       *int32 `json:"signal,omitempty"`        // 终止信号（如 9=SIGKILL）

	// LastState* 上一次运行（上次终止）：OOMKilled 最常见的位置就是 LastStateReason，
	// 只看当前状态会漏判，故单列一组字段。
	LastState           string `json:"last_state,omitempty"`             // 上次状态（running / terminated）
	LastStateReason     string `json:"last_state_reason,omitempty"`      // 上次终止原因（OOMKilled 常在此）
	LastStateMessage    string `json:"last_state_message,omitempty"`     // 上次状态说明
	LastStateExitCode   *int32 `json:"last_state_exit_code,omitempty"`   // 上次退出码
	LastStateStartedAt  string `json:"last_state_started_at,omitempty"`  // 上次启动时间
	LastStateFinishedAt string `json:"last_state_finished_at,omitempty"` // 上次结束时间

	// 规格字段（工作负载模板只填这一组）。
	Requests map[string]string `json:"requests,omitempty"` // 资源请求（调度依据）
	Limits   map[string]string `json:"limits,omitempty"`   // 资源限值（OOM 归因依据）
	Command  []string          `json:"command,omitempty"`  // 启动命令（覆盖镜像 ENTRYPOINT）
	Args     []string          `json:"args,omitempty"`     // 启动参数（覆盖镜像 CMD）
	EnvFrom  []string          `json:"env_from,omitempty"` // envFrom 来源（configMapRef/secretRef）
	// Env 环境变量的"名与来源"（不取明文值，见 format.go 的 envRefs）：
	// CreateContainerConfigError（引用的 cm/secret 少了 key）这类启动失败靠它定位。
	Env          []string    `json:"env,omitempty"`
	Probes       []ProbeView `json:"probes,omitempty"`        // 探针（liveness / readiness / startup）
	VolumeMounts []string    `json:"volume_mounts,omitempty"` // 挂载点（卷名:路径[:ro]）
}

// ProbeView 探针摘要：Probe Failed 类故障要拿它对比"失败阈值与启动耗时"。
type ProbeView struct {
	Type                string `json:"type"`                            // liveness / readiness / startup
	Handler             string `json:"handler,omitempty"`               // 探测动作（http/tcp/exec/grpc）
	InitialDelaySeconds int32  `json:"initial_delay_seconds,omitempty"` // 首次探测延迟（秒）
	PeriodSeconds       int32  `json:"period_seconds,omitempty"`        // 探测周期（秒）
	TimeoutSeconds      int32  `json:"timeout_seconds,omitempty"`       // 单次探测超时（秒）
	FailureThreshold    int32  `json:"failure_threshold,omitempty"`     // 连续失败几次判不健康
	SuccessThreshold    int32  `json:"success_threshold,omitempty"`     // 连续成功几次判健康
}

// AffinityView 亲和性摘要。硬软必须分层：required 不满足就是调度失败（Pending 根因候选），
// preferred 只影响打分（不满足也照样调度）——混成一个列表会让模型把"偏好没满足"报成根因。
// 三类规则各自独立（节点亲和、Pod 亲和、Pod 反亲和），宁可空着也不合并。
type AffinityView struct {
	NodeAffinity    *AffinityRules `json:"node_affinity,omitempty"`     // 节点亲和
	PodAffinity     *AffinityRules `json:"pod_affinity,omitempty"`      // Pod 亲和
	PodAntiAffinity *AffinityRules `json:"pod_anti_affinity,omitempty"` // Pod 反亲和
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
	Kind      string `json:"kind"`      // 类型（Deployment / StatefulSet / DaemonSet / Job）
	Namespace string `json:"namespace"` // 命名空间
	Name      string `json:"name"`      // 名称
	// Replicas 期望副本数（Deployment / StatefulSet / ReplicaSet 有；DaemonSet 没有）。
	// Job 也没有这个概念，故不填——它的"要跑几个"是 Completions，见 Job 块。
	Replicas          int32           `json:"replicas,omitempty"`
	ReadyReplicas     int32           `json:"ready_replicas,omitempty"`       // 就绪副本数
	AvailableReplicas int32           `json:"available_replicas,omitempty"`   // 可用副本数
	Unavailable       int32           `json:"unavailable_replicas,omitempty"` // 不可用副本数
	Strategy          string          `json:"strategy,omitempty"`             // 更新策略（RollingUpdate / Recreate）
	CreatedAt         string          `json:"created_at,omitempty"`           // 创建时间
	Conditions        []ConditionView `json:"conditions,omitempty"`           // 状态条件（Progressing / Available）
	OwnerRefs         []OwnerRefView  `json:"owner_refs,omitempty"`           // 归属链（已是顶层则为空）
	Template          PodTemplateView `json:"pod_template"`                   // Pod 模板摘要
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
	Active    int32 `json:"active"`    // 运行中个数
	Succeeded int32 `json:"succeeded"` // 成功个数
	Failed    int32 `json:"failed"`    // 失败个数
	// BackoffLimit 失败重试上限；事件里的 BackoffLimitExceeded 与它对应。
	BackoffLimit *int32 `json:"backoff_limit,omitempty"`
	// Conditions Complete / Failed：Job 失败时 Failed=True 的 Reason 直接点出真因。
	Conditions []ConditionView `json:"conditions,omitempty"`
}

// PodTemplateView 工作负载的 Pod 模板规格摘要（容器只填规格字段，不带状态）。
type PodTemplateView struct {
	Containers   []ContainerView   `json:"containers,omitempty"`    // 容器规格（不含状态）
	NodeSelector map[string]string `json:"node_selector,omitempty"` // 节点标签选择
	Tolerations  []string          `json:"tolerations,omitempty"`   // 容忍
	Affinity     *AffinityView     `json:"affinity,omitempty"`      // 亲和性
	Volumes      []VolumeView      `json:"volumes,omitempty"`       // 卷
}

// EventView 事件摘要：调度失败（Pending）、探针失败、拉镜像失败、容器退避都在这里。
// 同 type+reason+message 的事件已合并（count 记重复次数），Warning 优先排序。
type EventView struct {
	Type      string `json:"type"`                 // Normal / Warning
	Reason    string `json:"reason"`               // 原因（如 FailedScheduling / BackOff）
	Message   string `json:"message,omitempty"`    // 说明正文
	Count     int32  `json:"count,omitempty"`      // 合并后的累计次数
	FirstSeen string `json:"first_seen,omitempty"` // 首次发生时间
	LastSeen  string `json:"last_seen,omitempty"`  // 最近发生时间（排序按它倒序）
	Source    string `json:"source,omitempty"`     // 事件来源组件（kubelet / scheduler …）
	Object    string `json:"object,omitempty"`     // 涉及对象（Kind/Name）
}

// LogView 容器日志（当前或上次）：Truncated 标记是否按字节上限取了尾部，
// 让模型知道自己看的是截断后的片段而非全量。
type LogView struct {
	Container string `json:"container"`           // 容器名
	Previous  bool   `json:"previous,omitempty"`  // 是否上次运行（previous）的日志
	Lines     int    `json:"lines"`               // 行数
	Truncated bool   `json:"truncated,omitempty"` // 是否按字节上限取了尾部
	Text      string `json:"text"`                // 日志正文（截断时首行有提示）
}

// NodeView 节点摘要：Pending 归因要算"节点可分配余量 vs Pod requests"，
// 也要看 taints 与 conditions（节点压力类故障的表现）。
type NodeView struct {
	Name           string            `json:"name"`                      // 节点名
	Ready          string            `json:"ready,omitempty"`           // Ready 条件（True/False/Unknown）
	KubeletVersion string            `json:"kubelet_version,omitempty"` // kubelet 版本
	CreatedAt      string            `json:"created_at,omitempty"`      // 创建时间
	Conditions     []ConditionView   `json:"conditions,omitempty"`      // 节点条件（MemoryPressure / DiskPressure …）
	Allocatable    map[string]string `json:"allocatable,omitempty"`     // 可分配资源（余量分母）
	Capacity       map[string]string `json:"capacity,omitempty"`        // 总容量
	Taints         []string          `json:"taints,omitempty"`          // 污点（要与 tolerations 对着看）
	Labels         map[string]string `json:"labels,omitempty"`          // 标签（nodeSelector 命中的目标）
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
	// 连接类失败已归因（集群不可达/调用超时），见 k8s-diagnosis.md §17.2。
	AllocationError string `json:"allocation_error,omitempty"`
}

// ContainerMetricsView 容器用量与限值：使用率（usage/limit）已在采集侧算好，
// 免去模型自己做单位换算与比值（内存 99% 这类判断依赖它）。
type ContainerMetricsView struct {
	Name          string   `json:"name"`                              // 容器名
	CPU           string   `json:"cpu,omitempty"`                     // CPU 用量
	Memory        string   `json:"memory,omitempty"`                  // 内存用量
	CPULimit      string   `json:"cpu_limit,omitempty"`               // CPU 限值
	MemoryLimit   string   `json:"memory_limit,omitempty"`            // 内存限值
	CPUPercent    *float64 `json:"cpu_percent_of_limit,omitempty"`    // CPU 用量占限值百分比
	MemoryPercent *float64 `json:"memory_percent_of_limit,omitempty"` // 内存用量占限值百分比
}

// PodMetricsView Pod 实时用量（metrics.k8s.io，来自 metrics-server）。
type PodMetricsView struct {
	Namespace  string                 `json:"namespace"`            // 命名空间
	Pod        string                 `json:"pod"`                  // Pod 名
	Timestamp  string                 `json:"timestamp,omitempty"`  // 采样时间
	Containers []ContainerMetricsView `json:"containers,omitempty"` // 各容器用量
}

// NodeMetricsView 节点实时用量：分母用 allocatable（节点没有 limit）。
type NodeMetricsView struct {
	Name       string   `json:"name"`                                    // 节点名
	Timestamp  string   `json:"timestamp,omitempty"`                     // 采样时间
	CPU        string   `json:"cpu,omitempty"`                           // CPU 用量
	Memory     string   `json:"memory,omitempty"`                        // 内存用量
	CPUPercent *float64 `json:"cpu_percent_of_allocatable,omitempty"`    // CPU 用量占 allocatable 百分比
	MemPercent *float64 `json:"memory_percent_of_allocatable,omitempty"` // 内存用量占 allocatable 百分比
}

// Target 诊断目标标识（证据包与报告共用，保证两者能对齐同一对象）。
type Target struct {
	// APIServer 实际访问的 apiserver 地址（kubeconfig 的 server，New 里归一化）：回答"这份证据来自哪个端点"，
	// 回放与审计要用；context 只是名字，光有它看不出连的是哪个 IP（见 k8s-diagnosis.md §17.6）。
	APIServer string `json:"api_server,omitempty"`
	Context   string `json:"context,omitempty"` // 目标 kube context
	Namespace string `json:"namespace"`         // 命名空间
	Pod       string `json:"pod"`               // Pod 名
	// Workload 顶层工作负载标识，形如 Deployment/my-app；解析不到时为空。
	Workload string `json:"workload,omitempty"`
}

// PVCView 存储申请摘要：Pending 的"未绑定 PVC"分支靠它——phase 与 storage_class 组合能区分
// "供给失败"（Pending + 有 storageClass）与"没有可用 PV 也没法动态供给"（Pending + 无 storageClass）。
// UsedByVolumes 与 Events 由 Collect 填充（前者要知道 Pod 的卷名，后者是子对象事件的来源）。
type PVCView struct {
	Name             string   `json:"name"`                        // 名称
	UID              string   `json:"uid,omitempty"`               // 对象 UID（按它取子对象事件）
	Phase            string   `json:"phase,omitempty"`             // Pending / Bound / Lost
	VolumeName       string   `json:"volume_name,omitempty"`       // 绑定到的 PV 名（未绑定为空）
	StorageClass     string   `json:"storage_class,omitempty"`     // 存储类
	RequestedStorage string   `json:"requested_storage,omitempty"` // 申请容量
	AccessModes      []string `json:"access_modes,omitempty"`      // 访问模式（ReadWriteOnce 等）
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
	Kind    NoteKind `json:"kind"`    // 类别（决定是否算缺失证据）
	Message string   `json:"message"` // 说明
}

// Evidence 证据包（Context Builder 的产物）：一次诊断看到的全部裁剪后证据。
// Notes 记录采集期的降级（如 metrics-server 不可用、无上次日志）——降级必须显式可见，
// 它是模型判定"缺失证据"与压低置信度的依据，故每条都带类别（见 NoteKind）。
type Evidence struct {
	CollectedAt string           `json:"collected_at"`           // 采集时间
	Target      Target           `json:"target"`                 // 诊断目标
	Pod         *PodView         `json:"pod"`                    // Pod 现状摘要
	Workload    *WorkloadView    `json:"workload,omitempty"`     // 顶层工作负载
	Events      []EventView      `json:"events,omitempty"`       // 事件摘要
	Logs        []LogView        `json:"logs,omitempty"`         // 容器日志
	Metrics     *PodMetricsView  `json:"metrics,omitempty"`      // Pod 实时用量
	Node        *NodeView        `json:"node,omitempty"`         // 节点摘要
	NodeMetrics *NodeMetricsView `json:"node_metrics,omitempty"` // 节点实时用量
	// PVCs Pod 引用的存储申请（含各自的子对象事件）：Pending 的"未绑定"分支与
	// CrashLoop 里的"挂载/写盘失败"都靠它，挂在 Pod 上的事件看不到 claim 侧的进展。
	PVCs  []PVCView  `json:"pvcs,omitempty"`
	Notes []NoteView `json:"notes,omitempty"` // 采集期降级说明
}
