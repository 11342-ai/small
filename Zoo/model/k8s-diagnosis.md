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

组合根改动：`config.Load` 增字段 → `k8s.New(cfg)` 建采集包 → `builtin.Deps` 增 `K8s` 字段 → main.go 补 base 提示词的 k8s 工具说明句（workflow 分支清单由 `RenderBranch` 自动带出，不用手拼）；采集器构造失败只告警退化（`k8s_*` 工具不注册），不阻塞普通对话。`/diag <ns>/<pod>` 命令加在 `main_commands.go`，先做一次目标可达性预检再走 `errInject` 注入诊断请求。

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

常规合并门槛不变：`go test -race ./...`、`go vet`、`gofmt` 全绿（k8slab 测试不在其中）。

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
