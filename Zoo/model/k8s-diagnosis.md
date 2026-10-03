# K8s 故障诊断助手（k8s-diag）设计文档

> 位置：`internal/k8s`（采集包）、`internal/tool/builtin/k8s.go`（工具）、`internal/workflow/workflows/k8s-diag.md`（诊断分支）、`Zoo/k8s-lab/`（模拟环境与场景）、`main.go` + `main_commands.go`（装配与 /diag）
> 状态：已落地（2026-09-15 与代码对账过一轮）——第 1、1.5、2、2.5 批已完成（见 §12），第 3 批（场景集与评测）待做
> 关联：`Zoo/model/tool.md`（工具三件套与依赖纪律）、`Zoo/model/workflow.md`（分支机制）、`Zoo/model/policy.md`（权限横切）、`Zoo/model/trace.md`、`Zoo/model/kb.md`（叶子部件先例）、`CLAUDE.md`

## 1. 定位

本模块是故障定位系统的推理层：采集由确定性代码完成（client-go 直连 API），归因由 LLM 在 agent 循环里完成，产出带证据链与置信度的 RCA 报告。

它是 small 的一条能力分支，不是新内核——复用 agent 循环、工具系统（Spec/Execute/Registry）、权限横切（policy）、会话与轨迹持久化。agent 循环零改动，符合"新模块接 agent 用包装 + 组合根装配"的既定模板。

## 2. 需求边界

本期做：Pod 生命周期异常六类的定位与归因——CrashLoopBackOff、OOMKilled、ImagePullBackOff、Pending、ContainerRestart、Probe Failed。输入为 namespace/pod；采集七类证据：pod 现状（含拉取凭据）、owner workload 规格、events、logs（current + previous）、metrics（Pod 与节点）、node 信息（含分配账本）、Pod 引用的 PVC（含 claim 自己的事件）；输出根因 + 置信度 + 证据链 + 建议 + 缺失证据。

本期不做：节点级故障（NotReady/DiskPressure/PIDPressure 的独立归因）、网络与 CNI、应用内部逻辑 bug（能由证据推出者除外，如内存泄漏）。严格只读——不执行任何改变集群状态的动作（不 delete / 不 restart / 不 scale），诊断与修复解耦。

## 3. 架构与依赖方向

```
CLI /diag 或自然语言
        |
workflow 分支 k8s-diag（提示词层：收集→分析→验证）
        |
agent 循环（现成）+ 权限横切（工具全 Pass）
        |
builtin 薄壳工具（Spec + 参数解码 + 采集请求）
        |
internal/k8s 采集包（client-go typed + 裁剪 + 证据包组装 + 产物落盘）
        |
kube-apiserver（对象/事件/日志） + metrics.k8s.io（metrics server）
```

依赖方向：

- `main → internal/k8s`：采集包是叶子部件，只 import client-go 与标准库，不 import 任何内部包（与 memory/kb 同构）。
- `main → builtin`：`Deps` 增加 K8s 字段，组合根装配注入。
- `builtin → internal/k8s`、`builtin → tool/policy`：单向无环。builtin 依旧不 import `agent/session/provider/config`（`imports_test.go` 固化的正是这条）。
- workflow 分支是纯 md 资产，无代码依赖。

两条纪律澄清：

1. 红线措辞已同步（2026-09-15）：`CLAUDE.md` 与 `imports_test.go` 注释原写"只允许 import tool/memory/policy"，漏了实际已在用的 `kb` 与 `k8s`；现统一为"只允许 import 叶子部件（tool/policy/memory/kb/k8s），不得依赖 agent/session/provider/config"。测试的 forbidden 列表本来就是后者，代码无需改。
2. 采集包不接口化。按项目既有取舍（tool.md §4.2"单一实现不造接口"），builtin 工具直接持有 `*k8s.Collector`，与现有 `*memory.Store` / `*kb.Store` 同款。将来真出现第二个 metrics 后端，再在 k8s 包内部抽子接口。

## 4. 采集包 internal/k8s 设计

### 4.1 组成

一个模块对象 `Collector`（`k8s.New(cfg)`，配置含 kubeconfig 路径、context、产物目录、超时与证据预算），对外方法按证据类别划分：

- `Pod(ns, name)`：Pod 现状摘要（症状字段 + 规格线索 + owner 链 + 拉取凭据，后者要合 Pod spec 与 ServiceAccount 两处看）
- `WorkloadOf(ns, pod)`：由 owner 链上溯取顶层工作负载规格（ReplicaSet 会再上溯一级到 Deployment；裸 Pod 报错）
- `Events(ns, name, uid, since, limit)`：事件摘要（按 `involvedObject.uid` 过滤，uid 为空回落 name；去重合并 + Warning 优先）。同一个方法也用来取子对象（PVC）自己的事件
- `Logs(ns, pod, LogQuery)`：容器日志（默认尾部 200 行 / 64KB，超出保尾部并标 Truncated）
- `PodMetrics(ns, pod)` / `NodeMetrics(node)`：metrics.k8s.io 读数，并在采集侧算好"用量占 limit/allocatable 的百分比"
- `Node(node)`：节点 conditions / allocatable / taints + 分配账本（`pods_on_node`、`requests_on_node`、`limits_on_node`、`free_on_node`、`allocation_error`）。原独立的 `PodsOnNode` 方法已并入它（两次调用凑一份视图，降级走字段而非调用方各自判断）
- `Nodes()`：全部节点的同款摘要（一次列全量 Pod 后在内存里按 `spec.nodeName` 分组算账本，避免逐节点 list）。为什么必须有：Pending 的 Pod 没有 `node_name`，`Collect` 里"按 Pod 取节点"那条路对它完全失效，不列节点就看不到候选节点的标签与余量
- `PVC(ns, name)`：存储申请摘要（phase / volumeName / storageClass / 申请容量 / accessModes）；按 Pod 卷引用逐个取，`Collect` 再补上引用它的卷名与 claim 自己的事件
- `Collect(ns, pod)`：组装证据包；`Save(ev)` / `SaveReport(t, json, md)` 落盘（命名与目录语义见 §7.3）
- `LogTargets(pv)`（包级导出）：决定采哪个容器的日志、是否取 previous（异常容器优先；仅在有上次运行记录时取 previous）。`Collect` 与 `k8s_logs` 工具共用它，保证"选哪个容器"的逻辑只有一处

一次 `Collect` 内对每个来源独立容错：某个来源取不到只记 `Evidence.Notes`（在证据包里显式写出），不中断整体采集——降级必须可见，否则模型会把"取不到"误当成"不存在"，或反过来把"没证据"当成"没问题"。唯一的例外是 Pod 自身：它是这份证据包的主目标，取不到就没有可诊断的对象，`Collect` 直接返回错误、不出证据包（发一份没有主目标的报告比报错更糟）。

每条 Note 带类别（`NoteKind`），这是"评测能分得清环境抖动与证据真缺"的前提：

| kind | 含义 | 处置 |
|---|---|---|
| `required_failed` | 必需来源失败（events / logs 读失败 / workload / node / node 分配汇总 / PVC） | 写进 missing_evidence 并压置信度 |
| `optional_unavailable` | 可选来源不可用（Pod 指标 / 节点指标） | 不影响结论 |
| `budget_trimmed` | 证据本来有，被 token 预算裁掉 | 不是"没有"，必要时缩小范围重采 |
| `no_data` | 来源正常但没有数据（容器未启动所以没有日志、无上次运行记录、裸 Pod 没有上层工作负载） | 不是失败 |

日志读失败按错误类型定类：400（BadRequest，容器尚未启动 / 无上次运行）记 `no_data`，其余（无权限、超时、连接失败）记 `required_failed`——否则 ImagePullBackOff 这类"压根没有日志"的场景会凭空多出一条"缺失证据"。工作负载同理分两面：裸 Pod（无 controller owner）记 `no_data`（本来就没有这一层），有 owner 却读不到才记 `required_failed`（2026-09-15 真集群首轮跑场景集时踩到并修正）。测试与冒烟断言只对 `required_failed` 严格（见 §11）。

### 4.2 采集清单与 API 映射

这是本模块的准确性依据：每个字段对应哪次 API 调用，禁止让模型去猜字段含义。表里列的是归因用得到的字段、不是全量清单，实际字段以 `internal/k8s/view.go` 为准（如 `qos_class`、`init_containers`、`env_from`、`available_replicas`、`kubelet_version`、`collected_at` 这些排不上归因主线，就没逐条列出）。

