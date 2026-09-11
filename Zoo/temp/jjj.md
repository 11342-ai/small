# 协作草稿 —— k8s-diag：契约、补丁、backlog 与决策

> 本文件是草稿，不是最终源码；落地由你手动执行，我不动源码目录。
> 工作模式（2026-09-11 起默认）：你手写、我评审。你动手前我只给签名/结构/资料；你说"请审查"后我才给意见与建议。
> 例外记录：A-4 经你指定走补丁模式（第 1 节给的是可直接照抄的改法）。
> 操作类型只用四种，且需你确认：插入 / 删除 / 替换 / 参考。

## 0. 状态索引（2026-09-11 已对代码复核）

- 第 1 批：采集包 `internal/k8s/{k8s,view,format,read,evidence}.go` + `k8s_test.go`（9 个单测）已落地。
- 第 1.5 批：冒烟台 `Zoo/k8s-lab/{smoke.sh,smoke.yaml}` + `internal/k8s/lab_test.go`（build tag `k8slab`）已落地。
- 补丁 A 已落地并复核通过：A-1 尾行换行、A-2 事件计数累加、A-3 事件按 uid 过滤，连同两处勘误全部到位。
  复核：`gofmt -l .` 空、`go vet ./...` 空、`go test ./... -count=1 -race` 全绿、`go test ./internal/k8s/` ok。
- A-4：补丁已给（第 1 节），待你落地。
- 冒烟环境补丁（metrics 就绪等待 + test 前探测）：已给（第 5 节），待你落地。已在影子副本上验过：`bash -n` 通过、jsonpath 在真集群返回 True、探针实测 READY。
- 设计文档已同步：§11.1（冒烟台取舍）、§12（分期改为进度制）、§4.4（串行采集为代码事实 + 并行化前提）。

### 0.1 这轮踩过的两个坑（留档）

1. 替换逻辑时旧行没删：A-2 的旧语句留在原处，新逻辑又加一次，每条事件被计两遍（3 与 2 变成 10），测试挂在合并计数上。教训：替换型改动先确认旧语句已消失——编译器不会因为"多算一次"报错。
2. 改签名后关联点漏改：A-3 把参数 `pod` 改名 `name`，错误信息里仍用 `pod`；当时被另一处语法错误挡着没暴露。教训：改名后全文件搜旧标识符（含注释与错误信息）。

## 1. A-4 补丁（补丁模式；替换两处）

现象：`TestLabHealthyPod` 挂在"事件为空"。原因与 uid 过滤无关——kube-system 那个 coredns Pod 已运行数小时，而 kube-apiserver 事件 TTL 默认 1 小时（`--event-ttl`），事件已被回收；集群刚起时能过，跑几小时后必挂。断言本身时间相关，改断言不改采集。

### 1.1 目标选择改为确定标签

| 位置 | 代码 | 操作 |
|---|---|---|
| `internal/k8s/lab_test.go:50-54`（`TestLabHealthyPod`） | 见下 | 替换 |

```go
	// 用标签选 kube-dns（Deployment 管理，有 owner 链、日志与指标都稳定），而不是取 kube-system
	// 列表首项：列表顺序无保障，可能选中静态 Pod——静态 Pod 无 controller owner，工作负载必然取不到。
	list, err := c.core.CoreV1().Pods("kube-system").List(ctx, metav1.ListOptions{LabelSelector: "k8s-app=kube-dns"})
	if err != nil || len(list.Items) == 0 {
		t.Skipf("跳过：kube-system 下没有 kube-dns Pod（%v）", err)
	}
	target := list.Items[0]
```

（被替换掉的旧代码是 `FieldSelector: "status.phase=Running"` 那一行 + 对应的 Skipf 文案 + `target := list.Items[0]`。）

### 1.2 删掉"事件非空"断言

| 位置 | 代码 | 操作 |
|---|---|---|
| `internal/k8s/lab_test.go:62-64` | 见下 | 替换 |

```go
	// 不断言"事件非空"：kube-apiserver 的事件 TTL 默认 1 小时（--event-ttl），而这里选的健康 Pod
	// 通常已运行数小时，事件早已回收——此处的空是正常现象，不是采集缺陷。
	// "事件非空"留在故障目标那条测试上（TestLabImagePullBackOff），那边的事件是刚生成的。
```

保留不动：Pod 存在、日志非空、metrics 非空、节点匹配、工作负载已解析、"正常目标不该有降级 Note"。

### 1.3 验收

1. 先过编译（不需要集群）：`go vet -tags k8slab ./internal/k8s/`
2. 起场景后跑：`go test -tags k8slab ./internal/k8s/ -run Lab -count=1 -v`，三条 tag 测试全 PASS
3. 常规门槛不受影响：`gofmt -l .` 空、`go vet ./...` 空、`go test ./... -race` 全绿

## 2. 单元 2 契约（2026-09-12 冻结，四个契约点全按建议；按它手写，写完说"请审查"）

> 完整函数签名与调用签名速查见第 6 节（对照手写用）。

### 2.1 你要写/改的四处

1. 新增 `internal/tool/builtin/k8s.go`：六个工具的构造函数 + 六个 `runK8sXxx`（执行逻辑超 10 行就外置），对齐 [kb.go](internal/tool/builtin/kb.go) 范式。
2. 改 `internal/tool/builtin/register.go`：`Deps` 增 `K8s *k8s.Collector`；`RegisterBuiltins` 内 `if deps.K8s != nil { tools = append(tools, ...) }`。
3. 改 `internal/tool/builtin/tool_permissions.go`：六项全 `policy.Pass`（全量列举，漏登记由注册期校验 fail-fast）。
4. 改 `internal/k8s`（本单元唯一的采集包改动，决策 2）：包级 `logTargets` 导出为 `LogTargets`，签名 `func LogTargets(pv *PodView) []LogQuery`；`Collect` 与既有测试的调用点跟着改名；行为不变（异常容器优先；仅在该容器有上次运行记录时追加 previous）。工具层在 container 缺省时用它取首个元素的 `Container`。

### 2.2 公共约定

1. 失败语义：参数解码失败 / 必填缺失 / 对象不存在 / API 报错 → `tool.Result{Data: "中文说明", IsError: true}, nil`；只有框架级错误（如 Collector 为 nil）才 `return err`。
2. 返回形态：视图结构体 `json.MarshalIndent` 成文本回灌（字段即视图的 JSON 标签）；日志等长文本不再二次裁剪。
3. 参数缺省一律 Go 侧兜底（`<=0` / 空串取缺省），不依赖 JSON Schema 的 `default`（非 strict 模式下不保证生效）；缺省值同时写进字段 `description`。
4. builtin → `internal/k8s` 是合法叶子方向，[imports_test.go](internal/tool/builtin/imports_test.go) 的 forbidden 列表不含它，无需改；CLAUDE.md 措辞同步见 B11。

### 2.3 六个工具（冻结）

| 工具 | 参数（必填；可选含缺省） | 调用 | Description 要点 |
|---|---|---|---|
| `k8s_pod` | namespace、pod | `Pod(ctx, ns, pod)` | 诊断起点；症状字段（phase、waiting reason、lastState 的 OOMKilled、restartCount）与规格线索、owner 链 |
| `k8s_workload` | namespace、pod | `WorkloadOf(ctx, ns, pod)` | 顶层工作负载规格真源（limits/probes/image 模板、replicas、strategy、conditions）；只支持由 Pod 上溯；裸 Pod 返回业务失败 |
| `k8s_events` | namespace、pod；`since_seconds=3600`、`limit=50` | 先 `Pod` 取 UID，再 `Events(ctx, ns, pod, uid, since, limit)` | 已去重合并计数、Warning 优先、默认 1 小时窗；调度失败/拉镜像失败/探针失败/退避都在这 |
| `k8s_logs` | namespace、pod；`container`（缺省自动选）、`previous=false`、`tail_lines=200`、`limit_bytes=65536` | container 缺省时 `LogTargets(pv)` 取容器名，再 `Logs(ctx, ns, pod, LogQuery{...})` | 默认尾部 200 行 / 64KB、超出保尾部并标 truncated；CrashLoop/OOM 要看 previous=true，且仅在该容器有上次运行记录时可用 |
| `k8s_metrics` | namespace、pod；`container`（可选，只看该容器） | `PodMetrics(ctx, ns, pod)`（container 非空则过滤，无匹配业务失败） | 实时用量 + 占 limit 百分比；metrics-server 未就绪返回"不可用"而非"Pod 不存在" |
| `k8s_node` | node（必填，不做 pod 推导） | `Node(ctx, node)` + `PodsOnNode(ctx, node)`（计数失败不影响主结果） | 节点 conditions / allocatable / taints / Pod 数；Pending 归因对比节点余量与 requests、核对 taint 与 toleration |

### 2.4 单测形态（决策 4）

只覆盖参数校验与失败语义（不触达 Collector）：缺必填、参数非法 → `IsError=true` 且 `err=nil`；再加一条注册断言（六个名字都在 `Registry.List()`、权限表齐全）。真实行为交给 lab tag 测试。不开 `newWithClients` 那个测试后门。

### 2.5 验收

`gofmt -l .` 空、`go vet ./...` 空、`go test ./... -count=1 -race` 全绿；`go test -tags k8slab ./internal/k8s/ -run Lab -count=1` 仍全绿（`LogTargets` 改名后既有断言不受影响）。

### 2.6 参考资料（要点）