| 证据 | API 调用 | 关键字段 |
|---|---|---|
| Pod 现状 | `CoreV1().Pods(ns).Get` | `status.phase`；`containerStatuses[].state.waiting.reason`（CrashLoopBackOff / ImagePullBackOff / ErrImagePull）；`state.terminated.reason` 与 `lastState.terminated`（OOMKilled / Error / exitCode / signal / finishedAt）；`restartCount`；`conditions`（PodScheduled / Initialized / ContainersReady / Ready） |
| Pod 规格 | 同上，取 `spec` 摘要 | `containers[].resources`（requests/limits）、`image`、`command/args`、`env`（名与来源，值不取）、`probes`、`volumeMounts`、`nodeSelector/tolerations`、`affinity`（node/pod/pod-anti 三类，各分 required 硬门槛与 preferred 软偏好）、`restartPolicy`；拉取凭据 `imagePullSecrets`（Pod spec）与 `sa_pull_secrets`（ServiceAccount 上配置的，1.24 起不再复制进 Pod spec 但同样生效；读不到时记 `sa_read_error`） |
| owner 链 | `metadata.ownerReferences` | `kind/name/controller`，逐级上溯到顶层工作负载 |
| 工作负载规格 | `AppsV1().Deployments/StatefulSets/DaemonSets.Get`、`BatchV1().Jobs.Get` | `spec.template.spec` 的 resources / probes / image / command / env / affinity；`spec.replicas`、`strategy`；`status.conditions`、`unavailableReplicas`；Job 另取 `spec.completions/parallelism/backoffLimit` 与 `status.active/succeeded/failed`（这些不冒充 replicas，见 `JobView`） |
| 事件 | `CoreV1().Events(ns).List`（`fieldSelector: involvedObject.uid=<uid>`，uid 为空回落 `involvedObject.name=<pod>`） | `type/reason/message/count/firstTimestamp/lastTimestamp`；重点关注 BackOff / FailedScheduling / Unhealthy / FailedMount / FailedCreateContainer / Failed |
| 当前日志 | `Pods(ns).GetLogs(pod, {Container, Previous, TailLines})` | 文本流；容器选择三级优先——异常态容器（waiting 或上次异常终止）→ restartCount 最高者 → 第一个，逻辑只有 `LogTargets` 一处（`Collect` 与 `k8s_logs` 共用）；字节上限在客户端做：读满 1MiB 后按尾部裁到 64KB 并标 `truncated` |
| 上次日志 | 同上 + `Previous: true` | 文本流；CrashLoop / OOM 场景必取，缺失即压置信度 |
| Pod 指标 | `MetricsV1beta1().PodMetricses(ns).Get` | `containers[].usage`（cpu/memory），与 limits 相除得使用率 |
| Node 指标 | `MetricsV1beta1().NodeMetricses().Get` | `usage`（cpu/memory） |
| Node 信息 | `CoreV1().Nodes().Get` + `Pods("").List(spec.nodeName=<node>)` | `status.conditions`（Ready/MemoryPressure/DiskPressure/PIDPressure）、`allocatable`、`capacity`、`spec.taints`、labels（全量，理由见 §4.3）；分配账本 `pods_on_node`、`requests_on_node`、`limits_on_node`、`free_on_node`（= allocatable − 已用，pods 项按非终态 Pod 数扣）、`allocation_error`——Pending 归因的核心算术在采集侧算好，不让模型自己去加几十个 Pod 的 requests |
| ServiceAccount 拉取凭据（ImagePull 支） | `CoreV1().ServiceAccounts(ns).Get` | `imagePullSecrets`（与 Pod spec 上的合看；读不到时记 `sa_read_error`） |
| PVC（Pending 支） | `CoreV1().PersistentVolumeClaims(ns).Get`（按 Pod 卷引用逐个取） | `status.phase`（Pending/Bound/Lost）、`spec.volumeName`、`storageClassName`、`spec.resources.requests.storage`、`accessModes`；另按 `involvedObject.uid=<pvc uid>` 取一次 claim 自己的事件（子对象事件不在 Pod 事件里，`ProvisioningFailed` / `waiting for first consumer` 只在 claim 侧）；事件读失败时记 `events_error`——有值说明"没看到事件"不等于"没有事件"，与 `sa_read_error` 同理 |

### 4.3 裁剪规则

裁剪是生死线：Pod 完整对象带 managedFields 等噪声可达几十 KB，直接喂模型会挤爆 32k 预算。规则如下，全部落在这条路径上（唯一入口，模型拿不到未裁剪对象）：

- 丢弃：`metadata.managedFields`、`resourceVersion`、`selfLink`、`generation`、纯默认值字段（未显式设置的 securityContext 等）；`annotations` 只按 6 键白名单保留（`deployment.kubernetes.io/revision`、`kubernetes.io/change-cause`、`kubectl.kubernetes.io/restartedAt`、`prometheus.io/{scrape,port,path}`），其余一律不进视图——比"只丢大字段"更彻底，也免了逐键判断"算不算大"。
- 保留：`uid`（events 关联用）、`creationTimestamp`、`deletionTimestamp`、`labels`（全量，体量小且常用于归因）、`ownerReferences`。节点标签同样全量保留、不做白名单（2026-09-15 修正）：`nodeSelector` 与 `affinity` 可以引用任意自定义键，按"拓扑/机型"裁剪会让"要求的标签在不在"永远无法证实，而节点标签通常只有十几个。
- 事件：按 `type + reason + message` 去重合并（带 type 是因为 Normal 与 Warning 的同文案事件不该合并，否则 `count` 会把两类混成一个数），保留 `count` 与首末时间戳；默认窗口 1 小时、上限 50 条，Warning 优先。
- 日志：默认 `TailLines=200`（服务端截）；`LimitBytes=64KB` 由客户端实现（先读满 1MiB 再按尾部裁），标 `truncated: true`——故障现场在尾部，客户端裁能保证尾巴一定在，且服务端 LimitBytes 是"从头部截"的语义，不适合这个用法。
- 数值：cpu/memory 统一转成可读单位并给出使用率（usage/limit），避免模型自己做单位换算。

### 4.4 证据包（Context Builder）

`Collect(ns, pod)` 采集上述数据，按固定优先级组装成一份带 token 预算的证据包：pod 现状 → 工作负载规格 → events → PVC（含子对象事件）→ logs（previous 优先）→ metrics → node → node metrics。默认预算 8k token；超预算时按反优先级裁剪（先砍 node metrics、再砍 node、再砍 pod metrics、再缩日志保尾部、最后裁事件保 Warning 与最新）；Pod 现状与 PVC 不裁——它们分别是症状与"Pending 卡在哪一步"的唯一来源。证据包同时落盘，供回放与评测。

当前实现为串行采集（代码事实，先正确后快）：每个来源一次 API 调用，任一来源失败只记 Note 不中断。将来改并行前必须先固定 Notes 的追加顺序（或改为按来源分段追加），否则同一份证据两次采集的 Notes 顺序不同，评测基线会漂移；并行化只是耗时优化，不影响证据内容。

超时预算按调用类型分两档（2026-09-15 从审计里分出来的）：元数据调用（取对象、列列表）每次 30s，日志读单独 90s。理由是两者不是一个量级——日志是流式大对象（最多 1MiB 正文，还可能在服务端攒正文），共用 30s 会把"日志读得慢"判成读失败并记 `required_failed`，而那是把环境慢当成证据缺（`required_failed` 还会进缺失声明的客观基准、压置信度）。90s 这个数是拍的：够覆盖慢链路上 1MiB 的量级，又不至于把卡死拖太久。真正的空闲超时（读到一半不动了就掐、持续有流量就一直等）留作后续，见 §15。

## 5. 工具层设计

全部为只读工具，权限一律 `policy.Pass`（登记进 [tool_permissions.go](file:///home/cxr/Program/08_07_GO/small/internal/tool/builtin/tool_permissions.go)），命名统一 `k8s_` 前缀。

| 工具 | 参数（缺省） | 返回 |
|---|---|---|
| `k8s_pod` | namespace、pod（必填） | Pod 现状摘要 + owner 链 |
| `k8s_workload` | namespace、pod（必填；只支持由 Pod 上溯） | 顶层工作负载规格摘要 |
| `k8s_events` | namespace、pod、since_seconds（3600）、limit（50） | 事件摘要（去重、Warning 优先；按 uid 过滤） |
| `k8s_logs` | namespace、pod、container（自动选）、previous（false）、tail_lines（200）、limit_bytes（65536） | 日志文本 + 截断标记 |
| `k8s_metrics` | namespace、pod、container（可选） | CPU/内存用量 + 使用率（vs limits） |
| `k8s_node` | node（必填，不做 pod 推导） | 节点 conditions / allocatable / taints + 分配账本（`free_on_node` 等） |
| `k8s_nodes` | 无 | 全部节点的同款摘要数组；Pending 归因的入口（Pending 的 Pod 没有 `node_name`，`k8s_node` 无从下手） |
| `k8s_evidence` | namespace、pod | 证据包（采集 + 预算裁剪 + 落盘；`notes` 每条带 kind，见 §4.1） |
| `k8s_report` | 结构化报告字段（见 §7.3） | 落盘路径 + 渲染后的 md |

一处特别设计：`k8s_report` 是唯一的写工具，但只写自有受控目录 `~/.small/k8s/`，与 `doc_parse` 写 cache 同理，故仍为 Pass，不触发确认交互。它同时解决"结构化报告怎么拿到"的问题——用工具参数的 JSON Schema 承载报告结构，而非解析模型自由文本；md 报告由 Go 侧从 JSON 渲染，格式稳定。

`k8s_report` 是最后注册的工具（第九个），因此 `Deps` 需增加 `K8s *k8s.Collector`——注入建好的采集器（与 `*memory.Store`/`*kb.Store` 同款），而非注入配置再让工具自己建客户端：客户端构造属系统边界，错误应在组合根 fail fast；nil 即退化不注册。

## 6. 诊断工作流（workflow 分支）

资产落点 `internal/workflow/workflows/k8s-diag.md`，对齐 [workflow.md](file:///home/cxr/Program/08_07_GO/small/Zoo/model/workflow.md) 的 embed + frontmatter 模式，被 `RenderBranch` 自动收录，无需改 workflow 包。

frontmatter 元数据：name `k8s-diag`；trigger 为"用户报告 Pod 异常（重启/起不来/OOM/一直 Pending/探针失败）或要求诊断某个 Pod"；input 为 namespace/pod；output 为 RCA 报告 + 落盘路径；stop 为报告产出并汇报后等确认，工具失败/用户中断/轮次超限即停。

正文步骤（收集 → 分析 → 验证）：

1. 定位目标：缺 namespace/pod 先向用户确认；用 `k8s_pod` 确认对象存在与症状。
2. 统一收集：调 `k8s_evidence` 取证据包（已含 pod/events/workload/logs/metrics/node）。
3. 症状归类：在六类中确定主症状（可多），判别顺序是事件 reason → lastState/exitCode → 日志与配置收口。
4. 列候选根因：按症状给出候选类别与判别线索（词表 23 项逐条"长什么样"的表就在分支资产里，是除 `report.go` 之外的唯一维护点；`TestCategoriesCoveredByPlaybook` 固化"词表里每个类别都要在分支里有线索"这条一致性——加了类别却漏写进分支，模型就永远选不到它）。
5. 按需深挖：证据包缺"必查项"时，用原子工具补（previous 日志、其它容器、节点余量、PVC）。
6. 交叉验证：对每个候选根因找出支持证据与反对证据，明确写出被否候选与否决理由。
7. 置信度裁决：按 §7.2 规则定档。
8. 提交与汇报：调 `k8s_report` 落盘，对话里按 §7.1 模板输出摘要，等待确认或追问（结束）。

触发双通道（对齐 /pdf 先例）：/diag 命令显式进入（走 `errInject` 注入消息），以及模型按 trigger 自动进入，首句带 `[进入分支:k8s-diag]`、完成时带 `[分支完成:k8s-diag]`。

## 7. 报告设计

### 7.1 对话内模板

```
Root Cause: <一句话结论>（置信度 xx%）
Symptoms: ...
Evidence:
1. <证据>（来源：工具名 / 对象.字段 / 时间）
2. ...
Ruled Out: <被否定因与理由>
Missing Evidence: <缺什么、为什么影响判断>
Suggestion: <建议动作 + 理由>
```

### 7.2 置信度与证据完备度

模型自评容易虚高，故用规则约束：

- ≥0.80：至少两条独立证据，且含一条决定性证据（termination reason / event reason / 配置数值），且关键证据无缺失。
- 0.50–0.79：证据同向但缺决定性证据。
- <0.50：仅现象吻合。
- 关键证据清单中任一项缺失时，置信度上限 0.79，且必须写入 `missing_evidence`。
- 每条证据必须带来源；证据里不得出现"可能/也许/大概"这类无支撑表述。

五条里只有第一条的"至少两条独立证据"落成了代码硬校验（`k8s_report` 的 `checkConfidenceRules`，2026-09-15）：confidence ≥0.80 时证据需 ≥2 条且来源去重 ≥2，否则工具返回业务失败，把"差在哪"回灌给模型自己补证据或降档——不静默改数字，报告里的置信度要么是模型自己的判断，要么这份报告不成立。其余几条判不了语义（"决定性证据"要读懂证据内容，"关键证据缺失"要先知道哪个算关键），仍由提示词与评测的置信度档位项约束。一处实测依据：14 份满分报告里 13 份都申报了缺失证据且置信度 ≥0.80——那些缺口模型自己标注为"不影响结论"，按"有缺失就压到 0.79"硬套会把这批如实申报全部打回，等于把"降级可见"当成错误。

按症状的关键证据清单（"该查什么"；"该判成哪一类"的候选与判别线索见 §6 第 4 步，那张表覆盖词表全 23 项）：

- CrashLoopBackOff：previous 日志、`lastState.terminated`（reason/exitCode）、容器 command/args。
- OOMKilled：`termination.reason == OOMKilled`、limits、内存使用率（metrics）。
- ImagePullBackOff：事件的 image 名与错误码、image 全名（registry/tag）、imagePullSecrets 归属。
- Pending：FailedScheduling 事件文本、节点 allocatable vs Pod requests、taints/affinity/nodeSelector、PVC 状态。
- ContainerRestart（非 OOM）：restartCount 与 `lastState.terminated.exitCode`、previous 日志尾部。
- Probe Failed：probe 配置（path/port/initialDelay/period/failureThreshold）、Unhealthy 事件、容器启动耗时。

### 7.3 落盘产物

目录按"每次运行一个目录"组织：`~/.small/k8s/<会话 id>/`（组合根建 Collector 时把该目录作为 `Config.Dir` 注入；一个 CLI 会话一个目录，跨会话自然分开）。文件名按目标命名，三件同目录同前缀，便于按对象对齐回放与评测：

- `<ns>-<pod>.evidence.json`：证据包
- `<ns>-<pod>.report.json`：结构化报告
- `<ns>-<pod>.report.md`：渲染后的人读报告

同一会话内重复诊断同一 Pod 以最新一次覆盖（避免目录爆炸）；文件名片段经清洗（目标名来自用户输入，防路径注入）。采集器不持有"当前运行目录"之类的跨调用状态——`Dir` 装配期注入，`Save`/`SaveReport` 纯函数式落盘。

`report.json` 字段：`schema_version`、`generated_at`、`target`（context/namespace/pod/workload）、`symptoms[]`、`root_cause{summary, category, confidence}`、`evidence[]{source, ref, excerpt, supports}`、`ruled_out[]{summary, reason}`、`missing_evidence[]{item, impact}`、`suggestions[]{action, rationale, risk}`、`alternatives[]{summary, confidence}`。

其中 `root_cause.category` 取值受词表约束（23 个，按六类症状分组，含兜底 `other`）：唯一维护点是 `internal/k8s/report.go` 的 `rootCauseCategories`，`k8s_report` 的 schema enum 与工具层参数校验都由它生成——报告里不允许出现自造类别（模型拿不准时选 `other` 并在 summary 里说明）。评测侧不共享词表：`cmd/k8seval` 只做字符串相等比对，且刻意不 import internal（否则实现漂移会变成判分漂移）。清单、释义与取舍见 jjj §22。

## 8. 权限、安全与 RBAC

本期全只读，RBAC 最小集如下（2026-09-15 对着代码里每一个 client 调用逐个核过，精确到 verb；核出并修掉了一处漏项——`nodes` 还需要 `list`，`k8s_nodes` 与 Pending 归因都走它）。同日在 minikube 上用只读 SA（就是下面这份 YAML）实测：代码用到的端点全部可用（含集群范围 `pods` list 与 metrics 的 pod/node 两种 raw 读），`secrets` get、`pods` delete 均被拒——集合够用且没有多给：

- core：`pods` get + list（list 必须是集群范围，见下）、`pods/log` get（容器日志含 previous 走的是子资源）、`events` list（Pod 事件与 PVC 自己的事件）、`nodes` get + list（get 给 `k8s_node`，list 给 `k8s_nodes`）、`serviceaccounts` get（拉取凭据的第二个来源）、`persistentvolumeclaims` get。
- apps：`replicasets`、`deployments`、`statefulsets`、`daemonsets` get。batch：`jobs` get。
- metrics.k8s.io：`pods` get、`nodes` get（通常由 metrics server 的聚合 API 权限覆盖）。

除这三处外都不要给 list：我们只在跨命名空间列 Pod（节点账本）、列节点、列事件时用到 list，其余一律 get。

一处容易踩空的地方：节点分配账本要按 `spec.nodeName` 跨命名空间列 Pod（`Pods("").List`），所以 `pods` 的 list 必须是集群范围——只给命名空间级的 Role，`free_on_node` 会整块降级（记 `allocation_error` 与 Note）。本地 minikube 用默认 context 即可，无需额外授权；生产接入按下面这份直接授权即可（把 SA 换成你自己的身份）：

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: small-k8s-diag-readonly
rules:
  - apiGroups: [""]
    resources: ["pods"]
    verbs: ["get", "list"]           # list 要集群范围：节点账本按 spec.nodeName 跨命名空间列 Pod
  - apiGroups: [""]
    resources: ["pods/log"]
    verbs: ["get"]
  - apiGroups: [""]
    resources: ["events"]
    verbs: ["list"]
  - apiGroups: [""]
    resources: ["nodes"]
    verbs: ["get", "list"]           # list 给 k8s_nodes（Pending 归因的入口）
  - apiGroups: [""]
    resources: ["serviceaccounts", "persistentvolumeclaims"]
    verbs: ["get"]
  - apiGroups: ["apps"]
    resources: ["deployments", "replicasets", "statefulsets", "daemonsets"]
    verbs: ["get"]
  - apiGroups: ["batch"]
    resources: ["jobs"]
    verbs: ["get"]
  - apiGroups: ["metrics.k8s.io"]
    resources: ["pods", "nodes"]
    verbs: ["get"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: small-k8s-diag-readonly
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: small-k8s-diag-readonly
subjects:
  - kind: ServiceAccount
    name: small-k8s-diag           # 换成你的 SA 或用户
    namespace: default
```

想收得更紧可以拆成两段：ClusterRole 只留集群级必需的三项（`pods` list、`nodes` get + list、metrics 的 `nodes` get），其余（`pods` get、`pods/log` get、`events` list、`serviceaccounts` get、`persistentvolumeclaims` get、apps、batch、metrics 的 `pods` get）都用命名空间级 Role 授权，只给允许诊断的命名空间。代价是每加一个待诊断命名空间就要多一个 RoleBinding。

其余红线沿用：kubeconfig 与 token 绝不入库（用户本地文件引用，不进 config.yml 内容）；日志可能含业务数据，落盘目录在用户 home 下，不外传（除送 LLM 外）。

只读这条不只是承诺，还有守卫：`internal/k8s/k8s_test.go` 的 `TestReadOnlyGuard` 给两个 fake clientset 各装一个记录型 reactor，跑一遍完整 `Collect` + `Nodes()`，断言实际发出的每个 action 的 verb 只能是 `get`/`list`（并反向自检"记录器真的收到了对 pods/events/nodes/PVC/SA/RS/Deployment 与 pods/log 的读"，防假绿）。它锁的是行为而非源码文本：将来谁在采集路径里加一句 Update/Patch，测试立刻红。上面那张 RBAC 最小集仍要靠使用者配置才生效——代码只保证"不写"，不保证"没权限写"。

## 9. 配置与组合根装配

`config.yml` 实际落地三项（缺省走内置默认值，对齐现有配置来源单一的做法）：

```yaml
kube_config: ~/.kube/config     # 集群接入（不做 KUBECONFIG 环境变量隐式读取）
kube_context: ""                # 目标 kube context（多集群切换）；空 = 用 kubeconfig 的 current-context
k8s_dir: ~/.small/k8s           # 诊断产物根（证据包 + 报告）
```

证据包预算没进 config.yml：它是采集包内置默认值 `DefaultEvidenceMaxToken = 8192`（`k8s.Config.EvidenceMaxTokens` 可覆盖，但组合根暂不暴露配置键）——与 §15 里"8k 是否合适"待实测一并决定要不要提到配置层。

组合根改动：`config.Load` 增字段 → `k8s.New(cfg)` 建采集包 → `builtin.Deps` 增 `K8s` 字段 → main.go 补 base 提示词的 k8s 工具说明句（workflow 分支清单由 `RenderBranch` 自动带出，不用手拼）；采集器构造失败只告警退化（`k8s_*` 工具不注册），不阻塞普通对话。`/diag <ns>/<pod>` 命令加在 `main_commands.go`，先做一次目标可达性预检再走 `errInject` 注入诊断请求。构造之后还要做一次启动期能力探测（`Preflight`：判集群可达 + 探可选能力，并按能力裁剪工具与提示词/分支清单），见 §16。

多集群：`kube_config`（换 kubeconfig 文件）+ `kube_context`（选 context）已落地，两者都是启动期装配，走采集包的 `Config.Context` → clientcmd overrides，并记进 evidence/report 的 `target.context`（回放时能分辨这份证据来自哪个集群）。仍未做的是"按命令临时切集群"（`/diag --context`）——那要求按 context 现建 `Collector`，属装配层改动，见 §15。

## 10. 模拟环境与故障场景（Zoo/k8s-lab）

环境：minikube（`minikube start` + `minikube addons enable metrics-server`，addon 名带连字符）。选它是因为 metrics server 一键可用，省掉自建 Prometheus 的堆栈成本。

已落地的是冒烟台（`Zoo/k8s-lab/{smoke.sh,smoke.yaml}`，见 §11.1）；下面是第 3 批扩成的场景集结构：

```
Zoo/k8s-lab/
├── smoke.sh              # 冒烟台：up / test / down（已落地）
├── smoke.yaml            # 冒烟场景（已落地）
├── scenario.sh           # 场景生命周期：list / up / down / target / down-all（已落地）
├── eval.sh               # 端到端评测驱动：起场景 → 跑 /diag → 打分 → 收场景（已落地）
└── scenarios/<symptom>-<variant>/{manifest.yaml, expect.json, README.md}   # 14 个已落地（首批 8 + 第二批量 3 + 第三批量 3）
```

场景目录里可以有可选的 `setup.sh` / `teardown.sh`：`scenario.sh` 在 apply 前跑 setup、清理后跑 teardown，
`down-all` 对每个带 teardown 的场景都跑一遍。为什么需要它：有些前置是节点级对象（如给节点打 taint），
写不进 `manifest.yaml`，而没有它就只能靠人肉 kubectl（下一轮必然忘）。

变体矩阵（六类 × 变体）：CrashLoopBackOff 有启动即退出、依赖缺失、配置错误；OOMKilled 有 limit 过小、内存泄漏、堆未配；ImagePullBackOff 有镜像名错、tag 不存在、缺 secret；Pending 有资源不足、nodeSelector 不匹配、taint 未容忍、PVC 未绑定；ContainerRestart 有探针杀、应用崩溃、周期 panic；Probe Failed 有路径错、端口错、initialDelay 太短。

首批落 8 个场景（六类全覆盖 + Pending 三分支，逐条清单与选取理由见 jjj §21.2）。第二批量 3 个：`crashloop-missing-dependency`（DNS 解析失败）、`crashloop-config-missing-env`（必填 env 缺失）、`restart-periodic-crash`（跑 25 秒后自崩、无探针）。第三批量 3 个（原推迟的三个，落地方式见 jjj §34）：`oom-heap-misconfig`（真 JVM，`-Xmx256m` 对 128Mi limit）、`imagepull-missing-secret`（ghcr 私有路径匿名拉取 403）、`pending-taint-not-tolerated`（setup 钩子给节点打 NoSchedule taint）。矩阵里仍未收的只有 OOM 内存泄漏（要真泄漏，得让用量随重启单调上升）与 ProbeFailed 的 initialDelay 分支，原因见 jjj §34 末尾。

`expect.json` 字段（已冻结，逐字含义见 jjj §21.3）：`symptom`；`target{namespace, selector}`；`wait{container_state_reason, container_last_state_reason, phase, container_restart_count_min, pod_ready, event_reason, timeout_seconds}`（写了哪几个就等哪几个；`event_reason` 是 Pending 类场景的判别依据——Pod 一创建 phase 就是 Pending，只看它会在事件写出来之前就宣布"就绪"，见 jjj §34）；`evidence_test`；`report{symptoms_must_include, root_cause_category, confidence.min, evidence_keywords, evidence_min_hits, must_not_claim}`。

## 11. 评测与测试计划

分两层，理由是"LLM 输出不确定，不能进合并门槛；采集层必须确定性可回归"。

一、采集层（无 LLM，确定性）：Go 集成测试带 `k8slab` build tag（默认不跑，需集群），对每个场景断言证据字段——如 OOMKilled 场景必须出现 `termination.reason=OOMKilled`、limits、usage/limit 比值。测试文件可自由 import（项目先例），不破依赖纪律。

二、端到端（需 DEEPSEEK_API_KEY）：跑 /diag 后读 `report.json`，与 `expect.json` 比对——症状识别（0.15）、根因类别（0.35）、证据覆盖（0.25）、缺失声明（0.15）、置信度档位（0.10），命中 `must_not_claim` 则本场景 0 分（一票否决）。缺失声明只看漏报：基准是证据包 notes 里 kind=`required_failed` 的项（见 §4.1），必需源失败而报告没申报才按比例扣分；`optional_unavailable` 与"工具集之外的缺口"（容器内监听端口、ResourceQuota 明细等）申报与否都不扣分，超出基准时只在 score.json 的 notes 里提示人工复核（2026-09-15 两轮端到端实测后定，理由见 jjj §33——扣多报等于训练"少说话"，与降级可见原则相悖）。打分器是 `cmd/k8seval`（读产物 JSON，不依赖 internal），输出 `Zoo/k8s-lab/out/<run-id>/` 下的 score.json 与汇总表。本批只出分不设阈值，等基线稳定再定回归门槛。口径细则见 jjj §21.5。

常规合并门槛不变：`go test -race ./...`、`go vet`、`gofmt` 全绿（k8slab 测试不在其中）。启动期探测（preflight）与能力裁剪的测试、手工冒烟另见 §16.7。

### 11.1 冒烟台（2026-09-10 已落地）

采集层之外先立真集群冒烟台：`Zoo/k8s-lab/{smoke.sh,smoke.yaml}` +
`internal/k8s/lab_test.go`（build tag `k8slab`），用法见 CLAUDE.md 常用命令。五条设计取舍：

1. 健康目标不单独造：直接复用集群自带 kube-system 的 Running Pod——少一份 manifest，
   且顺带验证"证据齐全时不该出现必需来源失败"这条隐性回归（降级说明按 kind 分级后，
   可选源抖动不再误判成采集缺陷，见 §4.1）。
2. 等状态而不是 sleep：拉镜像失败是异步的（先 ErrImagePull，退避若干次后才变
   ImagePullBackOff），固定 sleep 在慢机器上必然误判，故按 waiting reason 轮询等待。
3. 故障目标选"拉不到镜像"而不是 OOM/CrashLoop：前者不依赖镜像能否拉取（它本来就是
   拉不到），任何网络条件下都能复现；OOM/CrashLoop 需要本地有可用镜像，放到后续场景集里做。
4. 用 build tag 而非 t.Skip 判断集群存在：tag 保证默认 `go test` 完全不编译它（合并门槛干净），
   tag 内再对"连不上集群/场景没建"做 Skip——两种缺失都不该让人误以为是代码错。
5. 冒烟断言不依赖事件：kube-apiserver 的事件 TTL 默认 1 小时（`--event-ttl`），健康 Pod
   跑久了事件会被回收，所以"健康目标事件非空"这类断言是时间相关的（集群刚起能过、
   几小时后必挂）。事件断言只放在故障目标上——那边的事件是刚生成的。
   （2026-09-11 在 TestLabHealthyPod 上踩到，修法见 jjj 草稿 A-4。）

## 12. 分期落地

第 1 批（已完成 2026-09-10）：`internal/k8s` 采集包——Config/Collector/New、裁剪视图（视图即白名单）、八类采集方法、证据包组装与预算裁剪、落盘，加 9 个 fake clientset 单测。验收：gofmt/vet/`test -race` 全绿，minikube 三类目标（正常 Pod、裸 Pod 拉镜像失败、Deployment 下坏镜像）冒烟通过。

第 1.5 批（已完成 2026-09-10）：冒烟台——`Zoo/k8s-lab/{smoke.sh,smoke.yaml}` + `internal/k8s/lab_test.go`（build tag `k8slab`）。

第 2 批（已完成 2026-09-15）：八个 builtin 工具（六个原子 + `k8s_evidence`/`k8s_report`，权限全 Pass）、config 两项（`kube_config`/`k8s_dir`）、组合根装配（会话产物目录 → `k8s.New` → `Deps` → 提示词条件拼句）、`/diag` 命令（走 `errInject` 注入）+ `workflows/k8s-diag.md` 分支资产（收集→分析→验证→提交，含按症状必查项与置信度裁决规则）。验收：门禁全绿；启动冒烟 `/tools` 列出八个 `k8s_*`、`/help` 列出 `/diag`；真集群 `-tags k8slab` 断言（健康 Pod / 故障 Pod / 证据包与报告落盘）全过。

第 2.5 批（已完成六项：imagePullSecrets、节点余量、PVC 含子对象事件、env 名与来源、affinity、Job 语义与三类工作负载 conditions）：补齐证据面——env 名与来源、affinity、imagePullSecrets、PVC（含其事件）、节点余量（allocatable − Σrequests）、StatefulSet/DaemonSet/Job 的 conditions、Job 语义（completions/parallelism 不冒充 replicas）。理由：这些字段是 §4.2 与 §7.2 已承诺的证据面，不补齐会让 Pending / ImagePullBackOff 的评测基准本身失真（把"采集不到"记成"模型诊断错"）。已完成的前置项：事件按 uid 过滤、事件计数累加（原列在本批，已在 2026-09-15 随工具层修掉）。缺口清单与优先级见 jjj 草稿的 backlog 一节；B11（CLAUDE.md 红线措辞）与 B15（Evidence.Notes 分级）已于 2026-09-15 完成，本批收尾。

第 3 批（收口，契约 2026-09-15 冻结）：契约见 jjj §21（场景集形态、expect.json、打分口径）与 §22（根因类别词表）。已完成：词表落点（report.go 词表 + schema enum + 参数校验 + 提示词，见 jjj §23）；`scenario.sh` 与首批 8 个场景（六类全覆盖，见 jjj §24/§25）；采集层断言 `lab_scenarios_test.go`（见 jjj §26）；Pending 的节点侧证据面（`Nodes()` + `k8s_nodes`，见 jjj §27）；打分器 `cmd/k8seval`（见 jjj §28）；端到端驱动 `eval.sh`（见 jjj §29）；真集群首轮验证（见 jjj §30）；症状→根因判别线索表（见 jjj §31）；第二批量 3 个场景（见 jjj §32）；端到端第二轮与打分口径定稿（见 jjj §33）；第三批量 3 个场景 + `scenario.sh` 的 setup/teardown 钩子与 `event_reason` 就绪条件（见 jjj §34）。本批收口。

后续（扩展）：Prometheus 后端、eBPF 探针评估、GUI 诊断视图、多 context 增强。

## 13. 预留扩展位

- metrics 后端：本期只接 metrics server；Prometheus 作为第二实现出现时，才在 k8s 包内部抽 metricsSource 接口（遵"单一实现不造接口"）。
- eBPF：将来以 go:embed 嵌编译好的 .o、由 cilium/ebpf 运行时加载，作为系统调用/内存/网络事件的补充采集源。本期只留扩展位注释与文档说明，不引依赖（cilium/ebpf 与项目零依赖风格冲突，且需内核条件与特权，前期不合适）。
- Loki：日志后端的可选替代，默认仍走 k8s API 直读。
- 多集群：`kube_config` + `kube_context` 已落地（启动期选集群，见 §9）；"按命令临时切集群"需按 context 现建采集器，未做。

## 14. 决策记录

1. 落点长在 small 内（2026-09-10）：复用 agent/工具/权限/会话/轨迹，agent 循环零改动。
2. 形态为混合：workflow 固定收集清单保证据完备 + 模型按需深挖保推理力。
3. 读取用 client-go typed clientset + k8s.io/metrics。
4. 严格只读，九个工具全部 Pass，不做修复动作。
5. 模拟环境用 minikube，metrics server 起步。
6. 工具粒度为原子六件套 + 列节点 + 证据包 + 报告提交，共九个。
7. 报告双产物：对话内 md + JSON 落盘（含证据包）。
8. 接入为 config.yml 的 `kube_config` + `kube_context`（启动期选集群；`/diag --context` 未做，见 §9）。
9. 入口双通道：/diag 命令 + 自然语言触发。
10. 评测为脚本打分，分采集层集成测试与端到端两层。
11. 接缝按 kb/memory 先例，builtin 直接持有 `*k8s.Collector`，不造接口。
12. 报告用 `k8s_report` 工具提交结构化 JSON（避免解析自由文本），md 由 Go 渲染。
13. client-go 与 `k8s.io/metrics` 取 v0.37.0（与集群小版本对齐），落地后集群为 v1.37.0，同版本号无需降级。
14. 启动期加一次轻量 preflight（2026-09-18）：`ServerVersion` 失败 = fail-closed（不注册全部工具），可选能力明确缺失 = 只裁对应工具，探测本身无法判定 = fail-open（保留工具，输出注明"未探测到"）。超时缺省 2s（`k8s.Config.PreflightTimeout` 可覆盖，不进 config.yml）。理由见 §16.2。
15. 探测落点在组合根显式调用 `coll.Preflight(ctx)`，`k8s.New` 保持纯装配（无网络 IO）。理由：`New` 的语义边界不变，探测可单独测（§16.4）。
16. 能力裁剪只作用于 `k8s_metrics` 这一个工具；`k8s_evidence` 里的指标字段仍走 `optional_unavailable` 降级（不改 Collect）。理由：指标只在这两处出现，后者不是独立工具，裁注册解决不了它（§16.5）。
17. 工作流分支清单按能力裁剪：frontmatter 增 `requires`，`k8s-diag` 标 `requires: k8s`，新增 `RenderBranchFor`。理由：工具不注册时分支仍被注入，模型会进一个没有工具的分支（§16.1）。
18. 顺带修正（2026-09-18）：`New` 把"实际生效的 context"归一化进 `cfg.Context`（显式 `kube_context` 优先，否则 kubeconfig 的 `current-context`），使 evidence/report 的 `target.context` 在默认场景不再为空（§16.3）。
19. 运行期连接类失败归因（2026-09-19）：`ExplainError` 把传输层错误翻成结论短语（`集群不可达（connection refused）`），4xx 业务错误原样回灌；作用面是工具回灌、证据包 notes（含 `pvc.events_error`）、视图错误字段（`allocation_error`/`sa_read_error`）与 `/diag` 拒绝文案（首版只堵了工具层与 3 处 notes，收口见 §17.6）。用户向建议只在连接类失败时由调用方补（`IsConnFailure`），模型向建议由 base 提示词交代一次。理由与收口过程见 §17。
20. 失败语义靠 `Data` 文案自述（2026-09-19）：tool 消息在协议里只有 content 能承载信息（`provider.Message` 无 error 字段），所以"失败"必须写进回灌文本；`Result.IsError` 只服务展示层与轨迹，不进模型上下文——`cli.md` 原先"`Result{Data,IsError}` 回灌模型"的说法已按此改正。是否给所有工具加统一的失败文本标记，见 §15。
21. 目标端点落盘（2026-09-19）：`Target` 增 `api_server`（值取 `New` 里的 `rest.Host`，与 context 同处归一化），证据包与报告都带、`report.md` 头部与 CLI 启动行展示，报告 JSON 契约变更故 `ReportSchemaVersion` 1 → 2；落产物与错误文本都做凭据脱敏（去 URL userinfo / `://***@`，见 §17.6 第 4 条）。理由：原以为"地址由 context 交代"，但 context 只是名字，结果地址在所有产物里消失、与"降级必须可见"相拧（见 §17.6）。

## 15. 待决问题

- 报告是否增加"验证方式"字段（怎么确认修好了）与建议动作的风险等级。
- 事件 API 是否改用 `events.k8s.io/v1`（先用 core/v1，它是当前主流且信息足够）。
- 证据包 8k token 预算是否合适（实测后调；是否提到 config.yml 也一并定，见 §9）。
- 是否支持离线诊断（直接吃已导出的 pod yaml 文件），本期先只支持在线。
- 是否做"按命令临时切集群"（`/diag --context`）：需要按 context 现建 `Collector`（当前是启动期建一个），值与成本都不大，但没有真实需求前不动。

2026-09-15 审计（只读性 / 依赖与工具契约 / 文档对账三项）后新增的待决项：

- 置信度规则：第一条的"至少两条独立证据"已于 2026-09-15 落成工具层硬校验（≥0.80 时证据需 ≥2 条且来源去重 ≥2，不满足打回让模型自己改，不静默改值）；"决定性证据"与"关键证据缺失去顶"判不了语义，暂留提示词与评测扣分。真要更硬，得先定义"可机器判定的决定性证据"长什么样（阈值、字段、还是正则），且要接受误杀代价。
- 日志流的超时：已于 2026-09-15 拆出独立预算（元数据 30s / 日志 90s，见 §4.4），解决"慢读被判成缺失证据"。仍未做的是真正的空闲超时——读出第一字节后若持续有数据就一直等、卡住才掐（对齐 provider 那条经验）；要做需要在流上包一层按读重置的定时器，代价比现在这个常量大一截，等真遇到"日志流卡住"再说。
- `List` 一律不带 `Limit`/分页，且节点账本要跨命名空间 `Pods("").List`：大集群（几千 Pod）下内存与耗时都不封顶。要不要加 Limit 或改分页。
- `Collect` 没有整体 deadline（每个来源各自 ≤30s，日志那条 ≤90s，PVC 随卷引用线性增加调用数）：要不要给整包一个总预算。

2026-09-18 新增待决（preflight 相关，见 §16）：

- 是否用 `SelfSubjectAccessReview` 把 RBAC 403 提前到启动期（本次未做，理由见 §16.6）。
- 是否做"重新探测/热恢复"：集群后起时不必重启 small。要做得让注册表可变、提示词重算，见 §16.6。
- preflight 是否要同时探其他可选能力（本期只探 `metrics.k8s.io`，扩展位就是 §16.3 的探测序列）。
- 连接类失败已归因（§17）；401/403 这类鉴权错误仍按原文回灌，是否也归成"权限不足"结论句待定。
- 是否给所有工具加统一的失败文本标记（如 `Data` 前置一个标签）：现在失败语义靠文案自述，模型得读文案判断（见 §14 决策 20）；加标记会改动所有工具的输出与提示词，属框架级改动，等出现真实误判案例再说。
- GUI 是否展示当前接入端点与能力（本次只在 CLI 启动行与 `report.md` 里展示 `api_server`，GUI 仍只有对话流）。

## 16. 启动期 preflight 与能力裁剪（2026-09-18）

### 16.1 问题

`k8s.New` 只读 kubeconfig 与拼 `rest.Config`，不拨号、不鉴权；它的失败面只有"文件层"（文件不存在 / 格式错 / context 名不存在）。凡"文件对但集群不可用"的情形——apiserver 没起、网络不通、token 失效、RBAC 不足、metrics-server 缺失——一律穿透到第一次工具调用才暴露。

后果是三处不一致：注册表里有 9 个 `k8s_*` 工具、提示词里列着它们、真实可用性却可能是零。两个区间：

- 组合根只把 `New` 失败当作"未接入"（main.go 的 nil 退化路径）。`minikube stop` 之后 `New` 依然成功，于是工具照注册、提示词照列，模型一调才拿到业务失败。
- 工具清单提示词已经是条件拼接（`k8sColl != nil` 才追加），但工作流分支清单是无条件注入的：工具不注册时 `k8s-diag` 分支仍会出现在提示词里，模型可能进入一个没有任何工具的 k8s 分支。

对模型侧这不是灾难——业务失败 + notes 降级口径是既有设计，降级可见；对用户侧则是"事前无法判断能不能用"。本次补的就是这一段：把"能不能用"从运行期的意外变成启动期的已知项。

### 16.2 目标与取舍

三件事：启动期一次轻量探测；按能力裁剪工具注册与两处提示词；把探测结果明确告知用户并区分原因。

分档原则（核心取舍）：

- 集群不可达 / 鉴权失败（`ServerVersion` 失败）→ fail-closed：不注册全部 `k8s_*` 工具，退回既有 nil 退化路径。
- 可选能力明确缺失（如 `metrics.k8s.io` 组不存在）→ 只裁对应工具，其余照常。
- 探测本身无法判定（超时、非 404 的报错、权限不足）→ fail-open：保留工具，输出里注明"未探测到"，把失败留给调用期降级。宁可"注册了偶尔失败"，也不要"因为探测不准而裁掉本来可用的能力"。

时效性边界（必须写清）：preflight 只回答"启动这一刻能不能用"。启动后集群挂掉仍会"注册着但调用失败"——调用期降级仍是兜底；本次不引入后台巡检，也不做热恢复（见 §16.6）。

### 16.3 探测契约（internal/k8s）

超时：`defaultPreflightTimeout = 2 * time.Second`；`Config.PreflightTimeout` 可覆盖（<=0 用缺省），但组合根不暴露 config.yml 键——探测是启动期一次性的，不值得多一个配置面；留这个字段是为了让单测能把超时压到几十毫秒。一次 `Preflight` 的所有请求共享同一个 2s 预算（不是每个请求各 2s）。

结果类型：

```go
// Availability 能力可用性三态：未知不等于不可用（未知按可用处理，见 §16.2 的分档）。
type Availability int

const (
	CapUnknown     Availability = iota // 未探测到（探测本身报错）：按可用处理
	CapAvailable                       // 明确可用
	CapUnavailable                     // 明确不可用（组不存在）
)

// Capabilities 启动期探测结果：给组合根做用户可见输出，也给注册层/提示词层做裁剪。
type Capabilities struct {
	Context       string       // 实际生效的 kube context（见本节末的归一化）
	ServerVersion string       // apiserver 版本（仅用于告知用户）
	Metrics       Availability // metrics.k8s.io 的可用性
}
```

探测方法（组合根显式调用一次，`New` 保持纯装配）：

```go
// Preflight 启动期探测：判集群可达 + 探可选能力；结果同时记录在 Collector 上（注册层/提示词层读它）。
// error 只表示"集群不可达 / 鉴权失败"这类致命情形；可选能力缺失不算 error，进 Capabilities 交给调用方裁剪。
func (c *Collector) Preflight(ctx context.Context) (*Capabilities, error)
```

探测顺序与判据（都用 `c.core.Discovery()`，共享 2s 预算）：

1. `ServerVersion()`：失败即 return error（fail-closed）；成功取版本串。
2. `ServerResourcesForGroupVersion("metrics.k8s.io/v1beta1")`：无错 → `CapAvailable`；`IsNotFound` → `CapUnavailable`（明确缺失，裁掉 `k8s_metrics`）；其他错误（503、超时、权限）→ `CapUnknown`（fail-open）。

为什么判据写死 `metrics.k8s.io/v1beta1` 而不取组的 preferredVersion：探的必须是"我们的客户端要用的那个 group-version"——`metricsclient` 生成自 v1beta1，集群若只服务别的版本（`k8s.io/metrics` 里还有 v1/v1alpha1），我们的调用同样用不了，此时判"明确不可用"才是正确结论（2026-09-18 实现时核对依赖后修正：原稿写的是"v1beta2 已出现、写死会漏判"，与依赖事实不符）。这样还省掉一次调用——不用先 `ServerGroups` 找组：`/apis` 会把不可用的聚合组照样列出（APIService 挂了也列），组存在本来就是粗判据，判断可用性终究要探一次组内资源。

带 ctx 的调用变体（`...WithContext`）是让 2s 预算真正生效的路径：`DiscoveryInterface` 只承诺无 ctx 版本，真实客户端与 fake 都实现了 `DiscoveryInterfaceWithContext`，故用类型断言走带 ctx 分支、断言失败时退化为无 ctx 调用（探测仍能跑，只是不受预算约束）。

能力记录与访问器：`Preflight` 把结果副本记在 Collector 上，注册层与提示词层各读 `MetricsUsable() bool`（`CapAvailable` 与 `CapUnknown` 都返回 true，未探测也返回 true——fail-open，同时保证旧调用方与既有测试行为不变）。这样 `builtin.Deps` 不需要新增字段：能力是"同一个客户端的注解"，不是新依赖；若改用 bool 字段表达，零值 false 会让"调用方忘传"退化成静默不注册指标工具（fail-silent），比 fail-open 更糟。启动期单线程写、之后只读，不加锁。

顺带修正（与探测同一处代码）：`New` 目前把 `cfg.Context` 原样留着，而 `Collect` 的 `Target.Context` 直接取它——`kube_context` 未配置（默认场景）时它是空串，与 §9"回放时能分辨这份证据来自哪个集群"的意图不符。改为在 `New` 里归一化：`cfg.Context` 非空则用它（同时作为 clientcmd override），否则取 kubeconfig 的 `current-context`，写回 `cfg.Context`。下游（`Target.Context`、报告 meta、启动输出）不用改代码就能拿到实际生效的 context 名。

### 16.4 组合根装配与用户可见性

装配顺序：`k8s.New` 成功 → `coll.Preflight(context.Background())` → 失败则沿用既有 nil 退化（不注册、打告警）；成功则能力留在 Collector 上，另用返回值打启动输出。把探测放在 `New` 之外，是为了让 `New` 的语义边界（纯装配、无网络 IO）不变，探测也能单独测。

启动输出（成功失败都打一行，用户才能事前判断能力边界）：

- 就绪且指标可用：`K8s 诊断就绪: apiserver v1.37.0 @ https://192.168.49.2:8443（context=minikube，metrics=可用）`
- 就绪但指标明确缺失：`… @ https://192.168.49.2:8443（context=minikube，metrics=不可用（不注册 k8s_metrics；证据包的指标字段会记 optional_unavailable））`
- 就绪但指标未探测到：`… metrics=未探测到（保留 k8s_metrics，调用失败时按可选源降级）`
- 不可达：`告警: K8s 采集器未就绪（连接 apiserver 失败: ...），本次不注册 k8s_* 工具`

`/diag` 的拒绝文案要按原因区分：现在写死"检查 kubeconfig"，集群没起时是误导。改为组合根把原因串传进 `cmdDiag`（kubeconfig 层失败 / 集群不可达 / 未接入且原因不明）。

### 16.5 注册、工具清单与分支清单的一致化

三处一起改，缺一处就还是不一致：

1. 注册表（`builtin/register.go`）：`deps.K8s != nil` 时注册 8 个，`deps.K8s.MetricsUsable()` 为真才追加 `k8s_metrics`。其余 8 个工具的注册形态不变。
2. 工具清单提示词（`main.go`）：现在 `k8sColl != nil` 时追加一整段，段内含 `k8s_metrics` 一句与"用 k8s_metrics 找证据"的步骤句——拆成"基础段 + 指标句（按能力追加）"，避免提示词列出未注册的工具。
3. 工作流分支清单（`internal/workflow` + `main.go`）：`Workflow` 增 `Requires string`（frontmatter `requires:`），`k8s-diag.md` 标 `requires: k8s`；`RenderBranch()` 保留原签名（等价"全部可用"，零回归），新增 `RenderBranchFor(available map[string]bool) string`；能力名常量 `workflow.CapK8s = "k8s"` 由 workflow 包持有（能力名写在资产里，词表归资产所在包）。组合根传 `{CapK8s: k8sColl != nil}`。分支正文里对 `k8s_metrics` 的引用改成"若该工具未注册则跳过"的措辞——能力缺失时模型不会去调一个不存在的工具，省一轮浪费。

### 16.6 不做的事（边界记账）

- 不读 `KUBECONFIG`：与 §9 既有取舍、以及"配置来源单一（config.yml）+ 环境变量只留给机密"的决定一致。换集群改 `kube_config`。
- 不做 RBAC 权限自检（`SelfSubjectAccessReview`）：多 1~3 次请求，且受限集群可能不允许 SSAR，反而把"可用"误判成"不可用"；403 仍留到调用期。要加时它属于能力探测的扩展位，不动现有契约。
- 不做后台巡检 / 主动告警：那是产品边界（"用户提问才诊断"的定位），与本次改动无关。
- 不做动态重探 / 热恢复：注册表在启动期定型，集群后起要重启 small 才恢复。热恢复要让注册表可变并让提示词重算，成本远大于收益。
- 超时不进 config.yml（见 §16.3）。

### 16.7 测试与验收

单测（`internal/k8s`，新增 preflight_test.go）：

- 假 discovery（`fakediscovery` 的 `Resources` + `FakedServerVersion`）覆盖：指标可用；指标明确缺失（组不在 `Resources` 里 → NotFound）；探测报错（装 reactor 返回 503 → `CapUnknown`）。
- httptest 假 apiserver 覆盖真实 HTTP 语义：不可达（连接失败 / 500）→ `Preflight` 返回 error；超时（handler 睡超过 `Config.PreflightTimeout`，测试里压到 50ms）→ error 是 deadline exceeded。
- context 归一化：临时 kubeconfig 夹具写 `current-context: mk`，断言 `New` 后 `cfg.Context == "mk"`；显式 `Config.Context` 优先于文件。

单测（`internal/tool/builtin`）：注册一致性是本次的核心契约，用 httptest 假 apiserver + 临时 kubeconfig 造出"指标不可用"的 Collector，断言注册表里没有 `k8s_metrics`、其余 8 个在；指标可用时 9 个都在。

单测（`internal/workflow`）：`requires: k8s` 的分支在 `available` 缺 `k8s` 时不渲染、存在时渲染；无 `requires` 的分支不受影响。

真集群（`k8slab`）：`TestPreflightAgainstCluster` 断言真集群下 `Preflight` 无错、`ServerVersion` 非空、`Metrics == CapAvailable`（minikube 已开 metrics-server）。

手工冒烟（真集群才能验，fake 客户端不认拨号语义）：`minikube stop` → 启动 small → 期望"告警 + 9 个工具都不注册 + /tools 里无 k8s_* + /diag 文案是集群不可达"；`minikube start` → 启动 small → 期望"就绪一行 + 9 个工具都在"。

不回归：`go test ./... -count=1 -race`、`go vet ./...`（含 `-tags k8slab`）、`gofmt` 全绿；端到端跑通一个场景（如 `bash Zoo/k8s-lab/eval.sh --keep oom-limit-too-small`）确认工具注册与诊断链路未被破坏。

### 16.8 落地清单

- `internal/k8s/k8s.go`：`defaultPreflightTimeout`；`Availability`/`Capabilities`；`Collector` 的能力字段与 `MetricsUsable()`；`Preflight`；`New` 的 context 归一化；`Config` 增 `PreflightTimeout`。
- `internal/k8s/preflight_test.go`：上述单测（假 discovery 三态 + httptest 不可达/超时 + context 归一化）。
- `internal/k8s/preflight_lab_test.go`：真集群探测冒烟（`k8slab` tag）。
- `internal/k8s/evidence.go`：`Target.Context` 不改代码（靠归一化生效），补一句注释说明来源。
- `internal/tool/builtin/register.go`：`k8s_metrics` 条件注册。
- `internal/tool/builtin/`：注册一致性单测（httptest 假 apiserver 夹具）。
- `internal/workflow/workflow.go`：`Workflow.Requires`、`CapK8s`、`RenderBranchFor`。
- `internal/workflow/workflows/k8s-diag.md`：frontmatter 加 `requires: k8s`；步骤 5 的 `k8s_metrics` 措辞。
- `internal/workflow/workflow_test.go`：分支裁剪测试。
- `main.go`：`Preflight` 调用；启动输出；工具清单与指标句条件化；`RenderBranchFor` 调用；`cmdDiag` 传原因。
- `main_commands.go` + `main_test.go`：`cmdDiag` 文案按原因区分与测试同步。
- 本文档：本章 + §9 加指向 + §11 加一行 + §14 追加决策 + §15 追加待决。

### 16.9 风险

- 启动多一次网络请求：最坏 2s（黑洞地址等满超时；连接被拒是立即失败）。这是有意的代价，已确认不加配置开关。实测补一条：`no route to host`（路由不可达）自然失败要约 3s，预算会把它截在 2s——用户看到的是 `context deadline exceeded` 而不是更具体的网络错误，归因信息弱一点，但结论（不可达、不注册）不变。
- 探测结果有时效：见 §16.2 的边界；调用期降级仍是兜底。
- `ServerResourcesForGroupVersion` 可能因 APIService 抖动落 `CapUnknown` → 保留 `k8s_metrics`（有意的 fail-open，代价是偶尔一次调用期失败）。
- 分支裁剪是静态能力名匹配：集群恢复后不会自动让分支回到提示词，要重启 small——与"不做热恢复"一致。


## 17. 运行期连接类失败的归因与文案（2026-09-19）

### 17.1 问题

§16 解决的是"开局能不能用"；开局之后集群断开，能力声明（工具注册、提示词、分支清单）不会变，
工具调用只能以业务失败回灌。回灌的原文是传输层错误——`dial tcp 192.168.49.2:8443:
connect: connection refused`——这句话里没有"集群没了"这个结论，模型得自己从句子里推断；
推断错了就会把"取不到证据"读成"Pod 没问题"。

这条错误文本有三条通道，首版只堵了第一条、且文档按"全堵了"写（2026-09-19 收口时补齐）：

- 工具层的业务失败文案（`<动作>失败: <原始错误>`）。
- 证据包里的降级说明——`Collect` 各来源失败会把原始错误写进 `notes`（`工作负载规格未取到: dial tcp …`）、`pvc.events_error` 同理，而 `k8s_evidence` 是诊断主入口，模型读的正是这份 JSON。
- 视图里的错误字段——`NodeView.allocation_error`（`k8s_node`/`k8s_nodes` 的输出，还会经 evidence 的"节点分配汇总失败"那条 note 二次进模型）与 `PodView.sa_read_error`。

第三条路径是 `Collect` 第一步取 Pod 失败：直接 `return nil, err`，连 `Notes` 都没有，等于把"什么都没拿到"伪装成一次普通失败——这条由工具层文案兜住。

用户侧同样：启动那行早就打完了，运行期不再打印任何东西；`/diag` 虽然每次会重探目标
（10s 预算），但文案是 `读不到目标 Pod x/y：<原始错误>`，集群没起时用户拿到的是一句 dial 错误。

### 17.2 判据与边界

只归因传输层（`internal/k8s/failure.go`，纯函数、只用标准库）：

- `context.DeadlineExceeded` / `net.Error.Timeout()` / 原文含 `i/o timeout` → 调用超时。
- `*net.DNSError` / `connection refused` / `no route to host` / `network is unreachable` /
  `no such host` / `dial tcp` → 集群不可达。
- 其余（4xx 业务错误：`pods not found`、`Forbidden`）原样回灌——它们的原文已经说清，
  再包一层反而丢细节；这也是"只在系统边界防御、信任内部契约"的延续。

判据顺序：先结构化（`errors.Is`/`errors.As`），再回落到关键子串——client-go 把错误包了
`url.Error`/`net.OpError` 好几层，字符串兜底最不容易漏。子串表里超时排在 `dial tcp` 之前：
拨号超时的原文同时含两者，不能归成"连接被拒"。

作用面（"统一"以本清单为准，逐处可核对；首版只覆盖了第一条与 3 处 note，2026-09-19 收口补齐）：

- 工具层业务失败文案：`<动作>失败: ` + `ExplainError(err)`（10 处采集调用）。
- 证据包降级说明：`Collect` 的 7 处 `Notes`（工作负载 / 事件 / 日志 / Pod 指标 / 节点信息 / 节点指标 / PVC）
  与 `pvc.events_error`。
- 视图里的错误字段：`NodeView.allocation_error`（[read.go] 的 `Node` 与 `Nodes` 两处赋值）、
  `PodView.sa_read_error`——字段语义不变（仍是"这一项没取到、原因在此"），只把内容从原始错误换成归因文案。
- `/diag` 拒绝文案：`ExplainError` + 仅连接类失败时补的用户向建议（由 `IsConnFailure` 判断）。

括号里保留的是"分类依据"（`connection refused` / `i/o timeout` / `no such host` 这类），不保留完整原文。
地址与端口的去向随收口改了：原来写"由 context 交代"不成立——`context` 只是 context 名，产物里没有服务端地址；
现在落到 `Target.api_server`（证据包与报告都带，`report.md` 头部与启动行也展示，见 §17.6），
于是"连的是哪个集群/端点"在产物里可查，而模型回灌保持短。（首版测试注释与 §17.2/§17.4 曾写成
"保留原始细节、人工排查要看 dial 地址"，与实现不符，已按代码校正。）

不做的事：不重探集群、不改注册表、不做热恢复（那是 §16.6 的边界）；"首次取 Pod 失败"仍按硬失败处理、
不写成 `Notes`（它是整体失败而非单来源降级——首版的表述把它误写成了"所有连接失败都不进 Notes"）。

### 17.3 文案

`ExplainError` 只产结论短语，调用方保留既有 `<动作>失败: ` 前缀：

- 不可达：`集群不可达（connection refused）`
- 超时：`调用超时（i/o timeout）`

建议语按受众在调用方各拼一次，不写进短语里——否则要么在 10 处工具文案里重复，要么让同一句
兼具两种语气（首版就长这样：`读取 Pod 失败: 集群不可达（…）：本次未取到数据；…后重试。`，
双冒号加句号堆叠，还与其它失败文案的风格不一致）：

- 用户向：`/diag` 用 `IsConnFailure` 判定是连接类后追加 `；确认集群可达（kubectl 能连上）后重试`。
  业务错误（Pod 不存在、权限不足）不能套这句，那会给出走不通的下一步。
- 模型向：base 提示词的 k8s 段统一交代一句——"工具回灌或 notes 里出现'集群不可达/调用超时'意味着
  本次没取到数据（不是'这里没问题'）：可重试，或据实写进 missing_evidence 并压低置信度"。
  `k8s-diag` 分支步骤 6 原有的"不把没取到证据当排除"继续管分支内纪律，两句不冲突。

工具回灌因此变短且无标点堆叠：`读取 Pod 失败: 集群不可达（connection refused）`；
证据包 notes 同理：`事件未取到: 集群不可达（connection refused）`。
文案里若带 URL（client-go 的错误原文），userinfo 已脱敏为 `***`，见 §17.6 第 4 条。

### 17.4 落地清单

- `internal/k8s/failure.go`：`classifyConn` + `ExplainError`（结论短语）+ `IsConnFailure`（要不要补建议）
  + `RedactCredentials`（文本级凭据脱敏，`ExplainError` 的原文分支也过它）。
- `internal/k8s/evidence.go`：7 处 `Notes` 文案与 `pvc.events_error` 走 `ExplainError`；`Target` 填 `api_server`。
- `internal/k8s/read.go`：`Node`/`Nodes` 两处 `allocation_error`、Pod 的 `sa_read_error` 走 `ExplainError`。
- `internal/k8s/k8s.go`：`Collector` 记 `rest.Host`（`New` 里与 context 同处归一化，落值前过 `sanitizeAPIServer` 去 userinfo），供产物与启动行使用。
- `internal/k8s/view.go`：`Target` 增 `api_server`（`json:"api_server,omitempty"`）。
- `internal/k8s/report.go`：`SaveReportView` 补 `target.api_server`；`report.md` 头部按需追加 `@ <api_server>`；
  `ReportSchemaVersion` 1 → 2（常量注释要求字段增减时递增）。
- `internal/tool/builtin/k8s.go`：10 处采集失败回灌走 `k8s.ExplainError`（落盘失败仍用原文）。
- `internal/tool/builtin/k8s_test.go`：`TestK8sToolClusterDownText`（关掉的 httptest 端点当死集群，
  断言回灌含"集群不可达"与分类依据 `connection refused`）。
- `main.go`：`Preflight` 后启动行带 `@ <api_server>`；base 提示词 k8s 段补一句"集群不可达/调用超时 = 本次没取到数据"。
- `main_commands.go`：`/diag` 预检拒绝文案归因，仅连接类失败补用户向建议。

### 17.5 测试

单测（`internal/k8s`）：

- `failure_test.go`：判据（被拒 / 拨号超时 / ctx 超时 / DNS / 路由不可达）、短语形态、`IsConnFailure`；
  另加 `RedactCredentials` 的表驱动用例（`user:pass@`、只有 `user@`、无凭据、URL 夹在长文本中间、
  非 URL 的 `a@b.com` 不许误伤），并断言 `ExplainError` 的原文分支已脱敏。
- `k8s_test.go`：`sanitizeAPIServer` 用例（普通 URL 原样、`user:pass@` 去 userinfo、带 path 保留 path、解析失败返回空）。
- `TestCollect_AllSourcesConnFail`：Pod 可取、其余来源（工作负载 / 事件 / 日志 / Pod 指标 / 节点 / 节点指标 / PVC）
  全部返回连接类错误 → 遍历所有 `notes` 与 `pvc.events_error`，断言都不含传输层原文（`dial tcp`）
  且都含结论短语。首版只有一条"仅事件失败"的用例，等于 1/7 覆盖却写着覆盖了 notes 通道，本次改成全来源。
- `TestCollect_ViewErrorFieldsConclusion`：节点可取、集群范围列 Pod 与 ServiceAccount 取不到 →
  断言 `allocation_error` 的三个出口（`Node` 字段 / `Nodes` 字段 / evidence 的"节点分配汇总失败"note）
  与 `sa_read_error` 都是归因文案且不含原文。

单测（`internal/tool/builtin`）：`TestK8sToolClusterDownText`（关掉的 httptest 端点当死集群，
断言回灌含"集群不可达"与分类依据 `connection refused`；fake 客户端不认拨号语义，与 §16.7 同一取舍）。

门禁：`go test -race ./...`、`go vet ./...`（含 `-tags k8slab`）、`gofmt` 全绿。

真集群只做人工确认：本机 minikube 正常时工具不误报（不触发归因分支）。这条没有回归断言，
先记为人工结论；要机器化就得在 k8slab 加一条"正常集群下回灌文案不含'集群不可达'"的用例。

### 17.6 收口：通道统一与端点落盘（2026-09-19 同日）

首版交付有三个问题，第 4 条是同一天复核输出面时追加的，本次一并收口：

1. 通道只堵了一半，文档却按"全堵了"写。真实情况是 7 处 notes 只改了 3 处（工作负载 / 事件 / Pod 指标），日志、节点信息、节点指标、PVC 四处仍是原始错误；`allocation_error` 与 `sa_read_error` 两个视图字段完全没动，前者还会经"节点分配汇总失败"那条 note 二次进模型。后果是同一份 evidence.json 里既有 `事件未取到: 集群不可达（connection refused）`、又有 `日志未取到（容器 app…）: dial tcp …: connection refused`——正是 §17.1 要消灭的矛盾信号。现在按 §17.2 的清单统一。
2. "地址由 context 交代"的技术依据不成立：context 只是 context 名，产物里没有服务端地址，等于地址在所有产物里消失。现在 `Target` 增 `api_server`（值取 `New` 里的 `rest.Host`，与 context 同处归一化，见 §16.3），证据包与报告都带，`report.md` 头部与 CLI 启动行也展示（`apiserver v1.37.0 @ https://…（context=…）`）；模型回灌继续保持短，不把地址塞回去。
3. 测试用单点覆盖冒充统一覆盖：`TestCollect_NotesUseConnConclusion` 只让事件来源失败，所以那 4 处没改也能绿。改成 §17.5 的"全来源强断言"，并把 `allocation_error` 的两个出口一起纳入。
4. 输出面的凭据脱敏（同日复核 `net/http.stripPassword` 后补）：`api_server` 落产物/启动行前去掉 URL 里的 userinfo（`url.Parse` + `u.User = nil`，解析失败则不落，schema/host/port/path 保留）；错误文本（工具回灌、启动告警、`/diag` 拒绝文案）过文本级脱敏 `RedactCredentials`，把 `://user[:pass]@` 抹成 `://***@`。依据是红线"机密绝不入库"——kubeconfig 的 `server` 允许 `user:pass@host` 写法，而 net/http 只把 password 掩成 `***`（[client.go 的 stripPassword]），用户名会随错误原文露出来；这是本次新加 `api_server` 之后才出现的暴露面，顺手一起封掉。

契约变化：报告 JSON 增字段 → `ReportSchemaVersion` 由 1 递增到 2（常量注释要求"字段增减时递增"）；证据包无版本字段，按增字段处理；`cmd/k8seval` 读的是 `expect.json` 的 target，不受影响。

一条做事要求（供下轮复查）：文档里"N 处 / N 个来源"这类可核对的数字，必须在改完之后按代码复核一遍再写定；同时"改完复读代码 + 有测试覆盖"是硬要求。本次 overclaim 有三处（§17.2 / §17.4 / §17.5），根因是"先按计划写文档、再改代码"把计划当成了结果，而当时只有单点覆盖的测试——4 处编辑没落到磁盘，它照样是绿的。


## 实现时踩坑
##### 第一
```
错误的
v := &toPodView(p)
return v, nil

invalid operation: cannot take address of toPodView(p) (value of struct type PodView)
Go 里 &x 只能作用于可寻址的表达式（变量、指针解引用、切片元素、结构体字段等）。函数调用的返回值是一个临时值（rvalue），语言不保证它有稳定地址，所以 &f() 一律编译不过

正确的
v := toPodView(p)
return &v, nil
```

##### 第二
```
ctx 提前取消（运行期，单元测试查不出，真集群冒烟才暴露）

WorkloadOf 里 ctx, cancel := c.callCtx(ctx) 复用同名变量，Get Pod 之后立刻 cancel() ，然后把 ctx 传给后续的 ReplicaSet 查询——传下去的正是刚被取消的那个子 ctx，于是报 client rate limiter Wait returned an error: context canceled 。fake 客户端不真正走 HTTP/限流，所以单测全绿，只有真集群能暴露。

错误代码
ctx, cancel := context.WithTimeout(ctx, time.Second)
_, _ = cli.Get(ctx, "pod/a") // 这一次用完
cancel()
_, err := cli.Get(ctx, "rs/b") // err: context canceled —— ctx 变量被覆盖了

正确的代码
pctx, cancel := c.callCtx(ctx) // 子 ctx 用独立名字
p, err := cli.Get(pctx, "pod/a")
cancel()                        // 只影响它自己
// 后续用原始 ctx
rs, err := cli.Get(ctx, "rs/b")

避雷：
一，派生 ctx 一律命名区分（pctx/reqCtx），不要用 ctx, cancel := ... 覆盖入参；
二， cancel 的生命周期只覆盖它创造的那次调用，跨调用传递要用父 ctx；
三，封装"每次调用一个超时"的辅助函数时，让辅助函数自己 defer cancel 并返回不可取消的父级思路
本项目 callCtx 由调用方显式 cancel，所以更需要命名纪律。
更一般地说：fake 客户端只验证逻辑，涉及真实传输语义（ctx、限流、超时、序列化）的问题必须有一次真集群冒烟。
```
##### 第三
```
我自己把测试期望写错了（不是实现错，但值得记）

是什么： tail("aaa\nbbb\nccc\n", 8) 期望 "bbb\nccc\n" ，实际返回 "ccc\n" 。因为实现是"取末尾 n 字节 → 再从第一个换行后开始"，被切断的半行 bb 会被丢掉。 

最简单的复现
s := "aaa\nbbb\nccc\n"      // 12 字节
t := s[len(s)-8:]           // "bb\nccc\n"（从行中间切开）
i := strings.IndexByte(t, '\n') // 2
t = t[i+1:]                 // "ccc\n" —— 半行被丢弃

避雷：截断类逻辑先写清语义再写断言。
这里的语义是"保尾部 + 行首对齐（宁可少一行，不出现半行）"，如果需求其实是"尽量多保内容"，那就该改成向前找上一个换行。
语义没定清时，实现和测试会各按一种理解写，谁都没错但结果对不上。
```