- Pod/Event/Node API 参考：[pod-v1](https://kubernetes.io/docs/reference/kubernetes-api/workload-resources/pod-v1/)（`status.containerStatuses[].state/lastState`）、[event-v1](https://kubernetes.io/docs/reference/kubernetes-api/cluster-resources/event-v1/)（`involvedObject/type/reason/count`）、[node-v1](https://kubernetes.io/docs/reference/kubernetes-api/cluster-resources/node-v1/)（`status.allocatable/conditions`、`spec.taints`）。
- DeepSeek 工具调用：[api-docs.deepseek.com/zh-cn/guides/tool_calls](https://api-docs.deepseek.com/zh-cn/guides/tool_calls)（`tools[].function` 三件套；文末 strict 模式列了服务端支持的 JSON Schema 子集，写 schema 时按它约束，将来想开 strict 不用重写）。
- JSON Schema：[understanding-json-schema/reference/object](https://json-schema.org/understanding-json-schema/reference/object)。
- 项目内范式：[tool.go](internal/tool/tool.go)（Spec/Result）、[kb.go](internal/tool/builtin/kb.go)（构造函数 + runXxx + 解码失败处理）、[register.go](internal/tool/builtin/register.go)、[tool_permissions.go](internal/tool/builtin/tool_permissions.go)、CLAUDE.md「新增内置工具的标准流程」五步。

## 3. Backlog（你列的 11 条 + 我补的 3 条）

优先级判据：P0 = 破门槛或一两行能改的正确性问题；P1 = 影响诊断正确性/评测基准；P2 = 可延后。

第 2.5 批（证据面，排在工具层之后、场景集之前）

- B4 env 变量名与来源（P1）。只落"名与来源"、不落明文值（已确认）。形态：`NAME`、`NAME<-configMapRef/cm`、`NAME<-secretRef/s`、`NAME<-fieldRef/metadata.name`。
- B5 affinity（P1）。`PodView` 只有 node_selector/tolerations；Pending 归因需要 affinity 摘要（nodeAffinity required/preferred + podAntiAffinity 的 topologyKey）。
- B6 imagePullSecrets（P1，优先）。`PodView` 增 `image_pull_secrets []string`，区分"镜像名错"与"私有仓库缺 secret"。成本最低。
- B7 PVC（P1）。不新造工具，在 `Collect` 里按 Pod 引用的 claim 逐个 Get，落成 `Evidence.PVCs`。
- B8 节点余量（P1，优先）。把 `PodsOnNode` 升级为分配汇总：`{pods, requests_sum, limits_sum, free}`（free = allocatable − Σrequests）+ 目标 Pod 的 requests。Pending 归因最硬的证据。
- B9 工作负载 conditions 与 Job 语义（Job 语义错 P1，conditions 降 P2）。Job 把 `Completions` 当 `Replicas`（`read.go:650`）是误导性证据，要修；STS/DS/Job 的 conditions 价值有限，降级。
- B10 子对象事件（P1，与 B7 同批）。取 PVC 时顺带按 claim 取一次事件。

第 2 批（工具层，先于第 2.5 批）

- B11 CLAUDE.md 红线措辞（P1，文档）。改为"`builtin` 只允许 import 叶子部件（tool/policy/memory/kb/k8s），不得反向依赖 agent/session/provider/config"，并补 `tool/builtin → k8s`。

第 3 批及以后

- B12 envFromRefs 的 `prefix=` 分支：已放弃（2026-09-11，按你的决定）。理由留档：prefix 只是 env 名拼接前缀，与 B4 的"名与来源"落点重叠，单独展示收益低。
- B13 串行采集改并行（P2，本期不做）。已在设计文档 §4.4 记为代码事实；并行前必须固定 Notes 顺序。
- B14 `WorkloadOf` 未支持 kind 的兜底（P2）。建议降级为"用 Pod spec 兜底 + 说明只缺扩缩容/发布状态"。
- B15 Note 分级与测试口径（P1，待拍板）。现在 `Evidence.Notes` 是纯文本，"可选源不可用（metrics/node metrics）"与"必需来源失败（pod/events/logs/workload）"混在一起，导致老实的降级被断言判成缺陷（2026-09-12 的 metrics-server 重启窗口就是这么红的）。彻底做法：Note 带类型枚举（可选源不可用 / 来源失败 / 预算裁剪 / 无数据），测试只对"必需来源"严格。第 5 节的探针只是"等环境就绪"，没消除这个口径问题。

## 4. 决策记录与待拍板

已定

1. 批次顺序：补丁 A → 单元 2（六工具）→ 单元 3（证据包/报告工具）→ 单元 4（config/Deps/组合根/提示词）→ 单元 5（/diag + workflow 资产）→ 第 2.5 批证据面 → 第 3 批场景与评测。
2. 第 2.5 批内部：B6、B8 优先；B9 的 conditions 降 P2（Job 语义错仍修）。
3. 串行采集：本期不做（B13 P2）。
4. env 明文值：不落（2026-09-11 你确认）。
5. B12（prefix 分支）：放弃（2026-09-11 你确认）。
6. A-4：走补丁模式（2026-09-11 你指定）。
7. 交付粒度：单次一个可独立审查的小任务，代码总量 ≤400 行；默认"你手写、我评审"。
8. 单元 2 契约点 1（2026-09-12 你确认"全按建议"）：`k8s_workload` 只保留 pod 上溯，不做 kind+name 直查；设计文档 §5 表格已同步。
9. 单元 2 契约点 2：`k8s_logs` 的 container 缺省选择走"导出包级 `LogTargets(pv *PodView) []LogQuery`"，选择逻辑只有一处；设计文档 §4.1 已同步。
10. 单元 2 契约点 3：`k8s_node` 的 node 必填，不做"由 pod 推导"的备用分支；设计文档 §5 表格已同步。
11. 单元 2 契约点 4：六个工具的单测只覆盖参数校验与失败语义（不触达 Collector），不开 `newWithClients` 测试后门。

下一批（等单元 2 落地并审查后再给契约）

- 单元 3：`k8s_evidence` 与 `k8s_report` 两个工具 + 报告 JSON 结构（§7.3 字段）+ 证据包落盘入口。
- B15：Note 分级与测试口径（本轮未做，见 §5.5）。
- B11：CLAUDE.md 红线措辞（随单元 2 落地一起改最自然：那时 builtin 才真的 import k8s）。

## 5. 冒烟环境补丁：up 等 metrics 就绪 + test 前探测（2026-09-12 你选定环境侧治本）

背景（真实故障，非推断）：2026-09-12 跑 `smoke.sh test` 时 `TestLabHealthyPod` 挂在

```text
metrics 为空：metrics-server 就绪时应能取到用量
正常目标不该有降级说明: [Pod 指标未取到: ... the server is currently unable to handle the request
  (get pods.metrics.k8s.io ...) 节点指标未取到（minikube）: ... (get nodes.metrics.k8s.io ...)]
```

复核结论：metrics-server 被自己的存活探针杀掉重启（事件里 `Liveness probe failed: ... context deadline exceeded` → `Container metrics-server failed liveness probe, will be restarted`；`Last State: Terminated / Reason: Error / Exit Code: 2 / Restart Count: 3`）。重启窗口内聚合层没有 ready 后端，kube-apiserver 对 `pods.metrics.k8s.io` 的代理请求统一返回 503（`the server is currently unable to handle the request`），于是采集侧如实降级写了两条 Note，测试挂在"无降级 Note"这条断言上。采集逻辑没错，错在"环境前提未等待"。

### 5.1 新增两个函数（插入到 `wait_reason` 之后）

| 位置 | 代码 | 操作 |
|---|---|---|
| `Zoo/k8s-lab/smoke.sh`（`wait_reason` 函数之后） | 见下 | 插入 |

```bash
# metrics_ready 探测 metrics-server 是否真的能出数：APIService Available=True 只说明
# aggregation 层注册好了，还要 kubectl top 能拿到数才算就绪——刚重启的 metrics-server
# 需要一两个 scrape 周期（约 30-60 秒）才出数。返回 0=就绪，1=未就绪（原因打到 stderr）。
metrics_ready() {
  local avail
  avail=$(kubectl get apiservice v1beta1.metrics.k8s.io \
    -o jsonpath='{.status.conditions[?(@.type=="Available")].status}' 2>/dev/null || true)
  if [ "$avail" != "True" ]; then
    echo "  APIService v1beta1.metrics.k8s.io 未 Available（当前 ${avail:-未知}）" >&2
    return 1
  fi
  if ! kubectl top nodes >/dev/null 2>&1; then
    echo "  APIService 已 Available，但 kubectl top 还拿不到数（多半在重启后的首个 scrape 窗口）" >&2
    return 1
  fi
  return 0
}

# wait_metrics 等 metrics 就绪（最多约 90 秒）：把"环境前提"等到位，
# 否则 tag 测试会撞上 metrics-server 的重启窗口，把环境抖动记成采集缺陷。
wait_metrics() {
  local i
  for i in $(seq 1 30); do
    if metrics_ready; then
      echo "  metrics-server 就绪（APIService Available 且 kubectl top 有数）"
      return 0
    fi
    sleep 3
  done
  echo "metrics-server 等了约 90 秒仍未就绪，排查：" >&2
  echo "  kubectl -n kube-system get pods -l k8s-app=metrics-server" >&2
  echo "  kubectl -n kube-system describe pod -l k8s-app=metrics-server | grep -A3 'Last State'" >&2
  echo "  minikube addons enable metrics-server" >&2
  return 1
}
```

### 5.2 `up` 分支末尾加等待（插入到末条 `wait_reason` 之后）

| 位置 | 代码 | 操作 |
|---|---|---|
| `Zoo/k8s-lab/smoke.sh`（`up)` 分支，`wait_reason "$dep_pod" ImagePullBackOff` 之后） | 见下 | 插入 |

```bash
    echo "等待 metrics-server 就绪…"
    wait_metrics
```

注意：`set -e` 下 `wait_metrics` 失败会让 `up` 直接失败退出（这是我的选择：环境前提不满足就该明确暴露）。若你要"缺 metrics 也允许继续"，把调用写成 `wait_metrics || echo "警告：metrics 未就绪，继续"`。

同时建议把末行提示改准（参考，非必须）：

```bash
    echo "场景就绪：故障目标 $NS/badimg 与 $NS/$dep_pod；健康目标复用 kube-system 的 kube-dns Pod；metrics 已就绪"
```

### 5.3 `test` 分支加前置探测（插入到 `need_cluster` 之后）

| 位置 | 代码 | 操作 |
|---|---|---|
| `Zoo/k8s-lab/smoke.sh`（`test)` 分支，`need_cluster` 之后） | 见下 | 插入 |

```bash
    # 环境前提探测：metrics 未就绪时明确跳过并说明原因，不把环境抖动记成代码失败。
    if ! metrics_ready; then
      echo "跳过 tag 测试：metrics-server 未就绪（原因见上）。" >&2
      echo "先跑 bash Zoo/k8s-lab/smoke.sh up（它会等 metrics 就绪）" >&2
      exit 0
    fi
```

取舍说明：这一版是"整轮跳过"（最省事、语义最干净）。备选是"只提示仍继续跑"，另在 `lab_test.go` 里把 metrics 两条断言条件化——那属于断言侧，见下方关联条目，等你拍板再动。

### 5.4 验收

1. 正常路径：`bash Zoo/k8s-lab/smoke.sh up` 末尾应打印"metrics-server 就绪"；`smoke.sh test` 三条 tag 测试全 PASS。
2. 窗口期路径：`kubectl -n kube-system delete pod -l k8s-app=metrics-server` 立刻跑 `smoke.sh test`，应看到"跳过 tag 测试：metrics-server 未就绪"并以 0 退出（不是测试失败）。
3. 仍未就绪时会打印三条排查命令，照抄即可定位是"没装/被探针杀/未 enable"哪一种。

### 5.5 关联（待你拍板，暂不在本次补丁内）

断言侧的口径问题没有消失：`TestLabHealthyPod` 把"可选数据源瞬时不可用"与"采集有缺陷"判成同一个结果。彻底做法是 Note 分级（可选源不可用 / 来源失败 / 预算裁剪 / 无数据）后，测试只对"必需来源"严格。这条归入 backlog 的 B15（本次不实现，先记名）。

## 6. 单元 2 函数签名速查（参考；对照手写用，不含实现）

### 6.1 新增 `internal/tool/builtin/k8s.go`：六个构造函数

```go
func K8sPod(coll *k8s.Collector) tool.Tool
func K8sWorkload(coll *k8s.Collector) tool.Tool
func K8sEvents(coll *k8s.Collector) tool.Tool
func K8sLogs(coll *k8s.Collector) tool.Tool
func K8sMetrics(coll *k8s.Collector) tool.Tool
func K8sNode(coll *k8s.Collector) tool.Tool
```

构造函数的形态（与 [kb.go](internal/tool/builtin/kb.go) 同款，一行转发闭包）：

```go
return tool.NewFunc(
	tool.Spec{Name: "k8s_pod", Description: "...", Parameters: json.RawMessage(`{...}`)},
	func(ctx context.Context, args json.RawMessage) (tool.Result, error) {
		return runK8sPod(ctx, coll, args)
	},
)
```

### 6.2 六个执行函数（外置，可脱离工具壳单测）

```go
func runK8sPod(ctx context.Context, coll *k8s.Collector, args json.RawMessage) (tool.Result, error)
func runK8sWorkload(ctx context.Context, coll *k8s.Collector, args json.RawMessage) (tool.Result, error)
func runK8sEvents(ctx context.Context, coll *k8s.Collector, args json.RawMessage) (tool.Result, error)
func runK8sLogs(ctx context.Context, coll *k8s.Collector, args json.RawMessage) (tool.Result, error)
func runK8sMetrics(ctx context.Context, coll *k8s.Collector, args json.RawMessage) (tool.Result, error)
func runK8sNode(ctx context.Context, coll *k8s.Collector, args json.RawMessage) (tool.Result, error)
```

与 kb.go 的差别：`runKbXxx(store, args)` 不收 ctx（知识库不涉网络），这六个必须收——Collector 的方法都要 ctx。

### 6.3 每个 run 内部的参数解码结构（字段清单）

k8s_pod / k8s_workload

| 字段 | 类型 | json 标签 | 必填 |
|---|---|---|---|
| Namespace | string | namespace | 是 |
| Pod | string | pod | 是 |

k8s_events

| 字段 | 类型 | json 标签 | 必填 |
|---|---|---|---|
| Namespace | string | namespace | 是 |
| Pod | string | pod | 是 |
| SinceSeconds | int | since_seconds | 否（缺省 3600） |
| Limit | int | limit | 否（缺省 50） |

k8s_logs

| 字段 | 类型 | json 标签 | 必填 |
|---|---|---|---|
| Namespace | string | namespace | 是 |
| Pod | string | pod | 是 |
| Container | string | container | 否（空 = 自动选，取 `LogTargets` 首个元素的 Container） |
| Previous | bool | previous | 否（缺省 false） |
| TailLines | int | tail_lines | 否（缺省 200） |
| LimitBytes | int64 | limit_bytes | 否（缺省 65536） |

k8s_metrics

| 字段 | 类型 | json 标签 | 必填 |
|---|---|---|---|
| Namespace | string | namespace | 是 |
| Pod | string | pod | 是 |
| Container | string | container | 否（非空则只回该容器） |

k8s_node

| 字段 | 类型 | json 标签 | 必填 |
|---|---|---|---|
| Node | string | node | 是 |

### 6.3.1 参数类型统一表（六工具全量，同名字段同类型）

命名规则：Go 字段 PascalCase；`json` 标签与 JSON Schema 的 key 完全一致且用 snake_case（模型看到的参数名 = 解码用的名字，不搞两套）；`required` 只列必填；缺省值写进 `description`，Go 侧兜底。

| 参数 | Go 类型 | Schema 类型 | 必填 | 缺省 | 出现的工具 |
|---|---|---|---|---|---|
| namespace | string | string | 是 | — | pod / workload / events / logs / metrics |
| pod | string | string | 是 | — | pod / workload / events / logs / metrics |
| node | string | string | 是 | — | node |
| container | string | string | 否 | 空 = 自动选（仅 logs；metrics 的空 = 全部） | logs / metrics |
| previous | bool | boolean | 否 | false | logs |
| tail_lines | int | integer | 否 | 200 | logs |
| limit_bytes | int64 | integer | 否 | 65536 | logs |
| since_seconds | int | integer | 否 | 3600 | events |
| limit | int | integer | 否 | 50 | events |

两处类型细节：`limit_bytes` 用 int64（与 `k8s.LogQuery.LimitBytes` 一致，避免解码后再转换）；`tail_lines` 用 int（`LogQuery.TailLines` 也是 int，交给 Collector 内部转 int64）。

### 6.4 各工具回灌的视图类型

| 工具 | 回灌内容（视图类型） |
|---|---|
| k8s_pod | `*k8s.PodView` |
| k8s_workload | `*k8s.WorkloadView` |
| k8s_events | `[]k8s.EventView` |
| k8s_logs | `*k8s.LogView` |
| k8s_metrics | `*k8s.PodMetricsView`（container 非空时裁剪 `Containers`，无匹配 → 业务失败） |
| k8s_node | `*k8s.NodeView`（`PodsOnNode` 的计数填进它的 `PodsOnNode` 字段；计数失败不影响主结果） |

### 6.5 你要调用的 Collector 方法签名（已存在，`internal/k8s`）

```go
func New(cfg Config) (*Collector, error)

func (c *Collector) Pod(ctx context.Context, ns, name string) (*PodView, error)
func (c *Collector) WorkloadOf(ctx context.Context, ns, pod string) (*WorkloadView, error)
func (c *Collector) Events(ctx context.Context, ns, name, uid string, since time.Duration, limit int) ([]EventView, error)
func (c *Collector) Logs(ctx context.Context, ns, pod string, q LogQuery) (*LogView, error)
func (c *Collector) PodMetrics(ctx context.Context, ns, pod string) (*PodMetricsView, error)
func (c *Collector) Node(ctx context.Context, name string) (*NodeView, error)
func (c *Collector) PodsOnNode(ctx context.Context, node string) (int, error)

type LogQuery struct {
	Container  string
	Previous   bool
	TailLines  int
	LimitBytes int64
}
```

### 6.6 采集包一侧的唯一改动：`logTargets` 改名导出（决策 2）

| 位置 | 代码 | 操作 |
|---|---|---|
| `internal/k8s/read.go:214,217`（注释 + 定义） | `func LogTargets(pv *PodView) []LogQuery` | 替换 |
| `internal/k8s/evidence.go:39` | `for _, q := range LogTargets(pv) {` | 替换 |
| `internal/k8s/k8s_test.go:205` | `got := LogTargets(&pv)` | 替换 |
| `internal/k8s/k8s_test.go:222` | `if g := LogTargets(&healthy); len(g) != 1 \|\| g[0].Previous {` | 替换 |

行为不变：异常容器优先；仅在该容器有上次运行记录（restartCount>0 或 lastStateReason 非空）时追加 previous。

### 6.7 register.go 与 tool_permissions.go 的改动签名

```go
// Deps 增一个字段（插入）
K8s *k8s.Collector

// RegisterBuiltins 内追加（插入；与 deps.Mem/deps.Kb 同款的条件注册）
if deps.K8s != nil {
	tools = append(tools, K8sPod(deps.K8s), K8sWorkload(deps.K8s), K8sEvents(deps.K8s),
		K8sLogs(deps.K8s), K8sMetrics(deps.K8s), K8sNode(deps.K8s))
}
```

```go
// ToolPermissions 六项（插入；全量列举，漏登记由注册期校验 fail-fast）
"k8s_pod":      policy.Pass,
"k8s_workload": policy.Pass,
"k8s_events":   policy.Pass,
"k8s_logs":     policy.Pass,
"k8s_metrics":  policy.Pass,
"k8s_node":     policy.Pass,
```

## 7. 单元 2 实现模板（参考；你要的整文件模板）

用法：这是"骨架 + 一个工具写透"的模板，不是最终代码——Description 文案你可以改；六个工具的骨架一致，照着往下抄即可。两处前置：`k8s.LogTargets` 的改名（§6.6）要先落地，否则 k8s_logs 编译不过；Deps 与权限登记见 §6.7。

```go
package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"small/internal/k8s"
	"small/internal/tool"
)

// K8s 只读采集工具（单元 2）：六个薄壳，把 internal/k8s 的裁剪视图回灌给模型。
//
// 分工（别越界）：采集、裁剪、单位换算、超时、重试都在采集包；这一层只做四件事——
// 解码参数、校验必填、转发调用、序列化回灌。
// 失败语义：参数问题 / 对象不存在 / API 报错统一按业务失败（IsError=true, err=nil）回灌，
// 让模型自己换参数或换策略；只有框架级错误（如序列化失败）才 return err。
// 权限：六个工具全只读，登记在 tool_permissions.go 为 policy.Pass。

// 参数缺省值：JSON Schema 的 default 在非 strict 模式下不保证生效，故 Go 侧兜底。
const (
	k8sLogTailLines   = 200
	k8sLogLimitBytes  = 65536
	k8sEventSinceSecs = 3600
	k8sEventLimit     = 50
)

// k8sFail 业务失败：作为结果回灌模型，不中断 agent 循环。
func k8sFail(msg string) (tool.Result, error) {
	return tool.Result{Data: msg, IsError: true}, nil
}

// k8sJSON 把视图序列化成回灌文本（两空格缩进：模型读得稳，人看也友好）。
func k8sJSON(v any) (tool.Result, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		// 视图是我们自己的结构，序列化失败说明契约破了 —— 框架级错误才往上抛
		return tool.Result{}, fmt.Errorf("k8s tool: marshal view: %w", err)
	}
	return tool.Result{Data: string(data)}, nil
}

// podArgs namespace + pod 型工具的共用入参（抽出来避免几处漂移）。
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

// runK8sPod 解码 → 校验 → 采集 → 序列化。
func runK8sPod(ctx context.Context, coll *k8s.Collector, args json.RawMessage) (tool.Result, error) {
	in, err := decodePodArgs(args)
	if err != nil {
		return k8sFail(err.Error())
	}
	pv, err := coll.Pod(ctx, in.Namespace, in.Pod)
	if err != nil {
		return k8sFail("读取 Pod 失败: " + err.Error())
	}
	return k8sJSON(pv)
}

// K8sWorkload 取顶层工作负载的规格真源（Pod → ReplicaSet → Deployment 等）。
func K8sWorkload(coll *k8s.Collector) tool.Tool {
	return tool.NewFunc(
		tool.Spec{
			Name: "k8s_workload",
			Description: "读取故障 Pod 所属顶层工作负载的规格真源（Pod → ReplicaSet → Deployment，或" +
				"StatefulSet/DaemonSet/Job）：模板容器的 resources/probes/image/command、replicas、strategy、" +
				"status.conditions。回答“限制与探针当初怎么配的”时调用；裸 Pod（无 controller owner）会返回业务失败。",
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
		return k8sFail("读取工作负载失败: " + err.Error())
	}
	return k8sJSON(w)
}

// K8sEvents 读 Pod 相关事件摘要（已去重合并计数、Warning 优先）。
func K8sEvents(coll *k8s.Collector) tool.Tool {
	return tool.NewFunc(
		tool.Spec{
			Name: "k8s_events",
			Description: "读取 Pod 相关事件摘要（已按 reason+message 去重并合并发生次数、Warning 优先、" +
				"默认最近 1 小时）：调度失败（Pending）、镜像拉取失败、探针失败（Unhealthy）、容器退避（BackOff）都在这里。",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"namespace": {"type": "string", "description": "命名空间"},
					"pod": {"type": "string", "description": "Pod 名"},
					"since_seconds": {"type": "integer", "description": "时间窗（秒），缺省 3600"},
					"limit": {"type": "integer", "description": "条数上限，缺省 50"}
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

// runK8sEvents 解码 → 校验 → 先取 uid 再查事件 → 序列化。
func runK8sEvents(ctx context.Context, coll *k8s.Collector, args json.RawMessage) (tool.Result, error) {
	var in struct {
		Namespace    string `json:"namespace"`
		Pod          string `json:"pod"`
		SinceSeconds int    `json:"since_seconds"`
		Limit        int    `json:"limit"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return k8sFail("参数错误: " + err.Error())
	}
	if in.Namespace == "" || in.Pod == "" {
		return k8sFail("参数错误: namespace 与 pod 均必填")
	}
	if in.SinceSeconds <= 0 {
		in.SinceSeconds = k8sEventSinceSecs
	}
	if in.Limit <= 0 {
		in.Limit = k8sEventLimit
	}
	// 多取一次 Pod 是为了拿 uid：事件按 involvedObject.uid 过滤更准（同名异类对象不会混入）
	pv, err := coll.Pod(ctx, in.Namespace, in.Pod)
	if err != nil {
		return k8sFail("读取 Pod 失败: " + err.Error())
	}
	events, err := coll.Events(ctx, in.Namespace, in.Pod, pv.UID,
		time.Duration(in.SinceSeconds)*time.Second, in.Limit)
	if err != nil {
		return k8sFail("读取事件失败: " + err.Error())
	}
	return k8sJSON(events)
}

// K8sLogs 读容器日志（默认尾部 200 行 / 64KB）。
func K8sLogs(coll *k8s.Collector) tool.Tool {
	return tool.NewFunc(
		tool.Spec{
			Name: "k8s_logs",
			Description: "读取容器日志（默认尾部 200 行 / 64KB，超出保尾部并标 truncated）。排查 CrashLoopBackOff / " +
				"OOMKilled 要看上一次崩溃现场时用 previous=true；该参数只在该容器确有上次运行记录时可用，" +
				"否则接口会直接报错。container 缺省取“有异常记录的那个容器”。",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"namespace": {"type": "string", "description": "命名空间"},
					"pod": {"type": "string", "description": "Pod 名"},
					"container": {"type": "string", "description": "容器名，缺省自动选（异常容器优先）"},
					"previous": {"type": "boolean", "description": "读上一次运行的日志，缺省 false"},
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

// runK8sLogs 解码 → 校验 → 补缺省 →（容器名缺省时先选容器）→ 采集 → 序列化。
func runK8sLogs(ctx context.Context, coll *k8s.Collector, args json.RawMessage) (tool.Result, error) {
	var in struct {
		Namespace  string `json:"namespace"`
		Pod        string `json:"pod"`
		Container  string `json:"container"`
		Previous   bool   `json:"previous"`
		TailLines  int    `json:"tail_lines"`
		LimitBytes int64  `json:"limit_bytes"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return k8sFail("参数错误: " + err.Error())
	}
	if in.Namespace == "" || in.Pod == "" {
		return k8sFail("参数错误: namespace 与 pod 均必填")
	}
	if in.TailLines <= 0 {
		in.TailLines = k8sLogTailLines
	}
	if in.LimitBytes <= 0 {
		in.LimitBytes = k8sLogLimitBytes
	}
	if in.Container == "" {
		// 复用采集包的选择逻辑，保证与证据包选中同一个容器
		pv, err := coll.Pod(ctx, in.Namespace, in.Pod)
		if err != nil {
			return k8sFail("读取 Pod 失败: " + err.Error())
		}
		targets := k8s.LogTargets(pv)
		if len(targets) == 0 {
			return k8sFail("该 Pod 没有可读日志的容器")
		}
		in.Container = targets[0].Container
	}
	lv, err := coll.Logs(ctx, in.Namespace, in.Pod, k8s.LogQuery{
		Container:  in.Container,
		Previous:   in.Previous,
		TailLines:  in.TailLines,
		LimitBytes: in.LimitBytes,
	})
	if err != nil {
		return k8sFail("读取日志失败: " + err.Error())
	}
	return k8sJSON(lv)
}

// K8sMetrics 读 Pod 实时用量与“占 limit 百分比”。
func K8sMetrics(coll *k8s.Collector) tool.Tool {
	return tool.NewFunc(
		tool.Spec{
			Name: "k8s_metrics",
			Description: "读取 Pod 的实时用量（metrics.k8s.io，来自 metrics-server），并给出占 limit 的百分比；" +
				"判断“内存是否贴到上限”时调用。metrics-server 未就绪会返回“不可用”，不是 Pod 不存在。",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"namespace": {"type": "string", "description": "命名空间"},
					"pod": {"type": "string", "description": "Pod 名"},
					"container": {"type": "string", "description": "只看该容器，缺省返回全部容器"}
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

// runK8sMetrics 解码 → 校验 → 采集 →（按容器过滤）→ 序列化。
func runK8sMetrics(ctx context.Context, coll *k8s.Collector, args json.RawMessage) (tool.Result, error) {
	in, err := decodePodArgsWithContainer(args)
	if err != nil {
		return k8sFail(err.Error())
	}
	m, err := coll.PodMetrics(ctx, in.Namespace, in.Pod)
	if err != nil {
		return k8sFail("读取指标失败: " + err.Error())
	}
	if in.Container != "" {
		kept := make([]k8s.ContainerMetricsView, 0, 1)
		for _, cm := range m.Containers {
			if cm.Name == in.Container {
				kept = append(kept, cm)
			}
		}
		if len(kept) == 0 {
			return k8sFail(fmt.Sprintf("容器 %q 不在该 Pod 的指标里（可用容器名见 k8s_pod 的返回）", in.Container))
		}
		m.Containers = kept
	}
	return k8sJSON(m)
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

// K8sNode 读节点现状（conditions / allocatable / taints / 节点上 Pod 数）。
func K8sNode(coll *k8s.Collector) tool.Tool {
	return tool.NewFunc(
		tool.Spec{
			Name: "k8s_node",
			Description: "读取节点现状：conditions（Ready/MemoryPressure/DiskPressure/PIDPressure）、" +
				"allocatable、taints、节点上 Pod 数。Pending 归因时用它对比节点资源与 Pod requests、" +
				"核对 taint 是否被 toleration 覆盖。",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"node": {"type": "string", "description": "节点名（可从 k8s_pod 的 node_name 取）"}
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

// runK8sNode 解码 → 校验 → 采集（Pod 数为附加信息）→ 序列化。
func runK8sNode(ctx context.Context, coll *k8s.Collector, args json.RawMessage) (tool.Result, error) {
	var in struct {
		Node string `json:"node"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return k8sFail("参数错误: " + err.Error())
	}
	if in.Node == "" {
		return k8sFail("参数错误: node 必填")
	}
	nv, err := coll.Node(ctx, in.Node)
	if err != nil {
		return k8sFail("读取节点失败: " + err.Error())
	}
	// 节点上 Pod 数是附加信息：取不到不影响主结果（不失败、不打断）
	if n, err := coll.PodsOnNode(ctx, in.Node); err == nil {
		nv.PodsOnNode = n
	}
	return k8sJSON(nv)
}
```

最小单测骨架（对应决策 4；注意 `coll` 传 nil 也不会 panic——因为必填校验在调用采集之前）：

```go
// TestK8sToolsParamValidation 六个工具的参数校验路径：缺必填 → 业务失败且 err 为 nil。
func TestK8sToolsParamValidation(t *testing.T) {
	cases := []struct {
		name string
		run  func(context.Context, *k8s.Collector, json.RawMessage) (tool.Result, error)
		args string
	}{
		{"k8s_pod", runK8sPod, `{}`},
		{"k8s_workload", runK8sWorkload, `{"namespace":"default"}`},
		{"k8s_events", runK8sEvents, `{}`},
		{"k8s_logs", runK8sLogs, `{"pod":"x"}`},
		{"k8s_metrics", runK8sMetrics, `{}`},
		{"k8s_node", runK8sNode, `{}`},
	}
	for _, c := range cases {
		res, err := c.run(context.Background(), nil, json.RawMessage(c.args))
		if err != nil || !res.IsError {
			t.Errorf("%s: 缺必填应业务失败（err=nil, IsError=true），实得 res=%+v err=%v", c.name, res, err)
		}
	}
}
```

三处说明：模板里 `podArgs`/`decodePodArgs`/`containerArgs` 这样的共用小结构，是为了让六处参数校验只有一种写法；`k8sFail`/`k8sJSON` 加前缀是为了不与 builtin 里既有助手重名（已核对无冲突）；`k8sJSON` 的 `any` 需要 Go 1.18+（本项目 1.26，没问题）。

## 8. 手写过程中的答疑与两处待修（2026-09-12，答疑时顺带发现）

背景：你问"工具要不要自己去收集症状字段"。不用——见对话里的三层分工说明。但对着当前文件（`internal/tool/builtin/k8s.go`）看到两处要改：

### 8.1 `runK8sWorkload` 调错了采集方法（Description 与返回不一致）

| 位置 | 代码 | 操作 |
|---|---|---|
| `internal/tool/builtin/k8s.go:57-61`（`runK8sWorkload` 体内） | 见下 | 替换 |

```go
	w, err := coll.WorkloadOf(ctx, in.Namespace, in.Pod)
	if err != nil {
		return k8sFail("读取工作负载失败: " + err.Error())
	}
	return k8sJSON(w)
```

现状是 `coll.Pod(...)` + `k8sJSON(pv)`：k8s_workload 于是成了 k8s_pod 的副本，Description 里承诺的 `replicas/strategy/conditions` 与模板规格永远不会出现——模型会据此误判"工作负载里没有这些字段"。

### 8.2 `runK8sPod` 的签名与收尾

| 位置 | 代码 | 操作 |
|---|---|---|
| `internal/tool/builtin/k8s.go:115`（签名） | `func runK8sPod(ctx context.Context, coll *k8s.Collector, args json.RawMessage) (tool.Result, error) {` | 替换 |
| `internal/tool/builtin/k8s.go:128`（占位收尾） | `return k8sJSON(pv)` | 替换 |

两个问题：签名里的 `namespace, coll *k8s.Collector` 把 `namespace` 也声明成了 `*k8s.Collector`（能编译，因为函数参数允许未使用，但它不是契约的一部分）；收尾的 `return tool.Result{Data: ""}, fmt.Errorf("")` 是占位残留——`fmt.Errorf("")` 是非 nil 错误，会被当成框架级错误直接中断 agent 循环，而我们要的是业务失败或成功回灌。

### 8.3 重复的解码器（建议删一套）

| 位置 | 代码 | 操作 |
|---|---|---|
| `internal/tool/builtin/k8s.go:97-113`（`k8sPod` + `decodeK8sPod`） | 整块 | 删除 |

理由：与 :79-95 的 `podArgs`/`decodePodArgs` 是同一份校验的两套写法；且 `k8sPod` 的字段没写 json tag，现在靠 Go 解码的大小写不敏感匹配侥幸能对，两套并存迟早漂移。留 `podArgs`/`decodePodArgs` 一份即可。

## 9. 单元 2 审查意见（2026-09-12，对照 §2 契约 + §6 签名 + §7 模板 + CLAUDE.md 五步）

事实基础：`go build ./internal/tool/builtin/` 报一条错（`k8s.go:161: in.limit undefined`），`go vet` 同；`gofmt -l` 干净。构造函数现有 4 个（K8sPod/K8sWorkload/K8sLogs/K8sMetrics），缺 K8sEvents、K8sNode；采集包 `LogTargets` 尚未导出；`register.go`/`tool_permissions.go` 尚无 k8s 条目。

### 9.0 落地状态（2026-09-12 更新，本轮由我直接在文件系统改）

已修（§9.1 六条全做）：
- 9.1.1 `in.limit` → `in.Limit`。
- 9.1.2 `k8sFail` 丢弃返回值 → 加 `return`。
- 9.1.3 补 K8sEvents / K8sNode 两个构造函数。
- 9.1.4 events 参数带 tag 且时间窗按秒（`since_seconds` → `time.Duration(...)*time.Second`）；顺带落地 9.2.1（uid 不进参数，工具内部先取 Pod 拿 uid）。
- 9.1.5 node 参数改正（`nodeArgs.Node` + `json:"node"`）。
- 9.1.6 六个 Spec 全部补齐（Name/Description/Parameters，统一 `additionalProperties:false`）。

连带必做（不做则 Spec 与解码器对不上，或新代码带着旧毛病）：
- 9.2.2 全条落地：`logArgs` 六字段平铺；`container` 已改可选（缺省走采集包 `LogTargets` 自动选，选了哪个见返回里的 `container` 字段）。
- 9.2.3：K8sNode 里补填 `PodsOnNode`。
- 9.2.4 部分：本轮新/改写的错误文案都带上了 `err.Error()`。
- 9.2.5 部分：`runEvents`→`runK8sEvents`、`runk8sNode`→`runK8sNode`、`cool`→`coll`。
- 9.3.1/9.3.5：被改写的小节顺带换成 `// --- k8s_xxx ---` 小节注释、导入块分组。

未做（留待下一轮）：9.3.4 参数校验单测与注册断言、组合根装配（单元 4：config 三项 + main 注入 `Deps.K8s` + 提示词补句）。
已补齐：9.2.6（`LogTargets` 已导出 + 导出注释补正）、9.2.2 的 container 可选、9.2.7（`Deps.K8s` + 注册分支 + 六项 Pass 权限，已交叉核对三处名单一致）。

注意：注册分支通了 ≠ 运行时可用的六个工具——组合根（main.go）尚未把 Collector 传进 `builtin.Deps`，运行时 `deps.K8s == nil` 会直接退化（不注册）。真跑一遍要等单元 4 的装配。

验证结果：`gofmt -l` 无输出；`go build ./...`、`go vet ./...` 通过；`go test ./... -race` 全绿；六个 `Name: "k8s_*"` 齐。另用一次性脚本复刻同样的参数结构与 Spec 键名，逐项断言"键能对上、缺省能兜住"，并复现了旧写法（无 tag）下 `pod`/`since_seconds` 必然失配——全部通过。

### 9.5 组合根最小装配（2026-09-15 完成，单元 4 的第一半）

改动（三处生产代码 + 一个测试文件）：

- `internal/config/config.go`：新增 `kube_config`（缺省 `~/.kube/config`）与 `k8s_dir`（缺省 `~/.small/k8s`）两项，含 Config 字段、fileConfig yaml 键、内建默认值与 `expandHome` 展开；`Load` 的返回改成多行字面量（顺带提升可读性）。
- `main.go`：① 导入 `internal/k8s`；② 在 kb 之后建采集器 `k8s.New(Config{KubeConfig: cfg.KubeConfig, Dir: filepath.Join(cfg.K8sDir, id)})`——产物按会话分目录；③ `builtin.Deps` 增 `K8s: k8sColl`；④ 提示词里的 K8s 段条件拼接（仅当采集器就绪）。
- `internal/tool/builtin/k8s_lab_test.go`（新建，tag `k8slab`）：工具层真集群冒烟——健康 Pod（kube-dns，断言 Running/Deployment 上溯/日志自动选容器/节点 allocatable）与故障 Pod（diag-lab/badimg，断言 ImagePullBackOff/Warning/容器未启动时日志与指标是业务失败/裸 Pod 上溯业务失败）。

三条设计取舍：

1. 采集器失败不 fail fast，改为打印告警 + 退化（`k8sColl = nil` → 六个工具不注册、提示词不加 K8s 段）。理由：k8s 诊断是附加能力，不该让"没配集群"阻塞普通对话；与 `Deps` 字段为 nil 即退化的既有语义一致。若将来希望强制，改成 `log.Fatalf` 一行即可。
2. `--session` 的值直接作为产物子目录名（不做清洗）：它是本地人工输入，与 session store 的信任级别一致；模型提供的 namespace/pod 才走 `safeName` 防注入。
3. 提示词条件拼接，避免"提示词列了工具、注册表里没有"的错配。

验证：`gofmt -l` 无输出；`go build ./...`、`go vet ./...`、`go test ./... -race` 全绿；`go vet -tags k8slab ./internal/tool/builtin/` 通过（新测试可编译）。启动冒烟（假 key，不发 LLM 请求）确认运行时注册生效：

```text
$ DEEPSEEK_API_KEY=dummy bash -c 'printf "/tools\nexit\n" | go run . --session k8s-wire-smoke'
... k8s_events / k8s_logs / k8s_metrics / k8s_node / k8s_pod / k8s_workload ...
```

真集群验证（2026-09-15 补跑完成，minikube 启动后 metrics-server 自动就绪）：

- `go test -tags k8slab ./internal/k8s/` 全过（含 TestLabHealthyPod / TestLabImagePullBackOff / TestLabWorkloadChain）。
- `go test -tags k8slab ./internal/tool/builtin/ -run TestK8sToolsAgainstCluster -v` 两个子测试全过：
  健康 Pod（kube-dns）——phase=Running、k8s_workload 上溯到 Deployment、k8s_logs 自动选容器并回显容器名、k8s_node 有 allocatable；
  故障 Pod（diag-lab/badimg）——k8s_pod 给出拉镜像失败原因、k8s_events 给出 Warning、容器未启动时日志与指标回灌业务失败（err=nil）、裸 Pod 上溯工作负载回灌业务失败。
- 场景已收（`smoke.sh down`）。minikube 保持运行（用户未要求停）。

一个断言上的让步：故障 Pod 的 waiting reason 会在 `ErrImagePull` 与 `ImagePullBackOff` 之间来回跳（退避重试），所以断言接受两者——这不是实现问题，是 k8s 状态本身在闪动，写断言时要知道。

未完成：单元 3（`k8s_evidence` / `k8s_report` 工具 + 报告 JSON 结构）、单元 4 的另一半（`/diag` 命令 + `workflows/k8s-diag.md` 分支资产）、第 2.5 批证据面（B6/B8 优先）、9.3.4 的参数校验单测与注册断言。

### 9.1 阻塞项（编译或运行必错）

| 位置 | 现象 | 建议（代码） | 操作 |
|---|---|---|---|
| `k8s.go:161` | `in.limit` 字段不存在（大小写） | `in.Limit` | 替换 |
| `k8s.go:271-273` | `k8sFail(...)` 的返回值被丢弃：过滤不到容器时不返回，继续把空列表当成功 | 见下 | 替换 |
| `k8s.go` 全文 | 缺 K8sEvents / K8sNode 两个构造函数（runEvents、runk8sNode 无壳，注册不进去） | 见 9.1.3 | 插入 |
| `k8s.go:130-136` | 参数字段无 json tag：`pod` 匹配不上 `Name`，`since_seconds` 匹配不上 `Since`；且 `Since` 是 time.Duration，模型的 3600（秒）会变成 3600 纳秒 | 见 9.1.4 | 替换 |
| `k8s.go:281-283` | `podNodeArgs.Name` 无 tag，模型的 `node` 键匹配不上 → 永远报"Name 必填" | 见 9.1.5 | 替换 |
| `k8s.go:100-111,170-181,222-233` | 构造函数的 Spec 仍是占位（`Name: ""`/`Description: ""`/`Parameters: {}`）：Name 空会被权限表校验拦下（validateToolPermissions 按名字查表）；Description 空模型不知道何时调用；Parameters 空等于没告诉模型要传什么 | 见 9.1.6 | 替换 |

9.1.2 过滤不到容器时要返回：

```go
		if len(kept) == 0 {
			return k8sFail(fmt.Sprintf("容器 %q 不在该 Pod 的指标里（可用容器名见 k8s_pod 的返回）", in.Container))
		}
```

9.1.3 两个缺失的构造函数（名字与签名按 §6.1）：

```go
func K8sEvents(coll *k8s.Collector) tool.Tool
func K8sNode(coll *k8s.Collector) tool.Tool
```

9.1.4 events 参数结构与单位换算（同时解决 UID 不该做参数的问题，见 9.2.1）：

```go
type eventArgs struct {
	Namespace    string `json:"namespace"`
	Pod          string `json:"pod"`
	SinceSeconds int    `json:"since_seconds"`
	Limit        int    `json:"limit"`
}
```

调用处（seconds → Duration 必须显式换算）：

```go
	events, err := coll.Events(ctx, in.Namespace, in.Pod, pv.UID,
		time.Duration(in.SinceSeconds)*time.Second, in.Limit)
```

9.1.5 node 参数结构：

```go
type nodeArgs struct {
	Node string `json:"node"`
}
```

9.1.6 k8s_pod 的 Spec（其余四个照 §2.3 的参数表补）：

```go
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
```

### 9.2 契约偏离（能跑但结果不对或口径不一致）

| 位置 | 现象 | 建议 | 操作 |
|---|---|---|---|
| `k8s.go:130-166` | k8s_events 把 UID 做成必填参数：UID 是采集层内部细节，模型不该提供（它只能先调 k8s_pod 再回填）。契约要求工具内部先 `coll.Pod` 取 uid 再查事件 | 删 UID 参数与校验；run 内先 Pod 后 Events（片段见 9.1.4 调用处） | 替换 |
| `k8s.go:183-204` | k8s_logs 把 container 改成必填，且用嵌套 `Query k8s.LogQuery` 暴露内部类型（该结构无 json tag，`tail_lines`/`limit_bytes` 匹配不上） | 参数拍平为 6 个字段；container 空时用 `k8s.LogTargets(pv)` 取首个元素 | 替换 |
| `k8s.go:296-308` | runk8sNode 未填 `PodsOnNode`，而 Description 承诺"节点上 Pod 数" | 见下 | 插入 |
| `k8s.go:163,215,60` | 错误文案丢掉底层错误（"获取指定事件发生了错误"），模型分不清"不存在"与"权限/网络"；k8s_workload 的文案仍是"读取 Pod 失败" | 统一带上 `err.Error()`，文案按对象改（如"读取事件失败: "） | 替换 |
| `k8s.go:155,296,100,170,222` | 命名漂移：`runEvents`（应 `runK8sEvents`）、`runk8sNode`（应 `runK8sNode`）、参数名 `cool`（应 `coll`） | 按 §6.1/§6.2 统一 | 替换 |
| `internal/k8s/read.go:214,217` 等 | 前置未做：采集包仍是 `logTargets`，k8s_logs 想按契约自动选容器就调不到 | 落地 §6.6 的四处改名 | 替换 |
| `register.go`、`tool_permissions.go` | 尚无 `Deps.K8s`、无六项权限登记 → 六个构造函数齐了也不会被注册 | 按 §6.7 插入 | 插入 |

9.2.3 节点 Pod 数（附加信息，取不到不影响主结果）：

```go
	if n, err := coll.PodsOnNode(ctx, in.Node); err == nil {
		nv.PodsOnNode = n
	}
```

### 9.3 规范与可维护性

1. 分隔注释 `// = = = = = =` 与单独一行 `//`：项目里没有这种用法，建议改成小节注释（如 `// --- k8s_events ---`）或删掉。
2. 五套解码函数写法不一（有的带 tag 有的不带、校验范围不同、缺省在 decode 里做）：同形参数复用 `podArgs`/`containerArgs`，只给 events/logs/node 各一份带 tag 的参数结构。
3. 缺 doc comment：四个构造函数与多数 run 函数没有注释；CLAUDE.md 要求注释写"为什么"。
4. 缺参数校验单测（§2.4/§7 的最小骨架）。
5. 导入块没有分组空行（stdlib 与 small/internal 分开）。
6. 落地顺序建议：9.1.1/9.1.2/9.1.4/9.1.5/9.1.6（让编译过、参数面对齐）→ 9.1.3 补两个构造函数 + 9.2.1/9.2.2 契约修正 → 9.2.7 注册 + 9.3.4 单测。

### 9.4 做得对的地方（别改回去）

构造函数形态（`tool.NewFunc` + 一行转发闭包）、`k8sFail`/`k8sJSON` 两个助手、`podArgs`/`containerArgs` 的共用结构、k8s_metrics 的容器过滤思路、k8s_pod 与 k8s_workload 的调用对象选择（WorkloadOf 已改对）——骨架方向与模板一致。

## 10. 单元 2 第二轮复审（最新结论；下面的 9.5/9.6 是更早记录，问题均已解决，仅作留档）

事实基础（命令跑出来的，不是读代码猜的）：`go build ./...` 无输出、`go vet ./internal/tool/builtin/` 无输出、`go test ./internal/tool/builtin/` ok、`LogTargets` 已导出（read.go:217）、`Deps.K8s` 与六项权限已登记。唯一未过的门槛是 gofmt。

### 10.1 已修（确认无误，别回退）

A1 `in.limit` → 已改为 `in.Limit`；A3 缺构造函数 → K8sEvents/K8sNode 已补；A4 EventArgs → 已加 json tag、已去掉 UID 参数（改成工具内先取 Pod 拿 uid）；A5 node 参数 → `Node string \`json:"node"\``；A6 Spec 占位 → 六个 Spec 的 Name/Description/Parameters 已补全；B2 logs 参数 → 已拍平且 container 缺省走 `LogTargets`；B6 前置与注册 → `LogTargets` 已导出、`Deps.K8s` 与六项 Pass 已登记；C4 单测 → `k8s_test.go` 已覆盖六个工具的缺必填路径。

### 10.2 仍待修

P0（静默错数据，测试覆盖不到、跑起来也不报错）

| 位置 | 现象 | 建议 | 操作 |
|---|---|---|---|
| `k8s.go:422-424` | 条件写反：`if n, err := coll.PodsOnNode(ctx, in.Node); err != nil { pv.PodsOnNode = n }`——成功时反而不赋值，失败时赋 0，于是 `PodsOnNode` 永远为 0；而 Description 明确承诺了"节点上 Pod 数" | `err == nil` | 替换 |
| `k8s.go:362-364` | `k8sFail(...)` 的返回值被丢弃（`if len(kept) == 0 { k8sFail(...) }`）：容器名写错时不会报错，继续把空列表当成功回灌 | 见下 | 替换 |

10.2.2 过滤不到容器时要返回：

```go
		if len(kept) == 0 {
			return k8sFail(fmt.Sprintf("容器 %q 不在该 Pod 的指标里（可用容器名见 k8s_pod 的返回）", in.Container))
		}
```

P1（门槛与文案）

| 位置 | 现象 | 建议 | 操作 |
|---|---|---|---|
| `internal/tool/builtin/register.go`、`k8s_test.go` | `gofmt -l` 报这两个文件（import 顺序与续行缩进、多余空行、末尾换行）→ 红线 7 未满足 | `gofmt -w internal/tool/builtin/` | 替换 |
| `k8s.go:75` | 文案仍是"读取 Pod 失败"，但这行是工作负载 | `"读取工作负载失败: "` | 替换 |
| `k8s.go:294` | 文案丢底层错误，模型分不清"不存在"与"权限/网络" | `"读取日志失败: " + err.Error()` | 替换 |
| `k8s.go:405` | 文案"参数错误: Name 必填"（字段已改名 Node，模型传的键是 `node`） | `"参数错误: node 必填"` | 替换 |

P2（可读性与规范）

1. `k8s.go:169` 的 `SinceSecond time.Duration`：值语义是"秒数"，类型却是 Duration。数值换算当前是对的（`time.Duration(in.SinceSecond)*time.Second`），但类型与语义不一致，下一个人容易误读。建议 `SinceSeconds int`，调用处保留转换。
2. `k8s.go:166` 的 `EventArgs` 只被包内 `decodeEvents` 用，不必导出；`NameSpace` 建议写 `Namespace`（namespace 是一个词）。
3. 小节注释 `// = = = K8sPod工具 = = =` 建议统一成 `// --- k8s_pod ---`；`// 调用接口` / `// 解析参数` 属"是什么"注释（CLAUDE.md 要求写"为什么"）——同一文件里"多取一次 Pod 是为了拿 uid"那条就写得对，照那个标准统一即可。
4. `k8s.go` 的 import 块少了 stdlib 与 internal 的分组空行。
5. `k8s.go:135` 有一处多余空行。
6. 单测可再补一条"非法 JSON → 业务失败"（当前只覆盖缺必填）；可选。

### 10.3 落地记录（2026-09-12，经你指定：直接改源码，不走补丁模式）

10.2/10.2 的 P0、P1 与 P2 已全部落到 `internal/tool/builtin/k8s.go`（另含 gofmt 触及的 register.go、k8s_test.go）：

- P0-1 `k8s.go` node：`err != nil` → `err == nil`（并加了一行 why 注释说明"取不到就留 0"）。
- P0-2 `k8s.go` metrics：过滤不到容器时补 `return`。
- P1 文案三处：workload → "读取工作负载失败"；logs → "读取日志失败: " + err.Error()；node → "参数错误: node 必填"。
- P1 门槛：`gofmt -w internal/tool/builtin/`（register.go 的 import 顺序与续行缩进、k8s_test.go 的多余空行与末尾换行）。
- P2-1 `EventArgs` → 包内 `eventArgs`，`SinceSecond time.Duration` → `SinceSeconds int`（调用处保留 `time.Duration(...)*time.Second`），`NameSpace` → `Namespace`。
- P2-2 五个小节注释统一为 `// --- k8s_pod ---` 形式；删掉"// 调用接口"/"// 解析参数"这类"是什么"注释（保留"多取一次 Pod 是为了拿 uid"这类 why 注释）。
- P2-3 导入分组（stdlib 与 small/internal 之间空行）；删掉 runK8sPod 里的多余空行。
- 可选项：单测加了一条非法 JSON 用例（`{invalid` → 业务失败）。

验收（跑出来的）：`gofmt -l .` 无输出、`go vet ./...` 无输出、`go test ./... -count=1 -race` 全绿。

尚未验的部分：真集群端到端要等单元 4 把 `Deps.K8s` 注入组合根之后才有入口；`k8slab` tag 测试未跑（需集群场景处于就绪状态）。

### 9.5 单测文件（2026-09-12 补充）

`internal/tool/builtin/k8s_test.go` 目前缺文件头，`go test` 报 `expected 'package', found 'func'`（setup failed）。补上头部即可：

```go
package builtin

import (
	"context"
	"encoding/json"
	"testing"

	"small/internal/k8s"
	"small/internal/tool"
)
```

跑法（Go 以包为单位编译，没有"只跑某文件"的常规方式，用 `-run` 过滤测试名）：

```bash
go test ./internal/tool/builtin/ -run TestK8sToolsParamValidation -count=1 -v
```

另外该测试引用的 `runK8sEvents` / `runK8sNode` 与当前文件名（`runEvents` / `runk8sNode`）不一致，属 §9.2 的命名项。

### 9.6 复核更新（2026-09-12 晚）

- 已修：`in.limit` → `in.Limit`（那条编译错误消失）；k8s_workload 已改调 `coll.WorkloadOf`；k8s_logs 参数拍平并带上 json tag、`decodePodLogs` 不再强制 container（自动选容器分支已可达）。
- 当前唯一编译错误：`k8s.go:274: undefined: k8s.LogTargets`。前置 §6.6 未做——采集包仍是未导出的 `logTargets`（`read.go:217`）。Go 的导出规则要求首字母大写才能跨包访问，所以是"改名"，不是加个别名。
- 当前 gofmt 未过：`k8s.go:276` 的 `return  k8sFail(...)` 多了一个空格（实测 `gofmt -l` 报该文件；`gofmt -w` 一行解决）。
- 待改文案：`k8s.go:288` 的 `k8sFail("获取指定日志发生了错误")` 丢了 `err.Error()`（模板是 `"读取日志失败: " + err.Error()`）；同类问题见 9.2.4。
- 比模板更好的地方：k8s_logs 把解码外置成 `podLogsArgs` + `decodePodLogs`（模板用的是内联匿名 struct）——参数结构可单测、可复用，建议 events/node 也照这个风格，但记得带 json tag。

## 10. 单元 3 契约：k8s_evidence 与 k8s_report（2026-09-15 冻结，四个决策点全按建议）

### 10.1 你要写/改的四处

1. 新增 `internal/k8s/report.go`：报告结构与渲染（ReportView 及子结构 + `RenderReportMD` + `SaveReportView`）。
2. 改 `internal/tool/builtin/k8s.go`：追加 `K8sEvidence` / `K8sReport`（构造函数 + `runK8sXxx` + `reportArgs`；evidence 复用 `podArgs`）。
3. 改 `internal/tool/builtin/register.go`：`deps.K8s != nil` 分支追加两个构造函数（现为 pod/workload/events/logs/metrics/node 六个）。
4. 改 `internal/tool/builtin/tool_permissions.go`：追加 `k8s_evidence`、`k8s_report` 两项 `policy.Pass`（都只写自有受控目录 `~/.small/k8s/`，与 doc_parse 写 cache 同理）。
5. 改 `main.go`：K8s 提示词段补两句（CLAUDE.md 五步的第 4 步）——诊断开始用 `k8s_evidence` 一次拿全证据；得出结论后用 `k8s_report` 提交（缺证据必须写进 missing_evidence 并压低 confidence）。
6. 测试：`register_test.go` 的权限列表补两项，并新增一个 `Deps{K8s: &k8s.Collector{}}` 的注册断言用例（零值可用：构造函数不碰客户端）；`k8s_lab_test.go` 追加真集群断言。

### 10.2 数据结构（`internal/k8s/report.go`）

字段与 Zoo/model/k8s-diagnosis.md §7.3 一致；数组元素一律 object、不用 `$ref`/`oneOf`，保持 DeepSeek strict 子集可覆盖。

```go
type ReportView struct {
	SchemaVersion   int               `json:"schema_version"`
	GeneratedAt     string            `json:"generated_at"`
	Target          Target            `json:"target"`
	Symptoms        []string          `json:"symptoms"`
	RootCause       RootCauseView     `json:"root_cause"`
	Evidence        []EvidenceItem    `json:"evidence"`
	RuledOut        []RuledOutItem    `json:"ruled_out,omitempty"`
	MissingEvidence []MissingItem     `json:"missing_evidence,omitempty"`
	Suggestions     []SuggestionItem  `json:"suggestions"`
	Alternatives    []AlternativeItem `json:"alternatives,omitempty"`
}

type RootCauseView struct {
	Summary    string  `json:"summary"`
	Category   string  `json:"category,omitempty"`
	Confidence float64 `json:"confidence"`
}

type EvidenceItem struct {
	Source   string `json:"source"`   // 来源工具名，如 k8s_logs
	Ref      string `json:"ref"`      // 对象/字段/时间，如 badimg.spec.containers[0].resources.limits
	Excerpt  string `json:"excerpt"`  // 原文片段（日志一行/事件 message）
	Supports string `json:"supports"` // 这条证据支持什么判断
}

type RuledOutItem struct {
	Summary string `json:"summary"`
	Reason  string `json:"reason"`
}

type MissingItem struct {
	Item   string `json:"item"`   // 缺哪项证据
	Impact string `json:"impact"` // 它缺失影响什么判断
}

type SuggestionItem struct {
	Action    string `json:"action"`
	Rationale string `json:"rationale,omitempty"`
	Risk      string `json:"risk,omitempty"`
}

type AlternativeItem struct {
	Summary    string  `json:"summary"`
	Confidence float64 `json:"confidence"`
}
```

### 10.3 落盘与渲染（`internal/k8s` 侧签名）

```go
// 既有底层原语保留不动（TestSaveArtifacts 已覆盖）：写 <ns>-<pod>.report.json / .report.md。
func (c *Collector) SaveReport(t Target, reportJSON []byte, reportMD string) (string, string, error)

// 新增：自动补齐 schema_version（缺省 1）、generated_at（缺省 now）、target.context（取采集器配置），
// 再调用上面的原语。返回 json 与 md 两个落盘路径。
func (c *Collector) SaveReportView(r ReportView) (jsonPath, mdPath string, err error)

// 新增：按文档 §7.1 模板渲染（Root Cause / Symptoms / Evidence / Ruled Out / Missing Evidence / Suggestion）。
func RenderReportMD(r ReportView) string
```

证据包落盘复用既有 `Save(ev)`（写 `<ns>-<pod>.evidence.json`）。三个文件同目录同前缀，配对回放。

### 10.4 两个工具（冻结）

| 工具 | 参数 | 调用 | Description 要点 |
|---|---|---|---|
| `k8s_evidence` | namespace、pod（必填） | `Collect(ctx, ns, pod)` → `Save(ev)` → 回灌**纯**证据包 JSON | 一次拿全六类证据（Pod/工作负载/事件/日志含 previous/指标/节点）；已按预算裁剪，`notes` 里列出降级项（取不到的来源）；诊断开始先调它，之后按需用原子工具深挖 |
| `k8s_report` | namespace、pod + 报告字段（见 10.2） | 组装 `ReportView` → `SaveReportView` | 提交结构化结论并落盘；每条 evidence 必须带 source；证据不足时写进 missing_evidence 并压低 confidence；回灌"落盘路径 + 渲染后的 md"，模型可直接转给用户（格式与落盘文件一致） |

权限：两项都 `Pass`；`k8s_evidence` 的 `IsError` 语义——Collect 失败（Pod 取不到）→ 业务失败。

### 10.5 校验与失败语义

- `k8s_evidence`：namespace/pod 必填（复用 `podArgs`）。
- `k8s_report`（只校结构；置信度与证据完备度规则留给 workflow 提示词）：namespace/pod 必填；`symptoms` ≥1；`root_cause.summary` 非空；`confidence ∈ [0,1]`；`evidence` ≥1 且每条 `source` 非空；`suggestions` ≥1 且 `action` 非空。不合规 → 业务失败（`IsError=true, err=nil`）。
- 落盘 IO 失败：按业务失败回灌（文案带原始错误），不中断 agent 循环——与采集类失败口径一致。

### 10.6 单测与验收

- 单测（不触达 Collector，沿用单元 2 口径、不开测试后门）：参数校验与失败语义；注册断言（八个 `k8s_*` 进 `Registry.List()`、权限表齐全，含 `&k8s.Collector{}` 零值触发 K8s 分支）；`RenderReportMD` 是纯函数，断言 md 含关键小节与根因文本。
- 验收：`gofmt -l` 空、`go vet ./...` 空、`go test ./... -count=1 -race` 全绿；启动冒烟 `/tools` 出现 `k8s_evidence` 与 `k8s_report`；真集群（tag `k8slab`）：evidence 落盘 `~/.small/k8s/<会话 id>/diag-lab-badimg.evidence.json`，report 落盘 `.report.json` + `.report.md` 且 md 含 `Root Cause`。

### 10.7 参考资料（要点）

- DeepSeek 工具调用（看文末 strict 模式支持的 JSON Schema 子集，写参数 schema 时按它约束）：https://api-docs.deepseek.com/zh-cn/guides/tool_calls
- JSON Schema 数组/对象（`items`、`required`、`minItems`）：https://json-schema.org/understanding-json-schema/reference/array
- 项目内范式：[evidence.go](internal/k8s/evidence.go)（Collect/Save/SaveReport 与降级 Note）、[k8s.go](internal/tool/builtin/k8s.go)（六个薄壳的构造函数与 runXxx 写法）、设计文档 §7（报告模板/置信度规则/落盘命名）。

### 10.8 决策记录（2026-09-15）

1. 报告结构与渲染放 `internal/k8s`（与 PodView、SaveReport 同处），工具只解码转发。
2. 报告字段集按设计文档 §7.3 全量，不额外加"验证方式"字段（保持与文档一致；将来要加走 B 类 backlog）。
3. `k8s_evidence` 自动落盘证据包（回放与评测需要稳定入口）。
4. 工具层只校结构，置信度/证据完备度规则交 workflow 提示词——工具不做静默修正，报告字段与实际声明必须一致。

### 10.9 实现期修正与完成记录（2026-09-15）

写代码时被真集群测试当场抓到一处契约不自洽，已修正：

- 原契约写 `k8s_evidence` 回灌"证据包 JSON + 落盘路径"，实现时我写成 `"证据包已落盘: <path>\n\n" + JSON`。跑 `k8s_lab_test` 时断言"回灌是合法 JSON"直接失败——掺一句中文前缀后整段不再是 JSON，任何下游解析（评测脚本、测试辅助）都被迫先剥前缀。改为回**纯 JSON**，落点由命名约定（`<ns>-<pod>.evidence.json`）给出。
- `k8s_report` 改为回灌"落盘路径 + 渲染后的 md"（与设计文档 §5 表格一致）：模型可直接把这段转给用户，格式与落盘文件一致，不必自己重排。
- 测试辅助一分为二：`execToolOK`（断言纯 JSON）与 `execToolText`（只断言成功）——回灌形态不同的工具不该共用同一断言，这正是上面那个 bug 的教训。
- 一个延后项：若希望证据回灌里也带上落盘路径，做法是给 `Evidence` 加 `saved_to` 字段（由 `Save` 填写，保持 JSON 纯净）。当前未做，因为路径已由命名约定确定，而报告工具会回灌两个完整路径。

完成情况（单元 3 全绿）：

- 新增 `internal/k8s/report.go`（165 行）与 `report_test.go`（112 行）；`internal/tool/builtin/k8s.go` 增至 635 行（六个原子工具 + 两个新工具）、新增 `k8s_test.go`（102 行，参数校验）、`k8s_lab_test.go` 增至 230 行（加"证据包与报告落盘"子测试）。
- 注册与权限：`register.go` 的 K8s 分支现注册八个；`tool_permissions.go` 八项 `Pass`；`register_test.go` 补权限列表 + 新增 `Deps{K8s: &k8s.Collector{}}` 注册断言（含"没有 Collector 时不注册"的退化检查）。
- `main.go` 提示词补两句（先 evidence 拿全证据、结论用 report 提交）。
- 验收证据：`gofmt -l` 无输出；`go build ./...`、`go vet ./...`、`go vet -tags k8slab ./internal/tool/builtin/` 通过；`go test ./... -count=1 -race` 全绿；启动冒烟 `/tools` 列出八个 `k8s_*`；真集群 `TestK8sToolsAgainstCluster` 三个子测试（健康 Pod / 故障 Pod / 证据包与报告落盘）全 PASS。

本轮代码量约 400 行（正好在上限附近，含验证后临时补的修正）。

## 11. 单元 4 完成记录（2026-09-15）：/diag 命令 + k8s-diag 分支资产

到此为止，第 2 批（工具层）与单元 4 全部落地：诊断已是一条"能显式进入、也能被自动触发"的完整链路。

改动：

- 新增 `internal/workflow/workflows/k8s-diag.md`（15 行：frontmatter + 7 步）。步骤按"收集→分析→验证→提交"编排，并写入两类提示词层规则：按症状的必查项（CrashLoop 看 previous 日志与 lastState、OOM 看用量比与 limits 配置、ImagePull 看事件里的镜像名与错误码、Pending 看调度事件 + 节点余量 + taint、Probe 看探针配置 + Unhealthy 事件）；置信度裁决（≥80% 需两条独立证据且含决定性证据；关键证据缺失时上限 79% 并写进 missing_evidence）。这是"工具只校结构"决策的配套——规则落在提示词层，工具不做静默修正。
- `main_commands.go` 新增 `cmdDiag(coll *k8s.Collector)`：采集器为 nil → 提示检查 kubeconfig（config.yml 的 kube_config，缺省 ~/.kube/config）；参数必须是 `<ns>/<pod>`（`strings.Cut`）；再做可达性预检（`coll.Pod` + 10s 超时），读不到就报错、不烧一轮模型调用；通过则返回注入消息 + `errInject`（沿用 /pdf 的机制）。
- `main.go`：CLI 与 GUI 两个命令表都注册 `cmdDiag(k8sColl)`。
- 测试：`workflow_test.go` 新增 `TestLoad_K8sDiag`（元数据齐全 + 步骤含关键动作），`TestRenderBranch` 断言补 `[k8s-diag]`（证明它会渲染进提示词）；`main_test.go` 新增 `TestCmdDiag`（采集器 nil、五种非法参数，全部在触达采集器之前拦截）。

验证：

- `gofmt -l` 无输出；`go build ./...`、`go vet ./...`、`go vet -tags k8slab ./internal/...` 通过；`go test ./... -count=1 -race` 全绿。
- 运行时冒烟（假 key，不发真实模型请求）：
  - `/help` 列出 `/diag`（用法串正确）；
  - `/diag` 无参数 → 回显 `用法：/diag <namespace>/<pod>（如 /diag default/web-0）`，循环不退出；
  - 场景起后 `/diag diag-lab/badimg` → 预检通过、消息注入、进入 agent 调用（随后在模型鉴权处 401 失败——正好证明注入路径已打通，而不是被命令层挡住）；
  - 场景已收（`smoke.sh down`）。

遗留（下一批起点）：第 2.5 批证据面（B6 imagePullSecrets、B8 节点余量优先，B9 降级）、B11（CLAUDE.md 红线措辞）、可选的 `saved_to` 字段。

## 12. 第 2.5 批（一）：B6 拉取凭据 + B8 节点余量（2026-09-15 完成）

### B6 拉取凭据：Pod spec 与 ServiceAccount 两处合看

- `PodView` 增三个字段：`image_pull_secrets`（Pod spec 声明的）、`sa_pull_secrets`（ServiceAccount 上配置的）、`sa_read_error`（读不到 SA 的原因）。
- 为什么必须两处合看：k8s 1.24 起 SA 的 `imagePullSecrets` 不再被复制进 Pod spec（更早版本会复制），而 kubelet 拉镜像时同样生效。只看 Pod spec，会把"凭据配在 SA 上"误判成"没配凭据"，进而把 ImagePullBackOff 归因到错误方向。
- 降级可见：SA 读不到（不存在/权限不足）时列表为空，但 `sa_read_error` 有值——空列表不能被当成"没配"。
- 落点：`read.go` 的 `Pod()`（多一次 Get，同 `PodMetrics` "两次调用凑一次判断"的先例）。

### B8 节点分配账本：余量算好再交给模型

- `NodeView` 增四项：`requests_on_node`、`limits_on_node`、`free_on_node`（= allocatable − requests）、`allocation_error`；`pods_on_node` 语义收紧为"占用资源的 Pod 数（终态不计）"。
- 计算口径：终态（Succeeded/Failed）Pod 不计；每个 Pod 的每项资源取 `max(普通容器之和, init 容器最大值)`——k8s 对 init 容器取 max 而非求和，漏掉重 init 的 Pod 会把余量算大。
- 原 `PodsOnNode` 方法删除：`Node()` 内部改为"Get node + List pods(node)"两次调用，调用点（`Collect`、`k8s_node` 工具）同步收敛；降级从"调用方各自判断"改为"视图字段 `allocation_error` + `Collect` 记 Note"。
- `k8s_node` 的 Description 同步写上 `free_on_node`（保持 Spec 承诺与返回一致）。

改动文件：`view.go`、`read.go`、`evidence.go`、`builtin/k8s.go`（含 Description）、`k8s_test.go`（+2 测试）、`k8s_lab_test.go`（+`free_on_node` 断言）。

验证：`gofmt -l` 无输出；`go build ./...`、`go vet ./...`、`go vet -tags k8slab ./internal/...` 通过；`go test ./... -count=1 -race` 全绿；真集群 tag 测试两包全过（`internal/k8s` 采集层 + `internal/tool/builtin` 工具层）。

第 2.5 批剩余：env 名与来源、affinity、PVC（含其事件）、StatefulSet/DaemonSet/Job 的 conditions、Job 语义（completions/parallelism 不冒充 replicas）。

## 13. 冒烟场景加"终态 Pod" + 修复 free_on_node 的 pods 项（2026-09-15 完成）

起因：核对真集群数字时发现两件事——一是我们需要一个"终态 Pod"来演示/回归"终态不占资源"的口径，二是 `free_on_node` 的 `pods` 项算错了。

改动：

- `Zoo/k8s-lab/smoke.yaml`：加一次性 Job（`once-done`，busybox:1.36，requests 100m/32Mi，跑完即 Succeeded），制造终态 Pod。
- `Zoo/k8s-lab/smoke.sh`：`up` 先删旧 Job 再 apply（apply 同一 spec 不会重跑已完成的 Job），并新增 `wait_job_succeeded` 等 `status.succeeded ≥ 1`；就绪提示里加上终态目标。
- `internal/k8s/lab_test.go`：新增 `TestLabTerminalPodNotCounted`——断言 `pods_on_node` 小于"节点上 Pod 总数（含终态）"，且 `free_on_node["pods"]` 不等于 `allocatable["pods"]`（漏扣已用数时会相等）。
- 修复：`pods` 是个数配额，不是 requests 账本（没人给 "pods" 写 requests），原来按 requests 求和去减，永远得到"一个都没占"。新增 `usedResources(reqs, podsOnNode)` 把"已用 Pod 数"补进 used，再交给 `freeResources` 相减；`view.go` 的 `FreeOnNode` 注释同步写明这个特例。单测补断言 `free["pods"] == "109"`（110 − 1）。

验证：`gofmt -l` 无输出；`go build ./...`、`go vet ./...`、`go vet -tags k8slab ./internal/...` 通过；`go test ./... -count=1 -race` 全绿。真集群：`smoke.sh up` 后 diag-lab 出现 `once-done-* Completed`，四条 tag 测试（含新增那条）全过，场景已收。

一个过程教训（本轮踩了两次，与代码无关但与协作方式有关）：同一文件的多处编辑不要并行提交。本轮有两次"并行编辑同一文件互相覆盖"——`usedResources` 函数体与 lab 测试函数体被吞掉，而同一批里的 import 改动却落下了，症状表现成"编译报 undefined / import 未使用"这种自相矛盾的错。改法：同一文件一次只改一处，改完立刻编译；把 import 与新函数分两批提交。

## 14. 第 2.5 批（二）：PVC 与子对象事件（B7 + B10，2026-09-15 完成）

四处取舍：

- 采集入口不新造工具：`Collect` 按 Pod 的卷引用逐个 Get claim，落成 `Evidence.PVCs`。现场以 Pod 为中心，证据包一次拿全的分工不变；"单独看某块 claim"本期没有需求，等有时再说（工具是需求决定的形态）。
- 定性靠 phase 与 storage_class 的组合：Pending + 有 storageClass 是供给失败（provisioner 报错/容量不足），Pending + 无 storageClass 是"既没有匹配的现成 PV、也没法动态供给"。缺任一个都只能猜，故两个都落。storageClassName 是指针（nil 与空串都表示不指定），采集侧统一留空。
- 子对象事件必须单独取一次：服务端只按 involvedObject 过滤，Pod 的事件里看不到 claim 的进展（`ProvisioningFailed` / `waiting for first consumer` / `FailedBinding`）。拿到 PVC 的 uid 后按 uid 再 List 一次；失败写进 `events_error` 而不是留空——空与"没有事件"是两回事。
- 引用关系回填 `used_by_volumes`：同一块 claim 可被多个卷复用（如挂两处），卷名按 Pod 卷列表顺序聚合。用 slice 承载不用 map：证据包要能逐字对照两次采集。

降级口径：claim 取不到记 Note——"卷引用了一块取不到的 claim"本身就是挂载失败的证据，静默少一项等于把故障抹掉；claim 事件取不到只写 `events_error`，PVC 摘要照旧有效（两个来源互不牵连）。

有意不做：generic ephemeral 卷自动生成的那块 PVC（名字可由 `<pod>-<volume>` 推出，但它是控制器派生的对象，未绑定时 Pod 侧已有 FailedScheduling 事件，多一次猜测不值当）；PVC 的 `spec.selector` 与 `dataSource`（克隆/快照类边角情形，等有真实场景再加）。

改动文件：`view.go`（`PVCView` + `Evidence.PVCs`）、`read.go`（`PVC()` + `toPVCView`）、`format.go`（`volumeSource` 补 `ephemeral`）、`evidence.go`（`collectPVCs`/`pvcRefs` + `Collect` 接线）、`builtin/k8s.go`（`k8s_evidence` 的 Description 补 PVC，保持 Spec 承诺与返回一致）、`k8s_test.go`（+2 测试）。

验证：`gofmt -l` 无输出；`go build ./...`、`go vet ./...`、`go vet -tags k8slab ./internal/...` 通过；`go test ./... -count=1 -race` 全绿。fake 侧两条新测试覆盖"已绑定 / 未绑定（无 storageClass）+ 同一 claim 多卷引用 + claim 侧事件计数"与"claim 取不到 / 事件读失败"两种降级。真集群侧不加断言：PVC 事件走的就是 Pod 事件那段 uid 过滤代码（`TestLabImagePullBackOff` 已在真集群验证过该路径）。注意 fake clientset 不实现 field selector（事件过滤只在真集群生效），这一点写在测试注释里，免得后人以为测试漏了。Pending + 未绑定 PVC 的真集群场景留到第 3 批场景集一起建（那时要一并出 expect.json）。

第 2.5 批剩余：env 名与来源（B4）、affinity（B5）、Job 语义（B9 前半，P1）、STS/DS/Job conditions（B9 后半，P2）、B11 CLAUDE.md 红线措辞。

## 15. 第 2.5 批（三）：env 名与来源 + affinity（B4 + B5，2026-09-15 完成）

B4 env：`ContainerView.Env []string`，按已确认的形态渲染，不落明文值（值可能是机密，排查只需要知道它指哪儿）：

```text
NAME                                   spec 里写常量值
NAME<-configMapRef/app-config          valueFrom.configMapKeyRef
NAME<-secretRef/db-secret              valueFrom.secretKeyRef
NAME<-fieldRef/metadata.name           downward API（Pod 自身字段）
NAME<-resourceFieldRef/limits.memory   downward API（容器资源值）
```

一处超出既定形态的补充：`resourceFieldRef` 那条是原形态表里没有的。理由不是"顺手加功能"，而是同一组 switch 里的一个枚举——不覆盖它，用了 resourceFieldRef 的变量会渲染成裸 `NAME`，"来源"这一栏就变成错的（比缺更糟）。key（如 secret 里的 `password`）按既定形态没有落：CreateContainerConfigError 的定位靠"变量名 ← 引用哪个 cm/secret"，若后续发现事件文本里的 key 对不上、需要精确到 key，再扩成 `NAME<-secretRef/db-secret/password` 即可（改一处渲染函数）。

B5 affinity：`AffinityView` 分三类（node / pod / pod-anti），每类下再分 `required` 与 `preferred`。硬软分层的理由是诊断语义不同——required 不满足就是调度失败（Pending 根因候选），preferred 只是打分（不满足照常调度），混在一个列表里模型会把"偏好没满足"报成根因。渲染要点：

- 节点选择：一个 term 内多个表达式是 AND（文本里显式写 ` AND `），多个 term 之间是 OR（一条一个元素）。空 term 按 API 语义渲染成"不匹配任何节点"——写成"匹配全部"会把结论写反。
- Pod 亲和 term：`topologyKey=...`（域边界）+ 目标 Pod 选择器 + 命名空间范围（`namespaces` 与 `namespaceSelector` 是并集，两个都写）。
- 选择器：matchLabels 是 map，拼之前排序（顺序不稳定则证据包无法逐字对照两次采集），matchExpressions 原样。
- 未配置就返回 nil，不在 JSON 里造 `{"node_affinity":{}}` 这类空壳。

两处接线：`PodView`（实际生效的约束）与 `PodTemplateView`（发布配置真源，改回来才知道是不是模板问题）。

顺带的提示词联动：`workflows/k8s-diag.md` 第 4 步的必查项补了两处——Pending 加"调度约束（nodeSelector/tolerations/affinity，required 是硬门槛）与 PVC 绑定状态"，CrashLoop 加"env 引用的 configMap/secret 是否存在"。理由：证据采了但流程不点名，模型很可能不看（这是纯提示词改动，不满意直接回退这一句即可）。

改动文件：`view.go`（`ContainerView.Env`、`AffinityView`/`AffinityRules`、两处 Affinity 字段）、`format.go`（`envRefs`/`envValueSource`、`affinityView` 一族渲染）、`read.go`（三处接线）、`k8s_test.go`（+2 测试）、`workflows/k8s-diag.md`（必查项两处）。

一个编译期小坑（留档）：`NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution` 的类型是 `*NodeSelector`，不是 `[]NodeSelectorTerm`——term 列表在它里面一层，直接传会编译报类型不匹配。

验证：`gofmt -l` 无输出；`go build ./...`、`go vet ./...`、`go vet -tags k8slab ./internal/...` 通过；`go test ./... -count=1 -race` 全绿。新测试覆盖：五种 env 形态 + 明文值不入证据 + 容器视图接线；亲和性三类渲染、硬软分层（weight 前缀）、选择器排序、topologyKey、nil 不占字段、PodView/PodTemplateView 两处接线。

第 2.5 批剩余：Job 语义（completions/parallelism 不冒充 replicas，B9 前半，P1）、STS/DS/Job conditions（B9 后半，P2）、B11 CLAUDE.md 红线措辞（P1，文档）。

## 16. 第 2.5 批（四）：Job 语义修正 + 三类工作负载 conditions（B9，2026-09-15 完成）

Job 语义（原 P1 缺陷）：`job()` 原先把 `spec.completions` 填进 `WorkloadView.Replicas`——"要成功 3 次"会被读成"保持 3 个副本"，属于误导性证据（比缺字段更糟，因为它看起来像个正常值）。改法：

- `Replicas` 对 Job 不填（`omitempty` 后 JSON 里干脆不出现，测试就是断言这一点），语义差异写在字段注释里。
- 新增 `JobView` 块：`completions`/`parallelism`/`active`/`succeeded`/`failed`/`backoff_limit`/`conditions`。Completions 与 Parallelism 用指针保留"未指定"（k8s 各按 1 处理）与"写了 0"的区别；三个计数不带 `omitempty`——0 是"确实是 0"，省掉会被读成"没采到"。
- `backoff_limit` 与事件里的 `BackoffLimitExceeded` 是一对，之前 Job 失败最典型的根因在证据里完全没有着落。

conditions（原 P2，随本次一并收掉）：`Deployment / StatefulSet / DaemonSet / Job` 四类都接上。四者的 condition 类型不同但字段同形，故收敛到一个 `condView(typ,status,reason,message,t)` 共用，各类型只留一个 8 行的搬运函数；`deploymentConditions` 顺带改为调它（同一处逻辑不复制两份）。价值点：STS 的 `ReplicaFailure/FailedCreate`（配额或 webhook 拒绝）、Job 的 `Failed/BackoffLimitExceeded` 都是"Reason 即根因"。

改动文件：`view.go`（`JobView` + `WorkloadView.Job` + Replicas 注释）、`read.go`（`job()` 重写、`condView` 与四个条件转换函数、STS/DS 接线）、`builtin/k8s.go`（`k8s_workload` 的 Description 补 job 块说明）、`k8s_test.go`（+2 测试）。

验证：`gofmt -l` 无输出；`go build ./...`、`go vet ./...`、`go vet -tags k8slab ./internal/...` 通过；`go test ./... -count=1 -race` 全绿。新测试覆盖：Job 经 `WorkloadOf` 上溯（`Replicas==0` 且序列化后无 `replicas` 字段、completions/parallelism/计数/backoff_limit/conditions 齐全、未指定留空、计数 0 显式出现）；STS/DS 的 conditions 接线。

第 2.5 批剩余：B11（CLAUDE.md 红线措辞，P1，文档）、B15（Note 分级，待拍板）。之后即可进第 3 批（场景集 + expect.json + 端到端打分）。

## 17. B11：CLAUDE.md 红线措辞（2026-09-15 完成）

- 红线第 6 条由"只允许 import `tool`/`memory`/`policy`"改为"只允许 import 叶子部件（`tool`/`policy`/`memory`/`kb`/`k8s`）"——原文漏了 `kb` 与 `k8s`（两者早已是 builtin 的生产依赖），照原文读会以为 kb/k8s 工具写错了。
- 依赖方向那条同步：原来逐条列 `tool/builtin → policy`、`tool/builtin → memory`（叶子），改为 `tool/builtin → 叶子部件（tool/policy/memory/kb/k8s）`——叶子集合只维护一处，加新叶子时不会漏改。
- `imports_test.go`：注释同步措辞，测试名从 `TestProductionImportsStayWithinToolAndMemory` 改为 `TestProductionImportsStayWithinLeaves`（原名把叶子集合写死在名字里，加 kb/k8s 后就名不副实）。forbidden 列表（agent/session/provider/config）不动——它守的是"反向依赖"，仍是这四项。
- 顺带补了目录树缺的一行：`internal/k8s/` 一直没进 CLAUDE.md 的核心目录结构（第 2 批就落地了）。这一条不在 B11 范围内，是我改依赖那行时发现同文件内不自洽（依赖段会提到 k8s 是叶子，树里却没有它），一并补上；不认可可直接删这一行。

验证：`gofmt -l` 无输出；`go build ./...`、`go vet ./...`、`go vet -tags k8slab ./internal/...` 通过；`go test ./... -count=1 -race` 全绿（含改名后的 imports 测试）。

第 2.5 批剩余：只有 B15（Evidence.Notes 分级，P1，待拍板）。它决定第 3 批评测的门槛口径——现在 notes 把"可选源不可用（metrics/node metrics）"与"必需来源失败（pod/events/logs/workload）"混在纯文本里，测试与打分都没法区分"老实降级"与"真缺失"，建议在开始做场景集之前先拍板。

## 18. B15：Evidence.Notes 分级（2026-09-15 完成）

背景：notes 原是 `[]string`，把四类事混在纯文本里——可选源不可用、必需来源失败、预算裁剪、来源正常但没数据。后果有两头：断言只能整体宽松（放过真缺口）或整体严格（2026-09-12 把 metrics-server 重启窗口判成采集缺陷，就是严格那一头）。

契约（三个口径里选"加类型枚举"这条彻底的；另两条——测试侧匹配文本前缀、维持现状——前者脆弱后者不解决问题）：

```go
type NoteKind string
const (
	NoteRequired NoteKind = "required_failed"      // 必需来源失败 → 写 missing_evidence + 压置信度
	NoteOptional NoteKind = "optional_unavailable" // metrics 类可选源 → 不影响结论
	NoteTrimmed  NoteKind = "budget_trimmed"       // 被预算裁掉（与"没有"是两回事）
	NoteNoData   NoteKind = "no_data"              // 来源正常但没有数据（不是失败）
)
type NoteView struct { Kind NoteKind `json:"kind"`; Message string `json:"message"` }
```

Evidence.Notes 改为 `[]NoteView`（对外 JSON 从 `["..."]` 变成 `[{"kind":...,"message":...}]`，模型读到的仍是人话）。

归类（逐个 call site 定的，不是按来源名拍的）：

- required_failed：workload 未取到、事件未取到、日志读失败（非 400）、节点信息未取到、节点分配汇总失败、PVC 未取到。
- optional_unavailable：Pod 指标未取到、节点指标未取到。
- budget_trimmed：fitBudget 里每次实际裁剪各一条（原来靠消息里带"预算不足"字样，现在靠 kind）。
- no_data：容器未启动/无上次运行导致的读日志 400、以及"无可用日志"兜底一条。

一处需要判断的地方——"日志读失败"该归哪类。容器从未启动时 `GetLogs` 必失败，kubelet 用 400 回（kubectl 显示 `Error from server (BadRequest): container ... is waiting to start`）。这类失败若记成 required_failed，ImagePullBackOff 场景会凭空多一条"缺失证据"（而它恰恰是六类故障之一，评测必跑）。故新增 `logFailureKind(err)`：`apierrors.IsBadRequest` → no_data，其余（403/超时/连接）→ required_failed。这条判定单独一个函数是有意的——它能脱离客户端的日志管道被单测（fake clientset 的 GetLogs 行为不稳，不值得为它建桩）。

顺带的口径更新（纯提示词，不满意可各自回退）：

- `workflows/k8s-diag.md` 第 2 步：列出四个 kind 及处置，明确"只有 required_failed 才算缺失证据"。
- `main.go` base 提示词：同一句括注。
- `lab_test.go`：健康目标从"不许有任何 Note"改为"不许有 required_failed"（可选源抖动不再误判）；ImagePullBackOff 那条从"笔记里含'日志未取到'"改为"该笔记归 no_data"——真集群对 400 分类的验证就落在这里（需要 minikube 起一次才能跑到，本次未跑）。

改动文件：`view.go`（`NoteKind`/`NoteView`、`Evidence.Notes` 类型）、`evidence.go`（`note()` 签名、9 处 call site、`logFailureKind`）、`k8s_test.go`（`noteText`/`notesOfKind` 两个断言助手、`TestLogFailureKinds`，三个老测试改按 kind 断言）、`lab_test.go`（两处断言）、`workflows/k8s-diag.md`、`main.go`。

验证：`gofmt -l` 无输出；`go build ./...`、`go vet ./...`、`go vet -tags k8slab ./internal/...` 通过；`go test ./... -count=1 -race` 全绿。真集群 tag 测试未跑（lab 断言改了两处，等第 3 批建场景时一并跑）。

第 2.5 批到此收尾。下一步进第 3 批（场景集 + expect.json + 端到端打分）：先扩 smoke 场景到六类故障 × 变体，再定 expect.json 的字段与打分口径（缺失声明的基准现在可以直接取 notes 里 kind=required_failed 的项）。

## 19. 设计文档与代码事实对账（2026-09-15）

范围：`Zoo/model/k8s-diagnosis.md` 全文逐节对照代码（`internal/k8s/*`、`builtin/k8s.go`、`main.go`/`main_commands.go`、`config`、`workflows/k8s-diag.md`、`Zoo/k8s-lab/*`）。共修 15 处，分三类。

一、文档说的是"计划"，代码已是"另一回事"（会把后来人带偏）：

1. 头部状态还写"设计定稿待实现" → 改为已落地 + 批次进度。
2. 位置写 `builtin/k8s_*.go`（多文件）→ 实际是单文件 `builtin/k8s.go`。
3. §2 "采集六类证据" → 现在七类（加 PVC 与节点指标、拉取凭据、分配账本）。
4. §4.1 `Node(node)` / `PodsOnNode(node)` → `PodsOnNode` 已在 B8 删除并入 `Node`；`PVC(ns,name)` 方法漏列；`Pod` 的说明漏了拉取凭据两处合看。
5. §4.2 事件行写 `fieldSelector: involvedObject.name` → 实际优先用 `involvedObject.uid`（uid 空才回落 name）。
6. §4.2 Node 行没写 B8 的分配账本字段 → 补 `free_on_node` 等；新增两行（ServiceAccount 拉取凭据、PVC）。
7. §5 `k8s_logs` 参数漏 `limit_bytes`；`k8s_node`/`k8s_evidence` 返回说明补账本与 notes kind。
8. §9 YAML 写了三项，其中 `evidence_max_tokens` 没落地（仍是采集包内置默认 8192）→ 改为两项 + 说明。
9. §10 目录树写 `up.sh/down.sh/apply.sh/scenarios/` → 实际只有 `smoke.sh` + `smoke.yaml`；标注哪些已落地、哪些是第 3 批计划。
10. §14/§13 说 `/diag --context`"已覆盖" → 实际未落地（采集层支持 `Config.Context`，组合根与命令行都没接，见下面"遗留"）。

二、命令写错（照抄会失败）：

11. §10 与 `smoke.sh` 里的 `minikube addons enable metrics server` → addon 名是 `metrics-server`（4 处，含脚本里两处 `-l k8s-app=metrics server` 的标签选择器）。这处不是措辞问题：用户按提示执行会直接报错，卡在"集群连不上"这一步。

三、注释里的过期数字/范围：

12. `main.go`、`builtin/k8s.go`（两处）、`builtin/k8s_lab_test.go` 仍写"六个工具/六个薄壳"→ 单元 3 加了 `k8s_evidence`/`k8s_report`，实际八个（`register_test.go` 的"八个"是对的，反衬出这几处是漏改）。
13. §15 待决项"client-go 版本"已定（v0.37.0，与集群 v1.37.0 同版本号）→ 移出待决，落 §14 决策记录第 13 条。
14. §8 RBAC 最小集漏 `serviceaccounts`（B6 起要读 SA 的 imagePullSecrets）→ 补齐，并写清"pods 的 list 必须集群范围"（节点账本按 `spec.nodeName` 跨 ns 列 Pod，只给命名空间级权限会让 `free_on_node` 整块降级）。
15. §4.1 顺带补了"同一 Events 方法也用于取 PVC 自己的事件"（B10 的用法在文档里没有落点）。

遗留（不在对账范围，等你定）：

- `--context` / config 的 `kube_context` 没接：采集层的 `Config.Context` 已就绪（clientcmd overrides + 记进 target.context），只差 config 一个字段 + 组合根透传，约 5 行。要不要现在补，还是留在 §13 扩展位？（我不擅自加：这是配置面新开口子，属于需要拍板的事。）
- `smoke.sh` 里 `wait_job_succeeded` 的 `local ... i` 被 shellcheck 报"未使用"（循环里只当计数用），一行可清；与本次对账无关，未动。

验证：`gofmt -l` 无输出；`go build ./...`、`go vet ./...`、`go vet -tags k8slab ./internal/...`、`go test ./... -count=1 -race` 全绿；`bash -n Zoo/k8s-lab/smoke.sh` 语法通过（脚本只改提示文案与标签选择器，逻辑未动）。

## 20. 收尾两项：kube_context 落地 + smoke.sh shellcheck（2026-09-15）

一、`kube_context`（多集群，启动期选集群）：

- `config.Config` 加 `KubeContext`；`fileConfig` 加 `kube_context`；`Load` 里直接透传 `file.KubeContext`——刻意不设默认值：空串的语义就是"用 kubeconfig 的 current-context"，填个默认值反而把语义做没了（测试里专门守这条）。
- `main.go` 装配处把 `cfg.KubeContext` 传给 `k8s.Config.Context`。采集层这条路本来就通（clientcmd overrides + 记进 evidence/report 的 `target.context`，回放时能分辨证据来自哪个集群），之前只是配置面没开口子。
- 测试 `TestLoad_KubeContextFromFile`：从文件读到 `prod`；顺带断言 `kube_config` 的绝对路径不被改写；换干净 HOME 后 `kube_context` 为空。
- 没做 `/diag --context`：那要求按 context 现建 `Collector`（当前是启动期建一个），属装配层改动，且没有真实需求；不为"文档提过"顺手加，改记为 §15 待决。

二、`smoke.sh` 的 shellcheck：三个重试循环的计数器 `i` 改成 `_`（`wait_reason`/`wait_metrics`/`wait_job_succeeded`）——变量声明了但从没被读，SC2034 报警；`_` 是"只是计数"的惯用写法，行为不变。改后该文件 diagnostics 清空。

三、文档同步：设计文档 §9（YAML 三项 + 多集群说明——启动期选集群已落地，按命令临时切未做）、§13、§14 第 8 条、§15（待决项换成"是否做 /diag --context"）。CLAUDE.md 顺带补两处陈旧（第 2 批落地、文档没跟）：config.yml 示例块补 `kube_config`/`kube_context`/`k8s_dir`；对话内命令清单补 `/diag <namespace>/<pod>`。

验证：`gofmt -l` 无输出；`go build ./...`、`go vet ./...`、`go vet -tags k8slab ./internal/...`、`go test ./... -count=1 -race` 全绿；`bash -n Zoo/k8s-lab/smoke.sh` 通过、shellcheck 无告警。

## 21. 第 3 批契约：场景集与打分口径（待拍板后开工）

### 21.1 边界

冒烟台 `smoke.sh` / `smoke.yaml` 不动：它守的是"采集链路能跑通"（快、无 LLM、两个目标）。场景集是评测集（每场景一个故障形态 + 期望结论），两者并存不合并。

本批做：场景生命周期脚本 + 每个场景的采集层断言 + 端到端打分器 + 首批场景。本批不做：修复建议的验证、通过阈值门禁（LLM 输出不确定，先看基线分布再定阈值）。

### 21.2 场景集形态

`Zoo/k8s-lab/` 下新增：

```
scenario.sh                     # 单场景生命周期：list / up <name> / down <name> / down-all
eval.sh                         # 端到端评测驱动：跑一个或全部场景 → 打分表
scenarios/<symptom>-<variant>/
  manifest.yaml                 # 场景资源，命名空间统一 diag-lab
  expect.json                   # 期望结论与断言（唯一打分依据）
  README.md                     # 这个变体在故障分类里代表什么、怎么人工复核
```

四条约定：

1. 目标定位一律用 `expect.json.target.selector`（裸 Pod 也打标签），脚本解析出具体 Pod 名再喂给 /diag——Deployment 生成的 Pod 名带 hash，写死名字不可行；统一成一条路径，免得两套逻辑。
2. 场景对象一律带 `lab=<场景名>` 标签，down 按标签删（含该场景的 PVC；集群级对象如 PV 不强删，交给 storageclass 的回收策略）。
3. 就绪判定写进 expect.json 的 `wait` 段，脚本轮询等待而不是 sleep（与冒烟台同款：拉镜像失败、CrashLoop 退避都是异步的）。
4. 场景未就绪一律 Skip 并明确报出原因，不记成"诊断失败"——环境问题与模型问题必须分得开。

首批 8 个场景（六类全覆盖，且每条都能顺带验证第 2.5 批的新证据面）：

- `crashloop-app-error`：busybox 起一个立即 exit 1 的脚本（Deployment）。最典型、零镜像依赖。
- `oom-limit-too-small`：busybox 往根文件系统写大文件填 page cache，limit 64Mi（Deployment）。验证 last_state_reason / limits / 使用率三个证据面。
- `imagepull-tag-missing`：`busybox:1.36-nope`（与冒烟台那条区分开：冒烟台是镜像名不存在，这条是 tag 不存在，事件 message 不同）。
- `pending-insufficient-resources`：requests 4Gi 超节点可分配（Deployment）。验证 `free_on_node` 分支。
- `pending-pvc-unbound`：PVC 引用不存在的 storageClass（裸 Pod + PVC）。验证 PVC 采集与子对象事件——B7/B10 至今没有真集群验证，正好补上。
- `pending-node-selector`：nodeSelector / affinity required 指向不存在的标签（裸 Pod）。验证 affinity 硬门槛分支（B5）。
- `restart-liveness-kill`：应用本身正常但 liveness 端口写错（Deployment）。用来区分"探针杀的"与"应用崩的"。
- `probefailed-readiness-port`：readiness 端口写错、不配 liveness（Deployment）。验证探针配置证据面。

第二批量（等首批跑通再加）：CrashLoop 的依赖缺失与非法 flag、OOM 的堆未配、ImagePull 的缺 secret、Pending 的 taint 未容忍、ContainerRestart 的周期性 panic。

### 21.3 expect.json 字段（冻结）

```json
{
  "name": "oom-limit-too-small",
  "symptom": "OOMKilled",
  "target": { "namespace": "diag-lab", "selector": "app=oom-app" },
  "wait": { "container_state_reason": "CrashLoopBackOff", "container_last_state_reason": "OOMKilled", "timeout_seconds": 180 },
  "evidence_test": "TestScenarioOOMLimitTooSmall",
  "report": {
    "symptoms_must_include": ["OOMKilled"],
    "root_cause_category": "oom_limit_too_small",
    "confidence": { "min": 0.8 },
    "evidence_keywords": ["lastState", "OOMKilled", "limits"],
    "evidence_min_hits": 2,
    "must_not_claim": ["节点内存不足"]
  }
}
```

字段含义：

- `symptom`：六类之一，脚本据此选采集层断言与就绪等待方式。
- `wait`：就绪条件（容器当前状态 / 上次终止原因 / phase，可组合），超时即视为场景未就绪。
- `evidence_test`：采集层断言的子测试名，只作索引（判分不用它，人查用）。
- `symptoms_must_include`：症状识别要命中的词。
- `root_cause_category`：期望根因类别（是否收紧为枚举见决策点 1）。
- `confidence.min`：置信度下限——这类证据齐全的场景，模型不该低于它。
- `evidence_keywords` + `evidence_min_hits`：证据覆盖用关键词判定而非精确 ref（`report.evidence[].ref` 是模型自由文本）；命中数达 min_hits 得满分，否则按比例给分。
- `must_not_claim`：一票否决的结论。匹配范围只有 `root_cause.summary`（以及 category 值本身）——`ruled_out` 与 `alternatives` 是"被排除的候选"和"次优可能"的地盘，若在那些文本里也匹配，模型写"已排除 OOMKilled（lastState 里没有）"这种正确推理反而会被判错（2026-09-15 写场景时发现并收紧）。

### 21.4 采集层断言（确定性，不进合并门槛）

新文件 `internal/k8s/lab_scenarios_test.go`（build tag `k8slab`），每场景一个子测试，用 Go 直接断言 Evidence 结构（不用路径字符串，类型安全）；场景未起就 Skip。断言三类：症状字段对（phase / waiting reason / last_state_reason）；该症状的关键证据在（如 Pending 场景断言 `free_on_node` 非空、PVC 场景断言 `PVCs[0].phase` 与 claim 侧事件）；`notes` 里没有 required_failed。

### 21.5 端到端打分口径（冻结）

跑法：`scenario.sh up <name>` → eval.sh 解析目标 Pod → 子进程驱动 CLI（`printf '/diag <ns>/<pod>\nexit\n' | go run . --session eval-<name>`）→ 读产物目录（`~/.small/k8s/<会话 id>/`）的 report.json 与 evidence.json → 打分。

为什么走子进程而不是在测试里装配 agent：端到端就该验真入口路径；也不必把组合根装配复制一份（复制品必然漂移）。

六个打分项与权重：

1. 症状识别 0.15：`symptoms_must_include` 全中得满分，缺一按比例扣。
2. 根因类别 0.35：`category` 与 expect 一致得满分，否则 0。权重最高，因为它是结论本身。
3. 证据覆盖 0.25：`evidence_keywords` 命中数 ÷ `evidence_min_hits`（封顶 1）。
4. 缺失声明 0.15：只看漏报——证据包 notes 里 kind=required_failed 的条目，每条都要在 `report.missing_evidence` 里有对应，漏声明按覆盖率扣分（基准按条数比，非逐条配对）；多报不扣分，超出基准时只在 score.json 的 notes 里提示人工复核（口径 2026-09-15 两轮端到端实测后定稿，理由见 §33）。
5. 置信度档位 0.10：落在 expect 区间内得满分，越界按偏离度线性扣。
6. 一票否决：`must_not_claim` 命中 `root_cause.summary` → 本场景总分 0。只在根因结论上匹配，不扫 ruled_out / alternatives（理由见 §21.3）。

输出：`Zoo/k8s-lab/out/<run-id>/`（每场景一份 score.json + 汇总 summary.md），stdout 打一张表（每场景一行 + 加权总分）。本批不设通过阈值，只标出"0 分场景"与"证据覆盖不足场景"。

打分器落点：新增 `cmd/k8seval/main.go`（独立 main 包；只 import 标准库 + encoding/json，不 import internal——它读的是产物 JSON 契约）。为什么不是 Go 测试或 jq 脚本：判分读的是产物文件、与 test 生命周期无关；Go 里写判定表可读可测，且能进 `go build ./...` 的编译检查。CLAUDE.md 目录树要补 `cmd/` 一行。

不引入新依赖（打分器纯标准库）。

### 21.6 决策点（2026-09-15 已拍板，四项全按建议）

1. 根因类别收紧为枚举（先出词表草稿送审，见 §22），评分精确比对。
2. 首批做 8 个场景（六类 + Pending 三分支）。
3. OOM 场景用 busybox 填 page cache；若实测不稳再换 stress 镜像。
4. 打分器落点 `cmd/k8seval`（独立 main 包，纯标准库，读产物 JSON）。

本节其余部分已冻结；实现按 §22 词表定稿后开工。

## 22. 根因类别词表（2026-09-15 已定稿）

### 22.1 形态与落点

词表是"报告契约的一部分"，落点与传播链如下：

1. `internal/k8s/report.go` 定义 `rootCauseCategories`（值 + 一行释义）与两个渲染函数（`CategoryValuesJSON` / `CategoryLegend`）——词表只维护这一处。落地时把"常量 + 切片"合并成了一个结构体切片：23 个独立常量没有任何调用方（只有工具层拼 enum 与 doc 用它），纯增加维护面。
2. `builtin/k8s.go` 的 `k8s_report` JSON Schema 用该切片拼 `enum`，并在字段 `description` 里带一句图例（strict 子集里 enum 无逐值说明，图例只能写在 description）。模型从此只能从词表里选。
3. workflow 分支提示词补一句"根因类别必须取自 k8s_report 的参数说明给出的词表"。
4. `report.md` 渲染原样输出类别值（与 JSON 一致，便于对齐与检索），词表解释留在文档里，不塞进报告。
5. 打分侧 `cmd/k8seval` 只做字符串精确比对，不需要词表。

命名统一 snake_case 英文（与 CrashLoopBackOff 这类 k8s 术语同源，避免中英混排与空格带来的比对噪声）。

### 22.2 词表（23 个，按症状分组）

CrashLoopBackOff：

- `crashloop_app_exit`：应用自身启动即失败（非零退出码 / panic / 命令不存在），日志里有直接错误。
- `crashloop_missing_dependency`：依赖不可达（DB、缓存、下游地址、DNS 解析失败）。
- `crashloop_config_error`：配置错误（非法 flag、必填配置缺失、env 引用的 configMap/secret 缺 key，即 CreateContainerConfigError 一族）。
- `crashloop_volume_mount_error`：卷挂载或权限问题导致起不来（FailedMount、只读文件系统、挂载点被占）。

OOMKilled：

- `oom_limit_too_small`：limit 太小（用量贴限被杀，无长期增长趋势）。
- `oom_memory_leak`：内存持续增长（用量随重启次数单调上升，多次被杀）。
- `oom_heap_misconfig`：运行时堆配置超过 limit（JVM/Node 等：堆上限 + 元空间 > limit）。

ImagePullBackOff：

- `imagepull_name_invalid`：镜像名/仓库地址错（repository does not exist）。
- `imagepull_tag_missing`：tag 不存在（manifest unknown）。
- `imagepull_missing_secret`：私有仓库缺凭据（unauthorized / 401、403；Pod spec 与 ServiceAccount 两处都没有可用 secret）。
- `imagepull_registry_unreachable`：网络或 DNS 问题导致拉不到（timeout、no such host、connection refused）。

Pending：

- `pending_insufficient_resources`：节点余量不足（requests 超 allocatable，FailedScheduling 提到 Insufficient cpu/memory）。
- `pending_node_selector`：nodeSelector 或 affinity 的 required 与节点标签不匹配。
- `pending_taint_not_tolerated`：节点 taint 未被 Pod 容忍。
- `pending_pvc_unbound`：PVC 未绑定（storageClass 不存在、供给失败、无可用 PV、等待首个消费者）。
- `pending_quota_exceeded`：命名空间配额或 LimitRange 拒绝创建（ResourceQuota exceeded）。

ContainerRestart（非 OOM）：

- `restart_probe_kill`：被 liveness 探针杀，但探针配置看不出具体错配（应用启动正常，事件只说 Unhealthy/Liveness probe failed）。探针配置本身能指出端口/路径/时序错的，用 `probe_*` 类，不用它——见 §22.4。
- `restart_app_crash`：应用自身崩溃退出（非 OOM 的 panic/exit，由 restartPolicy 拉起）。
- `restart_unknown`：周期性重启但证据不足（无日志、无终态原因、无事件），须在 summary 里说明差什么证据。

Probe Failed：

- `probe_path_wrong`：探针路径错（HTTP 404）。
- `probe_port_wrong`：探针端口错（connection refused 到错误端口；liveness 因此杀容器也用它）。
- `probe_timing_too_short`：时序不合理（initialDelay/period/timeout 太短，应用启动慢于判定门槛；liveness 因此杀容器也用它）。

兜底：

- `other`：证据不足以归入上述任一类别，或属于列表外的原因。选它时必须在 `summary` 里写清是什么、以及缺哪条证据——否则等于把"没查出来"包装成结论。

### 22.3 两处取舍（请留意）

- 保留 `pending_quota_exceeded`：场景集首批没有它，但真实集群常见，且与"资源不足"是两种根因、两种建议（一个是扩配额，一个是加节点/降 requests）。模型遇到时不至于只能选 other。
- 不设"节点级故障"类别（如 node_pressure_eviction / node_not_ready）：设计文档 §2 明确本期不做节点级故障的独立归因，这类情况走 `other` 并在 summary 里说明。真要收，等第 4 批把节点级故障纳入范围时再加词。
- `other` 是逃生口，不是省事口：词表里不加"无法判断"这类等价词，避免模型用同义词规避表态；`restart_unknown` 只覆盖 ContainerRestart 那一种"证据不足"，其余症状证据不足一律 other + 说明。

### 22.4 探针类与 restart_probe_kill 的重叠裁决（2026-09-15 定）

liveness 探针配错（端口/路径/时序）与"被 liveness 探针杀"在证据上是同一件事的两面：症状是 ContainerRestart（也可能同时报 ProbeFailed），根因既可以写成 `restart_probe_kill`，也可以写成具体的 `probe_*`。口径按用户拍板定为"优先判具体错配"：

- 探针配置本身能看出错配（端口≠监听端口、路径返回 404、initialDelay/period 明显小于启动耗时），一律用 `probe_*`；此时 `probe_port_wrong` / `probe_timing_too_short` 的语义同时覆盖"容器因此被杀"的情形（`probe_path_wrong` 一般不致死，仍只用于 readniess）。
- `restart_probe_kill` 降为兜底：只在事件说了被探针杀、但探针配置看不出具体错配时用（信息不足的"被探针杀"）。

理由：`probe_*` 带的信息更多（可指导改哪一个字段），而 `restart_probe_kill` 只说"跟探针有关"，把可定位的错配写成它等于丢信息。四处落点已对齐：`internal/k8s/report.go` 的释义、分支资产的判别线索表、场景 `restart-liveness-kill` 的 expect.json 与 README。

## 23. 第 3 批实现（一）：词表落点（2026-09-15 完成）

按 §22 落地第一步（先词表 + schema，可独立审查）：

- `internal/k8s/report.go`：`RootCauseCategory`（值 + 释义）+ `rootCauseCategories` 词表（23 项）+ `CategoryValuesJSON()`（拼 enum 的字面量）、`CategoryLegend()`（一行图例）、`IsRootCauseCategory()`（校验）。`RootCauseView.Category` 注释写明取值只能来自词表。
- `builtin/k8s.go`：`k8s_report` 的参数从整块 raw string 改成 `const reportParams` 模板 + `fmt.Sprintf` 注入 enum 与图例（模板里两处 `%s`）。为什么不留静态 schema：那样词表就有两份，必然漂移。
- 顺带把 `category` 从可选改成必填：评分 0.35 的权重压在它上面，不给就无从比对；`other` 已覆盖"归不进去"的情形。
- 校验补一道：`decodeReportArgs` 里 `!k8s.IsRootCauseCategory(...)` 直接回业务失败。schema 的 enum 在非 strict 模式下不保证生效，Go 侧再挡一道——把"自造类别"变成当轮就能改的参数错误，而不是让列表外的值静静落进报告、等评测时才发现。
- workflow 第 7 步补一句"category 必须取自词表，拿不准用 other 并在 summary 里说明"。
- 测试：`internal/k8s/report_test.go` 新增 `TestRootCauseCategories`（取值唯一且 snake_case、图例覆盖全量、必须有 other、enum 字面量可被 JSON 解析且与词表逐项一致、校验函数拒绝空串与带空格的自造值）；`builtin/k8s_test.go` 新增 `TestK8sReportSchemaEnums`（schema 是合法 JSON、enum 与词表一致、描述含图例、category 在 required 里）与两个参数校验用例（缺 category / 自造 category）；既有 fixture 三处补上 category（report/render 测试、工具校验基线、lab 报告提交）。

验证：`gofmt -l` 无输出；`go build ./...`、`go vet ./...`、`go vet -tags k8slab ./internal/...`、`go test ./... -count=1 -race` 全绿。

下一步按 §21 顺序：scenario.sh + 首批 8 个场景 → `internal/k8s/lab_scenarios_test.go` 采集层断言 → `cmd/k8seval` + eval.sh。真集群要跑一轮的话，需要你先起 minikube（`minikube start && minikube addons enable metrics-server`）。

## 24. 第 3 批实现（二）：scenario.sh + 首批 4 个场景（2026-09-15 完成）

按 400 行/轮的可审查粒度，场景集分两轮落地。本轮做脚本 + 前 4 个场景（CrashLoop 应用退出、OOM limit 过小、ImagePull tag 不存在、Pending 资源不足）；下一轮做剩下 4 个（PVC 未绑定、nodeSelector 不匹配、liveness 探针杀、readiness 端口错）与采集层断言。

`Zoo/k8s-lab/scenario.sh`（与冒烟台并存，不合并）：

- 子命令 `list / up <name> / down <name> / target <name> / down-all`。`target` 打印 `ns/pod`，是 eval.sh 取诊断目标的唯一入口——selector→Pod 名的解析只留一处。
- 幂等：up 先按 `lab=<场景名>` 标签清旧对象（apply 不会重置已退避的容器，也不会重跑已完成的 Job），再 apply，再等就绪。
- 就绪判定读 expect.json 的 `wait`（哪些条件写了就等哪些：container_state_reason / container_last_state_reason / phase），轮询而不是 sleep。
- expect.json 的读取用 grep 抽四个扁平键，不引 jq：本机不一定有 jq，而这份 json 是我们自己维护的扁平结构。读不到键一律报错退出——否则会把"脚本读坏了"伪装成"场景没就绪"。
- 清理：down 按文件 + 标签双路删（含该场景的 PVC），down-all 用 `-l lab` 扫全命名空间（冒烟目标不带该标签，互不干扰）。

四个场景（每个目录三件套 manifest.yaml / expect.json / README.md）：

- `crashloop-app-error`：busybox 打印一行 fatal 后 exit 1（Deployment）。wait 用 state=CrashLoopBackOff + lastState=Error 双条件。
- `oom-limit-too-small`：容器一次性把 100MB 读进 shell 变量（匿名内存），limit 64Mi → OOMKilled。原契约写的是"写大文件填 page cache"，实现时改了：干净页会被内核回收，可能只变慢而不 OOM；匿名内存没有回收余地，行为确定，且更像"一条平线撞上限"（与内存泄漏的形态区别）。这是对决策 3 的修正，换镜像那条备选仍然留着。
- `imagepull-tag-missing`：`busybox:1.36-nope`（仓库可达、tag 不存在；与冒烟台那条 message 不同）。
- `pending-insufficient-resources`：requests 100 CPU / 200Gi，任何本地节点都放不下；wait 用 phase=Pending。

写法上的几处约定：Pod 模板也打 `lab` 标签（这样 Pod 级清理与筛选都生效）；场景资源用的镜像只有 busybox:1.36（不新增镜像依赖）；场景 README 写"这个变体代表什么 + 怎么人工复核 + 期望结论"，供人工抽查时对照。

验证（本机无集群，故只到静态这一层）：`bash -n scenario.sh` 通过、shellcheck 无告警；`scenario.sh list` 正确列出四个场景（不连集群也能跑）；四份 expect.json 过 `python3 -m json.tool`；四份 manifest.yaml 过 yaml 解析（kind 分别为 Deployment / Pod / Deployment / Deployment）。

待办：起集群后跑 `scenario.sh up` 逐个确认就绪（尤其 OOM 那条的匿名内存能否稳定触发 OOMKilled、CrashLoopBackOff 的退避时长是否在 180s 内），再进下一轮。

## 25. 第 3 批实现（三）：剩余 4 个场景 + 就绪条件补两类（2026-09-15 完成）

首批 8 个场景补齐，六类症状全覆盖（Pending 三分支）。

四个新场景：

- `pending-pvc-unbound`：PVC 指向不存在的 storageClass（`lab-no-such-sc`）+ 引用它的 Deployment。claim 永远 Pending → 调度器给出 unbound immediate PVC，Pod 卡 Pending；验证的是 §4.1 那条子对象证据链（`PVCs[0].phase` + claim 自己的事件）。选"类不存在"而不是"容量供给失败"：外部供给者的报错时机与文本不稳定，判据不硬。
- `pending-node-selector`：`nodeSelector: lab-node-role=does-not-exist`。与资源不足同为 Pending，但事件是 `node(s) didn't match Pod's node affinity/selector`；要模型把"Pod 要的键值"与 node 的 Labels 对起来，不能看到余量数字不好看就改判。
- `restart-liveness-kill`：busybox httpd 正常监听 8080，liveness 探 8081（端口写错）→ 被 kubelet 反复杀掉重启。这是本批最刁的一条：退出码 137 与 OOMKilled 同码，但 `lastState.terminated.reason` 是 Error 且事件明写 liveness 失败——只凭退出码报"内存超限被杀"会被 must_not_claim 一票否决。
- `probefailed-readiness-port`：同一形态换成 readiness 探针 → 容器不退出不重启，Pod 永远 NotReady（restartCount=0）。与上一条成对，专门用于分辨"探针错导致重启"与"探针错导致不可用"。

`scenario.sh` 的就绪条件补两类（原先只有 state reason / lastState reason / phase）：

- `container_restart_count_min`：ContainerRestart 类场景唯一可靠的判据——容器被杀后立刻又起来，waiting reason 时有时无。比较用 `-lt`，就绪输出带 `restarts=`。
- `pod_ready`：readiness 类场景的判据（`{.status.conditions[?(@.type=="Ready")].status}`）。为什么要它：phase 在容器还没拉起来时也是 Running，光看 phase 会把"还没起"当成就绪；readiness 场景的 wait 写成 `phase=Running + pod_ready=False` 两条组合才准确。
- 读取与校验沿用既有约定：新增 `json_opt_num()` 读可选数字键，读不到键一律报错退出；"至少要给一个条件"的报错文案同步列出五个键。

写法上沿用首批约定（Pod 模板打 `lab` 标签、只用 busybox:1.36 镜像、README 三段式：这个变体代表什么 / 怎么人工复核 / 期望结论）。expect.json 的 `must_not_claim` 一律写成"别处症状的错误结论"而非否定句，避免模型在 summary 里写"不是 X"被误判。

一处对 §21.2 的偏离（2026-09-15 已拍板：保持 Deployment）：`pending-pvc-unbound` 与 `pending-node-selector` 原写"裸 Pod"，实现时改成 Deployment（前者 + PVC 两份对象）。理由：真实集群里 Pending 的 Pod 绝大多数由控制器托管，Deployment 更接近被诊断对象；裸 Pod 那条路径已由 `imagepull-tag-missing` 覆盖（含 `WorkloadOf` 取不到工作负载的分支），不存在覆盖缺口。§21.2 按此修正。

验证（本机无集群，只到静态层）：`bash -n scenario.sh` 通过；`scenario.sh list` 列出 8 个场景、症状列与六类对齐；8 份 expect.json 过 `python3 -m json.tool`；8 份 manifest.yaml 过 yaml 解析（含 `---` 分隔的 PVC + Deployment 两份对象）。

待办：起集群后逐个 `scenario.sh up` 确认就绪（重点看 OOM 的匿名内存、liveness 场景 restartCount 能否在 180s 内到 2、readiness 场景 ready=False 的判定），然后进 `internal/k8s/lab_scenarios_test.go`（采集层断言）与 `cmd/k8seval` + eval.sh。

## 26. 第 3 批实现（四）：采集层断言 lab_scenarios_test.go（2026-09-15 完成）

新文件 `internal/k8s/lab_scenarios_test.go`（build tag `k8slab`，不进合并门槛）。形态与冒烟台不同：冒烟台验"采集链路能不能跑通"，这里按场景验"该拿到的证据拿到了没有"。

- 入口 `TestScenarios` 一张场景表，每场景一个子测试；集群连不上或该场景没起一律 Skip（未起的场景不该把整个 tag 判红）。
- 目标定位不写死 Pod 名：从 `expect.json` 读 selector（标签只维护一份，Go 侧不再抄），路径相对包目录上溯两级到仓库根——go test 的工作目录就是包目录。
- 三层断言（口径见 §21.4）：每个子测试先跑一条通用断言"`notes` 里没有 `required_failed`"，再跑该场景的关键证据断言。
- 八条关键证据断言里，真正有判别力的几处：CrashLoop 要求 previous 日志里有那行 fatal（根因几乎只在这里）；OOM 要求模板里的 memory limit 能读到（只看 Pod 上的值看不出是它配错）；ImagePull 断言报错原文含请求的镜像引用、且不含 `pull access denied`/`unauthorized`（后两者是"缺 secret"的分界），不去绑具体文案；两个 Pending 场景互斥断言（selector 场景不许出现 Insufficient）；PVC 场景断言 claim 的 phase 与 storageClass；liveness 场景断言"上次终止原因不是 OOMKilled"（137 同码的坑）；readiness 场景断言 Running + Ready=False + 零重启，且探针端口（8081）与 args 里的监听端口（8080）都能读到。
- 不断言"报错文案等于某个字符串"这类随 CRI/registry 变的东西；凡是环境相关的取值都降级成"字段在且形态对"。

顺带修正一处事实错误（上一轮写场景时留下的）：`imagepull-tag-missing` 的 manifest 注释与 README 原写"与冒烟台 message 不同（manifest unknown vs not found）"，实际冒烟台 `smoke.yaml` 的 `badimg`/`badweb` 用的就是同一个 `busybox:1.36-nope`，两者报错完全同源；且 containerd 下透传的是 `not found` 而非 `manifest unknown`。已改成"仓库存在、tag 不存在，报错原文含镜像引用"，并把与冒烟台的关系写成"分工不同、不是重复"。

验证：`gofmt -l internal/k8s/` 无输出；`go vet -tags k8slab ./internal/...` 与 `go vet ./...` 均通过。真集群断言待起集群后跑（`go test -tags k8slab ./internal/k8s/ -run TestScenarios -v`）。

### 26.1 发现的一个证据缺口（需拍板）

Pending 类的 Pod 没有 `node_name`（没被调度就不可能绑定节点），而 `Collect` 只在 `pod.Spec.NodeName` 非空时才取节点证据——于是两个 Pending 场景的证据包里 `Node` 恒为空。而 `k8s_node` 又要求显式给节点名（`k8s_node` 的 `node` 必填，取不到时没有"列全部节点"的退路）。后果：

- `pending-insufficient-resources`：模型拿不到 `free_on_node`/`allocatable`，"requests 超可分配"只能靠事件里那句 Insufficient 间接支撑，无法自己核数字（设计文档 §4.1 把分配账本说成"Pending 归因的核心数字"，实际到不了手上）。
- `pending-node-selector`：模型拿不到节点标签，无法证实"没有任何节点带 `lab-node-role`"，只能推断。

这是场景集跑到评测阶段才暴露的缺口（写场景时先看到的是 Pod spec 与事件，两样都齐）。三条路：

1. 给采集包加"列节点"能力（`Nodes()` 返回各节点摘要：名称/标签/taints/allocatable/free），工具层加 `k8s_nodes`（只读，`policy.Pass`，走新增工具的完整五步）。Pending 归因从此能自己核账。工作量约一个 400 行内的轮次。
2. 让 `k8s_node` 的 `node` 变可选：留空即列出全部节点摘要（不新增工具，但把两种形态塞进一个工具，参数语义变得含糊）。
3. 不动：Pending 场景的验收标准降为"能凭事件定症状与类别"，评测里的证据覆盖项相应放宽。

倾向第 1 条（语义最干净，且 Pending 占六类之一、首批 8 个场景里占 3 个）。等拍板后再动。

## 27. 第 3 批实现（五）：补 Pending 的节点侧证据面（2026-09-15 完成）

§26.1 的缺口按拍板走第 1 条：新增"列节点"能力 + `k8s_nodes` 工具。走新增工具的完整五步。

采集包 `internal/k8s`：

- `Nodes(ctx)`：列出全部节点的摘要（名称/Ready/conditions/allocatable/taints/标签 + 分配账本）。账本一次列全量 Pod 后在内存里按 `spec.nodeName` 分组算，避免逐节点 list（N 次全量扫描）；未调度的 Pod 不属于任何节点。列 Pod 失败只降级：节点自身的标签/taints/allocatable 照旧返回，每条视图的 `allocation_error` 写出原因（空余量不能被读成"没有占用"）。
- 抽出三个共用件，避免"单节点"与"列节点"两条路径漂移：`nodeView(n)`（摘要里与集群状态无关的部分）、`ledger(pods)`（非终态计数 + requests/limits 合计，原 `nodeAllocation` 的循环体）、`applyLedger(v, allocatable, …)`（账本入视图 + 算 free）。`Node()` 与 `Nodes()` 各自复用，行为不变（原单测 `TestNodeAllocationAndFree` 未改仍过）。
- 顺带修一个会卡死 nodeSelector 归因的问题：`pickNodeLabels` 原按"拓扑/机型"白名单裁节点标签，而 `nodeSelector`/`affinity` 可以引用任意自定义键（disktype、gpu、自定义域）——白名单一裁，"要求的标签在不在"永远无法证实。改为 `nodeLabels` 全量保留（节点标签通常十几个，裁掉的风险远大于省下的 token），并把这条写进设计文档 §4.3。

工具层（新增工具五步）：

1. `builtin/k8s.go` 新增 `K8sNodes(coll)` + `runK8sNodes`（无参数，故不与 `decode*` 一族同形；失败按业务失败回灌）；`k8s_node` 的 Description 补一句"Pod 还在 Pending 时没有 node_name，那种情况用 k8s_nodes"。
2. `register.go` 的 K8s 分支追加注册（八 → 九）。
3. `tool_permissions.go` 登记 `k8s_nodes: policy.Pass`。
4. 提示词两处：`main.go` 的 base 工具清单补 `k8s_nodes`（并说明 Pending 场景为什么必须用它）、workflow `k8s-diag.md` 第 4 步的 Pending 必查项把 `k8s_node` 换成"`k8s_nodes` 列全部节点"。
5. 验证：`internal/k8s/k8s_test.go` 新增 `TestNodes`（按名排序、按节点分组算账本、未调度 Pod 不计、列 Pod 失败时每条都带 `allocation_error`——这条同时锁住了"标签全量"的新行为，旧白名单下它会失败）；`register_test.go` 的注册断言补 `k8s_nodes`；`builtin/k8s_lab_test.go` 补真集群断言（回灌是 JSON 数组且含 `labels`/`free_on_node`）；`lab_scenarios_test.go` 的两个 Pending 场景改用 `Nodes()` 断言节点侧（资源不足场景要求 `free_on_node` 在，nodeSelector 场景要求没有任何节点带 `lab-node-role`）。

一处刻意没做：不把节点列表塞进 `Collect` 的证据包。多节点集群下它是 O(节点数) 的额外体积，而 Pending 场景本来就要模型自己深挖（workflow 第 4 步已写明）；工具按需取，省的是每一次非 Pending 诊断的常驻 token。

文档同步：`k8s-diagnosis.md` §4.1（`Nodes()`）、§4.3（节点标签不做白名单）、§5（工具表加 `k8s_nodes`，"第八个工具"改"第九个"，共九个）、§14 第 4/6 条；`CLAUDE.md` 补采集层断言的跑法、场景集"8 个已落地"。

验证：`gofmt -l` 无输出；`go build ./...`、`go vet ./...`、`go vet -tags k8slab ./internal/...` 通过；`go test ./... -count=1 -race` 全绿。真集群断言（`-run TestScenarios` 与 `-run K8sTools`）待起 minikube 后跑。

## 28. 第 3 批实现（六）：打分器 cmd/k8seval（2026-09-15 完成）

按 §21.5 的落点做独立 main 包 `cmd/k8seval`（只 import 标准库 + encoding/json）。为什么是这个形态：判分读的是产物契约文件（expect.json / report.json / evidence.json），复用 internal 的类型会把"实现漂移"变成"判分跟着漂移"——被测代码改字段名，判分器也悄悄跟着改，评测就失去独立基线。`noteRequired` 那个常量刻意写成字面量而不 import `internal/k8s`，同一个理由。

两个子命令：

- `score --expect … --report … --evidence … [--out <run-dir>]`：打一个场景的分，落 `<run-dir>/<场景名>.score.json`，stdout 一行。
- `summary --out <run-dir>`：扫该目录下 `*.score.json`，打印一张表并写 `summary.md`（每场景一行 + 平均分 + 单独点出"0 分场景"与"证据覆盖不足场景"）。

打分实现里的几处取舍：

- 五项的达成度各自算 0-1，再乘权重求和（症状 0.15 / 类别 0.35 / 证据 0.25 / 缺失 0.15 / 置信 0.10），一票否决最后裁决。
- 症状与证据关键词用"不区分大小写的子串命中"而非精确相等：模型写症状常带补充说明（`CrashLoopBackOff` 后跟括号），精确相等会把对的答案判错（§21.5 也明确证据覆盖用关键词判定）。
- 根因类别保持精确比对（词表已冻结）：0.35 的权重压在它上面，同义词归并会把"差不多"变成"对"。
- 置信度按"下限处 1、0 处 0"线性扣；>1 视为无效值先夹到 1。
- 一票否决只扫 `root_cause.summary` 与 category 值，不扫 `ruled_out`——那里是"已排除候选"的地盘（§21.3 的收紧）。代价写进了注释与用例：summary 里的否定句（"不是 OOMKilled"）仍会被算中，故 must_not_claim 的词一律写成结论式短语。
- 缺失声明用"条数比对"实现，而非逐条配对：唯一客观基准是证据包 notes（我们的文本），报告里是模型的自由表述，逐条配对必然要上一套同义词启发式；首批场景证据齐全（`required_failed=0`），该维度实际作用集中在"凭空报缺失"这一侧。这是对 §21.5 字面的粗化实现，已在 score.json 的 notes 与代码注释里写明；2026-09-15 拍板先用条数代理，等基线稳了再决定是否加严（加严＝补"来源关键词 ↔ 报告表述"的匹配表）。
- 报告缺失记 `status=no_report`（总分 0、无逐项分）：评测要能区分"诊断错"与"根本没产出"，混在一起会让基线失真。证据包缺失不算致命（按"无基准"处理并写明），因为它只影响缺失声明一项。

测试 `cmd/k8seval/main_test.go`：

- `TestScoreScenario` 九条用例逐项验分（全对 1.0、类别错 −0.35、证据半命中 0.875、置信度低 0.95、凭空报缺失 0.925、漏声明 0.9625、命中否决词 0、ruled_out 里的候选不否决、summary 否定句算命中）。
- `TestScoreScenarioSymptomPartial` 症状项按命中比例扣，且带括号补充说明仍算命中。
- `TestExpectFilesParse` 遍历 8 个真场景的 expect.json，断言"该有的字段都有值"——判分器与场景契约是两端，字段名漂移在打分侧是静默的（空列表会让比例项直接满分），这条把它变成硬失败。
- `TestRenderSummary` 三类结果（正常 / 0 分 / 证据覆盖不足）都要在汇总里看得见。
- `TestRunScoreNoReport` 走一遍 CLI（flag 名、落盘路径与文件名）断言 no_report 形态。

验证（本机无集群，纯函数 + 契约文件）：`gofmt -l` 无输出；`go vet ./...` 通过；`go test ./... -count=1 -race` 全绿；手工跑一次 CLI：给 OOM 场景喂一份理想报告得 `total=1.000`，喂缺失报告得 `no_report`，`summary` 出表与 `summary.md` 正常。

文档同步：设计文档 §12 第 3 批进度、§11（打分器落点原已写）；`CLAUDE.md` 目录树补 `cmd/` 一行与打分命令。

下一轮：`eval.sh`（起场景 → `scenario.sh target` 取目标 → 子进程驱动 `go run . --session eval-<run-id>-<name>` 跑 `/diag` → 定位产物目录 → 调 k8seval → 收场景 → 出总表）。

## 29. 第 3 批实现（七）：端到端驱动 eval.sh（2026-09-15 完成）

`Zoo/k8s-lab/eval.sh`，把 §21.5 的跑法串成一条命令：逐场景「起 → 跑 → 打分 → 收」，末尾出总表。

- 用法：`bash Zoo/k8s-lab/eval.sh [--timeout <秒>] [--keep] [场景名...]`；不给场景名就跑全部（从 `scenarios/*/expect.json` 枚举，与 `scenario.sh list` 同源）。
- 前置只检查不代劳：`kubectl cluster-info` 与 `DEEPSEEK_API_KEY` 存在性（密钥只做检查，不打印不落盘——组合根仍从环境变量读）。
- 一次编译、跑多个场景：CLI 与打分器各 `go build` 进临时目录（`mktemp -d` + trap 清理），不用 `go run`——每个场景重编一遍纯浪费；仓库里只留评测产物。
- 驱动方式就是 §21.5 写的：`printf '/diag <ns>/<pod>\nexit\n' | <bin> --session eval-<run-id>-<场景名>`，输出全进 `out/<run-id>/<场景>.cli.log`（排障要看对话原文），stdout 只留状态行。
- 产物定位走通配符 `$k8s_dir/<sid>/*.report.json`，不拼文件名——Pod 名带 ReplicaSet hash，拼不出来（这条与 scenario.sh 的 selector 定位同源）。`k8s_dir` 从 `~/.small/config.yml` 读（缺省 `~/.small/k8s`）：产物目录按会话 id 分支，写死路径会把"改过配置的人"变成静默的 no_report。脚本自己读这一项是必要的重复——组合根只认配置文件。
- 报告缺失不中断：不打 `--report` 就是 no_report（打分器已有的状态），脚本额外提示 cli.log 在哪。
- 场景没起来就跳过：不写 score.json，只在末尾的"未参与的场景"清单里点名——汇总表只覆盖真正跑过的场景，否则"没跑"会被读成"0 分"。
- `--keep` 保留场景对象，跑完不收（排查用）；默认每场景跑完立刻 down，避免场景之间互相干扰（同一个 diag-lab 命名空间）。
- `--timeout` 默认 900 秒，走 `timeout(1)` 包住 CLI（LLM 诊断可能长，但不能无限挂）；系统没有 `timeout` 时退化为直接跑。

验证（本机无密钥，故只到静态与纯函数这一层）：`bash -n` 通过；`k8s_dir()` 在"无配置文件/带波浪号与行尾注释/带引号/无该键"四种输入下都给出正确结果；`first_match()` 命中与空输入都正确；`--timeout` 缺值的参数校验报用法并退 2。真跑一轮需要 `DEEPSEEK_API_KEY`；不需要密钥的那半（场景就绪 + 采集层断言 + 工具层断言）见下一节。

文档同步：设计文档 §10 目录树（eval.sh 与 8 个场景标"已落地"）、§12 第 3 批进度；`CLAUDE.md` 命令清单补 eval.sh 用法。

## 30. 第 3 批验证（一）：真集群首轮（2026-09-15 完成）

环境：本机 minikube 一直在跑（v1.37.0，单节点 minikube，containerd，metrics-server 就绪且 `kubectl top` 有数）。这是场景集落地以来第一次拿真集群跑，不需要密钥的那半全跑通了。

跑法与结果：

1. 八个场景逐个 `scenario.sh up`：全部就绪，且就绪输出与预期形态一致——CrashLoop 的 `last=Error restarts=2`、OOM 的 `last=OOMKilled`（匿名内存那条做法被证实有效，第一枪就打出 OOMKilled 而不是 Error）、ImagePull 的 `reason=ImagePullBackOff`、三个 Pending 各自 `phase=Pending`、liveness 的 `last=Error restarts=2`、readiness 的 `phase=Running ready=False restarts=0`。
2. `go test -tags k8slab ./internal/k8s/ -run TestScenarios`：8/8 通过（首轮 7/8，见下面第 1 条）。
3. `smoke.sh up` + `smoke.sh test`：4/4 通过（首轮 1 红，见第 3 条）；`go test -tags k8slab ./internal/tool/builtin/ -run K8sTools`：3/3 通过。
4. 收尾 `smoke.sh down`（删 diag-lab，场景对象随之清空），合并门槛三门全绿。

真集群踩出来的三件事（都已修）：

1. 裸 Pod 的"工作负载取不到"被记成 `required_failed`。`imagepull-tag-missing` 是唯一的裸 Pod 场景，它的通用断言（notes 里无 required_failed）首轮就红：报错是"pod diag-lab/badtag 无 controller owner"。定性错了——裸 Pod 本来就没有上层工作负载这一层，Pod spec 里已有全部规格；记成缺失会让模型凭空写一条 missing_evidence 并压低置信度。改法：`Collect` 里先看 `pv.OwnerRefs` 有没有 controller owner，裸 Pod 记 `no_data`（"该 Pod 无 controller owner（裸 Pod），没有上层工作负载可读"），有 owner 却读不到才记 `required_failed`。这正是 Note 分级要解决的问题第一次在真实数据上显形（第 15 条 B15 是纸面设计，这里是实测）。测试拆成两面：`TestCollectDegraded`（有 owner、RS 不存在 → required，原样保留）与新增 `TestCollectDegradedBarePod`（清掉 ownerRefs → no_data）；设计文档 §4.1 的 no_data 行与下面那段说明同步。
2. crashloop 场景的两处断言语境依赖，首轮红：
   - 当前状态在 `CrashLoopBackOff` 与刚被杀的 `Error` 之间摆动（kubelet 的状态真的在切），只认前者会随采样时刻红绿；
   - minikube + containerd 下 previous 日志常常取不到——正文是 `unable to retrieve container logs for containerd://…`（而且它是 200 带回的正文，不是错误），那行 fatal 就在当前日志里。
   改法：断言放宽为"state_reason 是 CrashLoopBackOff 或 Error"+"两次日志里能看到 fatal"。顺带在 workflow 第 4 步补了一句提示（previous 取不到时当前日志里就是崩溃前的输出），场景 README 也写清了。这不算放宽标准：判别力仍在 lastState(Error/1) + 重启计数 + 崩溃输出三者上。
3. `TestLabImagePullBackOff` 把事件文案绑死在 "not found" 上，而这次 registry 抖动返回的是 `... failed to do request: Head "https://registry-1.docker.io/...": EOF`——报错类型变了（网络问题而非 404），断言跟着红。改法：只断言"kubelet 报了这次拉取失败"（`failed to pull image`，大小写不敏感），具体失败类型留给模型按 message 判。并在 `imagepull-tag-missing` 的 README 记下前置：registry 不可达时这条场景的前提不成立（模型判成"registry 不可达"是对的，不该记它错），评测前要先确认报错是 404 类。

值得记住的环境事实：本机到 Docker Hub 的网络会抖，`tag 不存在` 与 `网络不通` 在这套环境下会长得很像（都是 Warning + 拉取失败）；凡是断言 registry 报错文案的地方都别写死。

未做：`eval.sh` 的端到端一轮（缺 `DEEPSEEK_API_KEY`，需要真实 LLM 调用）。跑法已固化为一条命令，密钥就绪时可直接 `bash Zoo/k8s-lab/eval.sh`（先用 `--keep <单场景>` 小步验一次更省）。

## 31. 第 3 批实现（八）：症状→根因判别线索表（2026-09-15 完成）

§21.2 之后一直挂着的"症状→根因模式表补全"：词表（23 项）此前只在 `k8s_report` 的 schema description 里以"值=释义"的形式存在，模型知道有哪些类别、但不知道各类别长什么样——只能临场编候选再去套枚举。评测里 0.35 的权重压在类别上，这块补上比再加场景更值。

落点：`internal/workflow/workflows/k8s-diag.md` 的分支步骤。原第 3 步"按症状列候选根因"一句话扩成两步——第 3 步症状归类（并把判别顺序写死：事件 reason → lastState/exitCode → 日志与配置收口），第 4 步按六类症状逐条列出候选类别与判别线索（23 项全覆盖，含兜底 other 的用法）。写法上每条只给"这个类别长什么样"的可观测线索（如 `oom_limit_too_small`＝用量贴着 limit 被杀、重启前后是平线不是爬升；`imagepull_registry_unreachable`＝timeout/no such host/EOF），不写方法论。

一致性固化：新增 `TestCategoriesCoveredByPlaybook`（internal/k8s/report_test.go）——词表里每个类别都必须出现在该分支资产里。这条不放宽就会静默失效：加了类别却漏写进分支，枚举里有、线索里没有，模型永远选不到它，评测表现为"这个类别从不出分"，而单看词表或单看分支都发现不了。方向是词表 → 分支（词表是源，分支必须跟上）。

文档同步：设计文档 §6 步骤表（7 步 → 8 步，第 4 步写清"表在分支资产里 + 由测试固化"）、§7.2 关键证据清单加一句区分（那份是"该查什么"，判别线索是"该判成哪一类"）。

一处成本要留意：`RenderBranch` 是把每个分支的步骤正文整段渲染进 base 提示词的，所以这张表是常驻成本，不是"触发后才加载"。2026-09-15 拍板"压缩成判别词"并已压过一轮：每条只留可观测的判别词（如 `oom_limit_too_small`＝用量贴 limit、重启前后是平线），去掉解释性描述；分支资产 5.9KB → 5.4KB（净增 ~1.75KB ≈ 1.1k token/次请求，相对 32k 预算约 3.5%）。再往下压就要削判别力本身了，先这样跑，等评测基线出来再决定是否只保留高频易混项。

## 32. 第二批量场景（一）：三个新变体 + wait 条件修正（2026-09-15 完成）

变体矩阵里"其余变体按第二批量逐条补"，这轮补了 3 个能在这套环境里确定性复现的，另 3 个明确推迟并写清原因。

新增：

- `crashloop-missing-dependency`：容器去访问不存在的 Service（`db-svc`），DNS 解析失败（wget 原生报错 "bad address"）后 exit 1 → CrashLoopBackOff。与 `crashloop-app-error` 的分界全在日志内容上（一个败在依赖、一个败在自身）；不用"DB 认证失败"那类变体是因为要真起一个有认证的后端（得拉镜像），DNS 失败零外部依赖。
- `crashloop-config-missing-env`：脚本要求 `MODE` 而 spec 里一个 env 都没给 → 打印 "missing required env MODE" 后 exit 1。顺带验证 env 证据面（断言里要求 `Env` 为空，模型得看出"缺配置"而不是猜某个 configMap）。
- `restart-periodic-crash`：先正常服务 25 秒再非 0 退出，restartPolicy 反复拉起；spec 里没有任何探针。与 `restart-liveness-kill` 成对：都是"能跑但反复重启"，分界是"有没有探针 + 事件里有没有 probe 失败"。

推迟的三个（连同原因记在设计文档 §10）：OOM 堆未配需要一个带堆配置的运行时镜像（JVM/Node，体积大、本机 registry 会抖）；ImagePull 缺 secret 需要真的私有仓库（公开 Hub 上"仓库不存在"与"需鉴权"的返回不稳定，这次实测还出现过 EOF）；Pending taint 未容忍要给节点加 taint，而 `scenario.sh` 的生命周期只有 apply/delete 文件 + 按标签清理，没有 setup/teardown 钩子——先加钩子再谈场景，单列一轮。

真集群跑出来的两件事（都已修）：

1. 三个 CrashLoop 场景的就绪条件原写 `container_state_reason=CrashLoopBackOff`，第一次 `up` 就撞墙：新场景的容器在第 4 次重启后进入长退避，`kubectl get pod` 的 STATUS 一直是 Error、`state` 是 `terminated`（不是 waiting），180 秒等不到。回头把那 8 个场景里的 crashloop-app-error 一起改成"`container_last_state_reason=Error` + `container_restart_count_min=1`"：崩溃类场景的可靠信号是"上次终止 + 已经重启过"，当前状态落在退避周期的哪一段是采样运气（与 §30 里放宽断言是同一个道理，这次把就绪条件也对齐了）。
2. 容器里的 PID 1 对"没装处理函数"的信号是忽略的——`restart-periodic-crash` 原设计用 `kill -SEGV $$` 自杀，实测三种写法都不成立：`kill -11 $$` 与 `kill -s 11 $$` 在 busybox ash 下直接报 `sh: invalid number '$'`（语法不认），`kill -6 1` 发得出去但 PID 1 照常活着（内核不把默认动作的信号投给 init）；改用 SIGKILL 能杀掉，但退出码 137 会与 OOMKilled 撞车、反而污染判别。最终用普通非 0 退出码，并把"运行时长 ≥20s"（lastState 的 startedAt→finishedAt）作为断言，把"先服务再崩"与"启动即退"分开。这条经验记在场景 manifest 注释里。

验证（真集群，逐个 `scenario.sh up` + `go test -tags k8slab -run TestScenarios`）：四个崩溃类场景全部就绪（含新加的三个），断言 PASS；`gofmt` / `go vet`（含 `-tags k8slab`）/ `go test -race ./...` 全绿；收尾删掉 diag-lab。

文档同步：设计文档 §10（11 个已落地 + 推迟原因）、`CLAUDE.md` 场景数。

## 33. 第 3 批验证（二）：端到端第二轮与打分口径定稿（2026-09-15 完成）

第二轮完整跑（`bash Zoo/k8s-lab/eval.sh`，11 场景，产物 `Zoo/k8s-lab/out/20260915-200942/`）：症状、根因类别、证据覆盖、置信度四项 11/11 满分；均分 0.978，扣分全落在"缺失声明"一项，查证后确认是口径问题而非模型问题（见下第 3 条）。

本轮改了三处：

1. 探针类优先级落地（按 §22.4 拍板"优先判具体错配"）：`internal/k8s/report.go` 释义、分支资产判别线索表、`restart-liveness-kill` 的 expect.json 与 README。第二轮该场景类别判 `probe_port_wrong`、与 expect 一致 → 类别项满分，首轮那 0.65 分的根因消除。
2. 关键词归一化（`normText`：小写 + 去下划线/连字符/空格）：模型写 `last_state`、expect 写 `lastState` 的撞车不再误判；`TestEvidenceKeywordNormalization` 固化。
3. 缺失声明改为"只判漏报"。旧口径把证据包 notes 当成"模型能申报的全部缺口"，按条数算多报惩罚；实测证明这个假设不成立——证据包 notes 只覆盖采集器自己的来源读失败，而报告的 `missing_evidence` 是模型视角的证据缺口，天然可以超出这个集合。本轮模型申报的三条（"容器内实际监听端口清单（ss/netstat）""ResourceQuota 明细""previous 日志正文不可读"）都是工具集之外或环境抖动造成的真实缺口，且模型自己写明"不影响结论"，却被当成"凭空报缺失"扣到 `restart-liveness-kill` 只剩 0.50。终版口径：`required_failed` 漏声明按覆盖率扣分（这条抓的是"隐瞒环境降级"，真正要防的失效模式），多报不扣分、只在 score.json 的 notes 里留一条人工复核提示。扣多报等于训练"少说话"，与"降级必须可见"的设计原则直接相悖。

改后复评（同一批产物重新打分，不重跑集群与 LLM）：11/11、均分 1.000。产物只留每个批次最新的一轮：`20260915-200942`（11 场景，终版口径）与 `20260915-204524`（3 个新场景，见 §34）；更早的单场景试跑（`194610`/`195115`）、被第二轮取代的首轮（`195210`）与派生出来的复评目录（`200922`）都已删除。

测试同步：`cmd/k8seval/main_test.go` 的缺失声明用例重写（多报不扣分 1.0、两个必需源只报一个 0.925、必需源一条没报 0.85），并加 `TestScoreScenarioOverDeclareNote` 断言"多报要留下提示"。

门禁：`gofmt -l .` 无输出、`go vet ./...` 与 `go vet -tags k8slab ./...` 通过、`go test ./... -count=1 -race` 全绿。

口径同步：设计文档 §11、jjj §21.5 第 4 项、§22.4。

## 34. 第三批量场景（一）：原推迟的三个变体落地（2026-09-15 完成）

§32 推迟的三个变体全部落地（`oom-heap-misconfig`、`imagepull-missing-secret`、`pending-taint-not-tolerated`），
场景集 11 → 14。三处前置条件各有各的解法，先侦察后动手（下面每条都写清"实测到了什么"，不是推测）：

1. `pending-taint-not-tolerated`：`scenario.sh` 加可选钩子——场景目录里有 `setup.sh` / `teardown.sh` 就在
   apply 前跑 setup、清理后跑 teardown、`down-all` 对每个带 teardown 的场景都跑一遍（钩子必须幂等，
   down-all 会重复触发）。为什么必须由脚本做：taint 挂在节点对象上，写不进 `manifest.yaml`（那是命名空间内
   对象的清单）。风险与对策：单节点集群打上 NoSchedule 会封掉唯一可调度目标，漏摘会让后续场景全部 Pending，
   所以 teardown 摘完还要复查一遍并打警告，`down-all` 也把每个场景的 teardown 都跑一遍（不是只删命名空间内的对象）。
   顺带补了就绪条件 `event_reason`：Pending 的 Pod 一创建 phase 就是 Pending，只等它会在 FailedScheduling
   事件写出来之前就宣布"就绪"，而那条事件正是诊断要用的判据。
   两处实测偏差：API server 会给每个 Pod 注入 not-ready / unreachable 两条默认 NoExecute 容忍，
   断言不能写"tolerations 为空"（要写"没容忍本场景那把 taint"）；本版调度器的事件文案是
   `1 node(s) had untolerated taint(s)`，不列出 taint 键——"是哪把 taint"只能从节点侧证据看，
   判别线索表里也补了这句。
2. `oom-heap-misconfig`：主机 docker 里本来就有 `eclipse-temurin:17`，`minikube image load` 进节点后
   场景完全离线（tag 不是 latest，默认 IfNotPresent）。容器命令行写 `-Xmx256m -Xms256m
   -XX:MaxMetaspaceSize=64m -XX:+AlwaysPreTouch`、limit 128Mi：`AlwaysPreTouch` 是必需的，不加它
   JVM 只是"允许"用 256m，`-version` 这种不出内存的命令根本不会去碰堆，被杀时序复现不出来。
   实测启动即 OOMKilled（137），与 `oom-limit-too-small` 的分界是"堆是显式配过且配大的"。
3. `imagepull-missing-secret`：本地伪 registry 这条路走不通，实测两处硬伤——containerd 对 plain HTTP
   的 registry 按 HTTPS 处理（要改节点 containerd 配置加 insecure registry 并重启），而 busybox 1.36 的
   httpd 连 `-a` 认证参数都没有（认证改走配置文件的另一套语法）。改用 ghcr.io 上一个对匿名不可见的路径：
   实测匿名拉取返回 `failed to authorize: failed to fetch anonymous token: ... 403 Forbidden`，是干净的
   鉴权失败文案；Docker Hub 那条路被否是因为它把两种原因写在同一句里（`pull access denied, repository
   does not exist or may require authorization`），与 `imagepull_name_invalid` 撞车、期望类别不唯一。
   Pod 不写 imagePullSecrets、用 default SA（上面同样没有 secret），凭据两处来源都空才叫"缺 secret"。

端到端（真集群 + 真 LLM，`bash Zoo/k8s-lab/eval.sh <三个场景>`，产物 `Zoo/k8s-lab/out/20260915-204524/`）：
三项全部 1.000（症状/类别/证据/缺失声明/置信度五项满分），首轮即过。模型在 taint 场景的推理正是设计意图
（"唯一节点带 diag-lab-scenario:NoSchedule，而 Pod 只带默认的 not-ready/unreachable 容忍，未容忍该 taint"，
并且它自己注意到了默认容忍这层，说明判别线索足够）。

本轮改动：`scenario.sh`（钩子 + `event_reason`）、三个场景目录、`lab_scenarios_test.go`（三个断言 + 两处
实测修正）、判别线索表补 taint 那句、`CLAUDE.md`（场景数、down-all 语义、镜像准备一行）、设计文档 §10/§12。

门禁：`gofmt -l .` 无输出、`go vet`（含 `-tags k8slab`）通过、`go test ./... -count=1 -race` 全绿。

仍未收的变体（连同原因）：OOM 内存泄漏要有真实随时间增长的用量（得让容器真的漏，或用一个可控的
分配器脚本伪造曲线——后者语义假，与 §32 否掉 busybox 模拟堆配置同理）；ProbeFailed 的 initialDelay
分支与 `probe_timing_too_short` 现有场景的形态差异太小（都靠"启动耗时 > 判定门槛"这一个信号），
收了也是同一类判据考两遍。这两条要收得先想清"用什么证据把两者分开"。

## 35. k8s-diag 模块审计（2026-09-15）：文档漂移修正 + 只读守卫测试

用户问"这个模块正确实现了吗"，按"文档声称 → 代码事实 → 测试/实测是否固化"三层核一遍。三项并行审计
（只读性 / 依赖与工具契约 / 文档对账），结论分三类：

1. 核心链路正确且有固化：只读（全仓写 verb 在 k8s 链路零命中，采集只发 Get/List/GetLogs-Stream）；
   依赖方向（internal/k8s 无任何 internal 依赖，builtin 只 import 五个叶子部件且由 imports_test 固化）；
   9 个工具 + 全 Pass + 注册期 fail-fast；NoteKind 四值语义（含"裸 Pod=no_data / 有 owner=required_failed"）
   真的成立；词表 23 项与"每个类别都要在分支资产里有线索"由测试锁住；端到端 14 场景全 1.000。
2. 文档比代码多说的 6 处（已改，见下）；代码有而文档没写的一批字段（qos_class、service_account、
   init_containers、env_from、last_state*、metrics 的限值明文、node 的 ready/kubelet_version、
   PVC 的 events_error、事件的 source/object、Report md 的 Target/类别/Alternatives 三处等）——
   文档号称"唯一技术索引"，这些证据面漏列，但暂不逐条补（价值低于维护成本，将来按需补）。
3. 文档声称但代码没有强制：§7.2 两条置信度规则只在提示词里（代码只校验 0–1 与 evidence≥1）；
   §4.1"任一来源失败不中断"有一处例外（Pod 自身失败是硬失败，合理的例外但文档没写）；
   SA 读失败与 PVC 事件失败只落视图字段、不进 notes（因此也不进评测的缺失声明基准）。

本轮落地（按用户点单的两件）：

- 文档 6 处漂移：§4.2 日志行（API 参数实际只有 Container/Previous/TailLines，字节上限在客户端：读满
  1MiB 后按尾部裁到 64KB）、§4.2 节点 labels（"白名单"是残留，§4.3 已改全量，两处矛盾）、§4.3 注解
  （实际是 6 键白名单，比"只丢大字段"严）、§4.3 事件去重键（实际是 type+reason+message）、§4.3 日志
  限额（TailLines 服务端截 + 客户端裁的原因写清）、§7.3（评测侧不共享词表，`cmd/k8seval` 刻意不 import
  internal）；顺手修 `view.go` 里漏了 type 的注释。
- 只读守卫测试 `internal/k8s/k8s_test.go:TestReadOnlyGuard`：给两个 fake clientset 各装记录型 reactor，
  跑完整 Collect + Nodes()，断言实际 verb 只能是 get/list；并反向自检"记录器确实收到了对
  pods/events/nodes/PVC/SA/RS/Deployment 与 pods/log 的读"（防假绿）。为什么不是源码扫描：验行为比验
  文本难绕过；覆盖不了的（裸 HTTP、没走到的代码）在注释里写明由静态审计兜。
  负向验证：临时在 `Pod()` 里插一句 `Pods(ns).Delete(...)`，测试立刻红（报 "非只读动作 delete"），
  验证后已还原（grep 确认无残留）。
- 审计发现的三个残余风险进设计文档 §15 待决项（日志流共享 30s 预算、List 无 Limit/分页、Collect 无总
  deadline），置信度规则是否升级为硬校验列为待拍板；§8 补一段说明守卫测试与"只读靠代码不靠 RBAC"的边界。

门禁：gofmt 干净、`go vet`（含 `-tags k8slab`）通过、`go test ./... -count=1 -race` 全绿。

### 35.1 RBAC 最小集确认（同日，真集群实测）

用户要确认生产接入的 RBAC 最小集。先把代码里 15 处 client 调用（`c.core.` / `c.metrics.`）逐个列出对到 verb，
发现文档 §8 漏了一项：`nodes` 还需要 `list`——`Nodes()`（read.go:361）走的是 `Nodes().List`，`k8s_nodes` 与
Pending 归因都依赖它，只给 `nodes get` 会让"列全部节点"直接 403。已补。

修正后的集合（精确到 verb）：core 的 `pods` get+list、`pods/log` get、`events` list、`nodes` get+list、
`serviceaccounts` get、`persistentvolumeclaims` get；apps 四个工作负载 get；batch `jobs` get；metrics.k8s.io
的 `pods` get 与 `nodes` get。三处 list 的边界：只有"跨命名空间列 Pod（节点账本）""列节点""列事件"用到，
其余一律 get。

真集群验证（minikube，SA `small-k8s-diag` + 文档里那份 ClusterRole/Binding，token 走 `create token`，
临时 kubeconfig 落 /tmp，验证完即删）：代码用到的端点全部 OK——get pods 按名、list pods 集群范围（`-A`）、
get pods/log、list events、get node、list nodes、get sa、get deploy/rs/ds 按名、`--raw` 读
`/apis/metrics.k8s.io/v1beta1/.../pods/<pod>` 与 `.../nodes/<node>`；无现存对象可试的 PVC/statefulsets/jobs
用 `auth can-i` 查为 yes。反向验证最小集没放宽：`secrets` get 与 `pods` delete（server dry-run）都返回
Forbidden。

"只给命名空间级 Role 会让 `free_on_node` 整块降级"这条没单独造第二套凭据实测，按 RBAC 语义（跨命名空间
list 必须集群范围授权）推得，与判分口径无关。凭据与 CA 只落 /tmp，不进仓库（红线）；集群对象与 /tmp 文件
验证后已清理。

## 36. 置信度规则：代码判得了的那半条落成硬校验（2026-09-15 完成）

用户拍板"硬校验 = 拒绝并回灌，不静默改值"。落地前先拿 14 份已归档的满分报告量了一遍规则强度，避免
立一条会误杀正确报告的校验：

| 候选规则 | 实测（14 份报告） | 结论 |
|---|---|---|
| 有 `missing_evidence` 就压 ≤0.79 | 13 份命中（缺 1–3 条、置信度 0.87–0.95） | 不能立：这些缺口模型自己标注"不影响结论"且报告满分，硬套等于把如实申报当错误 |
| conf ≥0.80 需证据 ≥2 条且来源去重 ≥2 | 0 份命中（证据 4–7 条、来源 2–5 种） | 可立：不误杀，正好拦住"单条单源却报高置信度"这种虚高 |
| 证据里禁"可能/也许/大概" | 0 份命中 | 能立但不在本次范围（属 §7.2 第 5 条表述规则，不是置信度规则），先不动 |

落地内容：

- `internal/tool/builtin/k8s.go`：新增 `checkConfidenceRules`（+ 常量 `highConfidenceMin = 0.80`），在
  `runK8sReport` 里解码之后、落盘之前调用；不通过就 `k8sFail` 回灌（业务失败），错误信息写清"证据几条、
  几个来源、该怎么改（补证据或降到 0.80 以下并说明缺什么）"。`decodeReportArgs` 的注释同步改成"只校结构
  与取值域"，规则校验分到另一个函数——两类回灌各自说清。
- 工具 Description 与分支资产第 7 步各补一句"≥0.80 会被工具校验、不满足打回"，让模型提交前就知道，
  少浪费一轮。代价要记一笔：分支资产是常驻提示词，这句是净增 token（约 60 字符）。
- 测试 `internal/tool/builtin/k8s_test.go`：新增 `TestK8sReportConfidenceRule`（单条被打回、两条同源被打回、
  低置信度不触发、两条独立来源可过校验），并把 `validReportArgs` 夹具从"1 条证据"改成"2 条 / 2 个来源"
  （旧夹具本身就是这条规则要打回的形态）。
- 设计文档 §7.2 补一段写清"五条里哪条落了代码、其余为什么判不了"；§15 那条待决项改成"已落一半 + 剩下的
  卡在'决定性证据'无法机器判定"。

另一半为什么不硬做（写进代码注释，免得后人再问）：决定性证据要读懂证据内容；"关键证据缺失"要先知道
哪个算关键。两件都依赖语义判断，代码化就要引入正则/词表启发式，代价是把正确报告也拒掉——比不判更糟。

门禁：gofmt 干净、`go vet`（含 `-tags k8slab`）通过、`go test ./... -count=1 -race` 全绿；另跑一个场景
的真链路（`eval.sh restart-liveness-kill`）确认 k8s_report 落盘路径未被这次改动碰坏。

## 37. 日志读拆出独立超时预算（2026-09-15 完成）

审计发现的第二条风险落地（用户点名做这条，另两条 List Limit / Collect 总 deadline 仍不做）。

问题：日志读与元数据调用共用 30s 预算。日志是流式大对象（最多 1MiB 正文，还可能在服务端攒正文），
共用预算会把"读得慢"判成读失败 → 记 `required_failed` → 进缺失声明的客观基准、压置信度——
等于把环境慢当成证据缺。场景集日志都很小，所以一直没暴露。

改动（简单版，按用户拍板的"先做简单版"）：

- `internal/k8s/k8s.go`：新增 `logCallTimeout = 90 * time.Second`（数值是拍的，注释里写明理由与
  "为什么不共用"）；`callCtx` 拆出 `callCtxFor(ctx, d)`，`Logs` 走 `callCtxFor(ctx, logCallTimeout)`。
  回退语义写严：传 0/负值按"配置里的 Timeout → 包内默认"逐级回落——零预算等于 ctx 一出生就过期，
  那种失败最难查（看着像"API 挂了"），不留这个口子。
- 测试 `TestCallCtxBudgets`：锁预算接线（元数据用 cfg.Timeout、显式传值按传入、零值逐级回落、
  日志预算必须比元数据宽）。为什么不做行为级断言：deadline 挂在 ctx 上，fake clientset 是 in-process
  调用、不认 ctx 取消（`Stream` 也只是把 reactor 结果包成响应），行为级测不出来——这点写进测试注释，
  真实传输语义仍靠真集群冒烟。
- 真集群验证：`scenario.sh up crashloop-app-error` + `-tags k8slab -run TestScenarios/crashloop-app-error`
  通过（该断言本身要读日志正文），跑完已收场景。

文档：§4.4 补一段"超时预算分两档"及其理由；§15 那条待决项改成"已拆独立预算，真正的空闲超时仍未做"
（空闲超时要在流上包一层按读重置的定时器，等真遇到'日志流卡住'再说）；§15 里 Collect 总 deadline
那条同步标注日志为 ≤90s。

门禁：gofmt 干净、`go vet`（含 `-tags k8slab`）通过、`go test ./... -count=1 -race` 全绿。

收尾决定（同日，用户拍板）：加固到此为止——判断继续投入趋于过度设计，"先跑起来"优先。§15 剩余的
待决项（`List` 加 Limit、`Collect` 总 deadline、日志真空闲超时、§7.2 禁词校验、RBAC 清单落成仓库文件）
保持搁置，不再主动推进；要做时按需单独立项。







