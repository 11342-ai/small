# K8s 概念梳理（写给初学者，结合我们 k8s-diag 项目）

> 用途：概念学习与排障对照。放在 Zoo/temp/ 作草稿，需要的话再收进 Zoo/model/。
> 面向：会用一点 kubectl，但说不清"对象、控制器、现象层"区别的人。
> 本文所有命令在 minikube 上可直接跑（我们冒烟台用的就是它）。

## 0. 先说明：我选了哪些点，为什么选

不按名词表平铺，而是先立 10 个锚点，再用一张归位表把你的 13 组名词收进去。理由：k8s 的名词有 100 多个，平铺记不住也用不上；抓住"对象模型 + 三层平面 + 五个排障入口"之后，其余名词都能挂上去。

选点原则（三条）：
1. 与我们项目直接相关：诊断模块天天碰 Pod 状态、事件、日志、指标、节点、工作负载——这六个必须是深水区。
2. 是认知骨架：声明式对象模型、控制面、kubelet 与运行时、调度、网络、存储——不知道这些，排障只能靠猜。
3. 是高频入口：配置与安全、可观测、发布运维——出事时最先被怀疑的三块。

十个锚点：
1. API 对象模型与声明式/期望态（kind、resource、ownerReferences、控制循环）
2. 控制面（kube-apiserver、etcd、kube-scheduler、kube-controller-manager、cloud-controller-manager）
3. kubelet 与容器运行时（CRI、OCI、containerd、runc、cgroup、Linux Namespace、Pod Sandbox）
4. Pod 生命周期与状态机（phase、containerStatuses、conditions、探针、重启策略）
5. 工作负载对象（Deployment、ReplicaSet、StatefulSet、DaemonSet、Job、CronJob、HPA、VPA、PDB）
6. 调度（requests/limits、QoS、taint/toleration、affinity、TopologySpread、驱逐与抢占）
7. 网络（Pod IP、CNI、Service、EndpointSlice、CoreDNS、Ingress、Gateway API、NetworkPolicy、Overlay/VXLAN/BGP）
8. 存储（Volume 类型、PV、PVC、StorageClass、CSI、动态供给、StatefulSet 的卷）
9. 配置与安全（ConfigMap、Secret、ServiceAccount、RBAC、PSA、Gatekeeper/Kyverno、Admission Webhook、TLS/mTLS）
10. 可观测与发布（metrics-server、Prometheus/Grafana/Loki/ELK/Jaeger/OTel；滚动更新/回滚/蓝绿/金丝雀；Helm/Kustomize/GitOps/Argo CD/Flux）

## 1. 先回答你直接问的四个边界问题

### 1.1 event 是什么

namespace 级的 API 对象（kind=Event），记录"某个对象上发生了什么事"。由 k8s 组件（kubelet、scheduler、控制器）写入，也有应用自定义事件。

关键字段：`involvedObject`（指向对象，带 kind/name/uid）、`reason`（机器可读原因码，如 BackOff / FailedScheduling / Unhealthy / Failed）、`message`（人读描述）、`type`（Normal / Warning）、`count`（同一原因的重复次数）、`firstTimestamp`/`lastTimestamp`。

关键特性：它是"对象"，存在 etcd 里，有 TTL（kube-apiserver `--event-ttl` 默认 1 小时），过期被回收——这就是我们冒烟台踩过的坑：健康 Pod 跑了 8 小时，事件全被清空了。

```bash
kubectl -n diag-lab get events --field-selector involvedObject.name=badimg
kubectl -n diag-lab get events --field-selector type=Warning -o wide
```

### 1.2 log 是什么

容器 stdout/stderr 的字节流，不是 API 对象。由容器运行时（containerd）写到节点磁盘，kubelet 提供读取，客户端通过 API 的 `pods/log` 子资源代理拿到。

关键特性：只有"启动过"的容器才有日志（镜像都没拉下来时没有）；`previous=true` 读上一次已退出实例的日志（CrashLoop 的现场在这里）；可以按行数、字节上限、时间戳拉取。

```bash
kubectl -n diag-lab logs badimg                    # 容器没起来 → 报 waiting to start
kubectl -n diag-lab logs badimg --previous          # 上一次崩溃的现场
kubectl -n diag-lab logs badimg --tail=50 --timestamps
```

### 1.3 event 和 log 的区别（六个维度）

| 维度 | event | log |
|---|---|---|
| 本质 | API 对象（结构化事实） | 字节流（进程叙述） |
| 存哪 | etcd，有 TTL（默认 1 小时） | 节点磁盘（容器运行时日志），随容器重建/驱逐消失 |
| 谁产生 | k8s 组件（kubelet/scheduler/控制器），也可应用自定义 | 应用进程自己 |
| 取数方式 | List + fieldSelector（按 involvedObject 过滤） | GetLogs 流式读（TailLines/Previous/LimitBytes） |
| 回答什么 | k8s 对它做了什么、失败在哪一步 | 进程内部发生了什么、为什么退出 |
| 结构 | 有 reason 码，可程序化判断 | 无结构，可能要正则/模型理解 |

一句话记法：event 是系统视角的事件流，log 是进程视角的输出流。两者的交集最有价值——event 给结论（`reason=OOMKilled`），log 给证据（`java.lang.OutOfMemoryError: Java heap space`）。只信一个都会误判。

### 1.4 metrics 是什么，node 算不算资源

metrics 是资源用量的瞬时采样（CPU、内存），数据源是 kubelet 侧的 cAdvisor，经 metrics-server 聚合后以 `metrics.k8s.io`（v1beta1 的 PodMetrics / NodeMetrics）暴露。

能力边界（很容易被高估）：
- 只有"当前"，没有历史（要看趋势必须上 Prometheus）
- 只给用量，不给 limit（limit 得从 Pod spec 里取，比例要自己算）
- 依赖聚合链路（metrics-server 没就绪就 503，我们冒烟台撞过）
- 有采样间隔（默认约 15 秒）和"刚起的容器还没数"

node 这个词有两层含义，分开看就清楚了：
- Node 对象：集群的成员（kind=Node），由 kubelet 注册并上报状态，含 `status.conditions`（Ready/MemoryPressure/DiskPressure/PIDPressure）、`status.allocatable`/`capacity`、`spec.taints`。
- 节点资源（resources）：CPU/内存/临时存储/GPU 这些"可分配量"，是 Node.status 的一个字段（allocatable），不是独立对象。

所以答案是：Node 是对象，allocatable/capacity 是它身上的资源账本。调度就是把 Pod 的 requests 与节点 allocatable 做匹配，再叠 taint/affinity 等约束。

```bash
kubectl top pod -n diag-lab          # 用量（来自 metrics-server）
kubectl describe node minikube | sed -n '/Allocatable/,/Events/p'   # 资源账本
kubectl get node minikube -o jsonpath='{.status.allocatable}{"\n"}'
```

### 1.5 术语澄清：uid、name、nodeName 别混

这几个字段经常被误当成同一类东西，但它们回答的是不同问题：

- `metadata.name`：人类起的名字。同一 namespace 内同类资源唯一，但可复用——Pod 删掉重建，还能叫同一个名字。
- `metadata.uid`：kube-apiserver 在创建对象时分配的全局唯一 ID（UUID 形式），永不复用。它标识"这一个实例"，删了重建就是新 uid。
- `metadata.namespace`：它归属哪个命名空间。
- `spec.nodeName`：这个 Pod 被绑定到哪台 Node（调度器写入的结果）。指定"去哪个节点"用的是它，或者调度约束 nodeSelector / affinity——都不是 uid 的用途。
- `metadata.resourceVersion` / `generation`：版本号（并发控制、watch 用）与 spec 版本计数，也不是身份。

谁在用 uid：`Event.involvedObject.uid`（这条事件讲的是哪个对象实例）与 `ownerReferences[].uid`（父对象是谁）——引用靠 uid 才精确。Node 自己也有 uid，那是 Node 对象的身份，和"Pod 运行在哪"是两回事。

一条命令并列看三者（非破坏性，随时可跑）：

```bash
kubectl get pods -A -o custom-columns=NAME:.metadata.name,UUID:.metadata.uid,NODE:.spec.nodeName
```

为什么我们按 uid 过滤事件而不是按 name：Pod 删除重建后 name 相同、uid 不同；按 name 过滤会把上一代 Pod 的旧事件（还在 1 小时 TTL 内）一起捞进来，模型就会把"上一代的失败"当成"这一代的证据"。

## 2. 十个锚点

每个锚点四段：定义 / 职责 / 联系 / 学习建议。

### 2.1 API 对象模型与声明式模型

定义：一切皆对象（Pod、Node、Event 都是），每个对象有 apiVersion + kind + metadata + spec + status。spec 是你写的期望态，status 是系统回报的实际态。
职责：把"我要什么"与"现在是什么"解耦。控制器循环读 spec、改现实、写 status。
联系：`metadata.ownerReferences` 串起层级（Pod → ReplicaSet → Deployment）；`spec.nodeName` 指向节点；事件用 `involvedObject` 指回对象。
学习建议：先记 GVK（group/version/kind）与 resource（`pods`）的区别：kind 是"这是什么"，resource 是"路径上怎么访问"。练：`kubectl api-resources`、`kubectl explain pod.spec.containers`。

### 2.2 控制面

定义：kube-apiserver（唯一入口 + 认证授权准入）、etcd（唯一持久存储）、kube-scheduler（决定 Pod 去哪个节点）、kube-controller-manager（跑各种控制循环），云上还有 cloud-controller-manager（LB/路由对接）。
职责：apiserver 是"数据库 + 网关"，其余组件都是它的客户端；所有写操作最终落到 etcd。
联系：kubectl/client-go 也只是一个 apiserver 客户端——我们的采集包走的就是这条路；scheduler 只写 `spec.nodeName`（绑定），真正拉起容器的是节点上的 kubelet。
学习建议：先理解"没有 apiserver 就什么都做不了"。练：`kubectl -n kube-system get pods`，`kubectl get --raw /healthz`。

### 2.3 kubelet 与容器运行时

定义：kubelet 是节点上的代理，认领绑定到本节点的 Pod，调用 CRI 拉起容器；CRI 是 kubelet 与运行时之间的接口；运行时（containerd/CRI-O）再通过 OCI 标准调 runc 真正起进程。
职责：把 Pod spec 变成真实容器、探针、卷挂载、状态上报、日志提供。
联系：`containerStatuses`（含 lastState / restartCount）、事件里的拉镜像与探针记录、日志都出自 kubelet 这条链路；cgroup 管资源限制，Linux Namespace 管隔离，Pod Sandbox 是同 Pod 内容器共享网络的基础。
学习建议：动手 `minikube ssh` 看 `/var/log/pods`、`crictl ps`、`cat /sys/fs/cgroup/.../memory.max`，比看书有效。

### 2.4 Pod 生命周期与状态机

定义：Pod 是最小可调度单元；phase（Pending/Running/Succeeded/Failed/Unknown）只是粗粒度结论，真正的细节在 `status.containerStatuses[].state`（running/waiting/terminated）与 `lastState`。
职责：承载"实际态"。CrashLoopBackOff、ImagePullBackOff、OOMKilled 全是这一层的字段与原因码。
联系：`conditions`（PodScheduled/Initialized/ContainersReady/Ready）由 kubelet 与调度器共同维护；探针失败会写事件、按需重启容器；restartPolicy 决定重启行为。
学习建议：把六类症状与字段位置背下来（waiting.reason：CrashLoopBackOff/ImagePullBackOff/ErrImagePull；lastState.terminated.reason：OOMKilled/Error）。练：`kubectl get pod -o jsonpath='{.status.containerStatuses[*].lastState}'`。

### 2.5 工作负载对象

定义：替你管 Pod 的控制器对象。Deployment（无状态、滚动发布）、StatefulSet（有序、稳定标识与独立卷）、DaemonSet（每节点一个）、Job/CronJob（跑完即止/定时）、HPA/VPA（自动扩缩）、PDB（自愿中断预算）。
职责：维护期望副本数与发布过程，Pod 挂了就重建，这就是"Pod 是易失的"的由来。
联系：Deployment → ReplicaSet → Pod 三级；StatefulSet 的 Pod 名有序且绑 PVC；我们的 k8s_workload 工具就是沿 ownerReferences 上溯到这一层取模板规格（limits/probes/replicas/strategy/conditions）。
学习建议：先理解"改了 Deployment 的模板并不直接改现有 Pod"，要等滚动更新。练：`kubectl rollout status`、`kubectl rollout history`。

### 2.6 调度

定义：scheduler 依据 requests（不是 limit）做资源匹配，再叠约束：nodeSelector、affinity/anti-affinity、taint/toleration、TopologySpread、镜像本地性等。
职责：把 Pod 放到合适节点；放不下就留在 Pending 并写 FailedScheduling 事件。
联系：QoS 等级（Guaranteed/Burstable/BestEffort）由 requests 与 limits 是否相等决定，影响被驱逐的先后；抢占（preemption）会驱逐低优先级 Pod；驱逐（eviction）由 kubelet 在节点压力下执行。
学习建议：Pending 排障就三问——事件说什么、节点余量够不够、约束是否满足。练：给 Pod 写一个超大的 requests，看 FailedScheduling 事件。

### 2.7 网络

定义：每个 Pod 一个 IP（由 CNI 插件分配，平坦网络，Pod 间直连）；Service 给一组 Pod 一个稳定的虚拟 IP（ClusterIP/NodePort/LoadBalancer/ExternalName/Headless）；kube-proxy（iptables/IPVS 或 eBPF）实现转发；CoreDNS 提供 `svc.ns.svc.cluster.local` 解析。
职责：让"易失的 Pod"对外有稳定入口，让服务发现不依赖 Pod IP。
联系：Service 的后端由 EndpointSlice 维护（随 Pod 就绪动态更新）；Ingress/Gateway API 是七层入口；NetworkPolicy 控制东西向流量；Overlay（VXLAN/Geneve）与 Underlay（BGP）是两种实现路线。
学习建议：先分清"Pod IP 会变、Service VIP 不变、DNS 名不变"。练：`kubectl get endpointslice`、`kubectl run tmp --rm -it --image=busybox -- nslookup kubernetes.default`。

### 2.8 存储

定义：Volume 是 Pod 级挂载点，类型很多（emptyDir、hostPath、configMap/secret、PVC 等）；PV 是集群里的存储资源，PVC 是使用申请，StorageClass 定义"怎么动态供给"，CSI 是插件标准。
职责：把"数据不能随 Pod 消失"这件事落地：StatefulSet + PVC 是常见组合。
联系：PVC 绑定 PV（或由 StorageClass 动态创建）；Pod 引用 PVC 才真正挂载；PVC 未绑定会直接导致 Pod Pending（我们 Pending 场景的一个变体）。
学习建议：先记"emptyDir 随 Pod 消失，PVC 不随"。练：`kubectl get pvc,pv`，把 storageClassName 写错看 Pending。

### 2.9 配置与安全

定义：ConfigMap/Secret 注入配置与密钥；ServiceAccount 给 Pod 一个身份；RBAC（Role/ClusterRole + Binding）决定这个身份能做什么；PSA（Pod Security Admission）与 Gatekeeper/Kyverno（策略引擎）在准入阶段拦截不合规 Pod；NetworkPolicy 控制流量；TLS/mTLS 管传输加密与双向认证。
职责：把"最小权限"落实到对象层：身份 → 权限 → 准入 → 流量。
联系：Admission Webhook 是准入扩展点（Gatekeeper/OAM 都挂这里）；Secret 只是 base64，不等于加密，落盘与 etcd 加密要单独配置。
学习建议：先懂"ServiceAccount 是身份，RBAC 是权限，两者缺一不可"。练：`kubectl auth can-i list pods --as=system:serviceaccount:default:default`。

### 2.10 可观测与发布

定义：可观测三层——指标（metrics-server 即时用量 / Prometheus 长期时序）、日志（Loki/ELK 聚合）、链路（Jaeger/OpenTelemetry）；发布方式——滚动更新、回滚、蓝绿、金丝雀；工具链——Helm/Kustomize 管模板，GitOps（Argo CD/Flux）管部署状态。
职责：让"变更"可控可回滚，让"故障"可观测可归因。
联系：事件与日志是一线证据，指标是量化证据，链路是跨服务证据；发布动作会直接产生 Probe Failed / CrashLoop 这类症状，所以排障时要先问"最近发过版吗"。
学习建议：先把 `kubectl rollout undo`、`kubectl diff`、`helm history` 用熟，再谈平台的监控栈。

## 3. 整体知识关系图（文字版）

```
                      ┌──────────────── 控制面（期望态的裁判） ────────────────┐
   kubectl / client-go │  kube-apiserver ── etcd（唯一存储）                    │
        │              │        ▲   ▲                                          │
        └───REST───────┼────────┘   └── kube-controller-manager（控制循环）      │
                       │            └── kube-scheduler（绑定 nodeName）          │
                       └──────────────────────────┬────────────────────────────┘
                                                  │ watch/下发
                       ┌──────────────── 数据面（实际态的执行） ────────────────┐
                       │  Node                                          │
                       │   └─ kubelet ── CRI ── containerd ── OCI ── runc │
                       │        ├─ cgroup（资源限制） Namespace（隔离）    │
                       │        ├─ CNI（给 Pod 分配 IP）                  │
                       │        └─ CSI（挂载卷）                          │
                       │   └─ kube-proxy（Service 转发）                  │
                       └──────────────────────────────────────────────────┘

   对象层的两条正交关系：
     Pod.spec.nodeName ──────────► Node（在哪台机器上）
     Pod.ownerReferences ─► ReplicaSet ─► Deployment（归哪个工作负载管）

   现象层（不在对象树里，挂在对象上、随时间消失）：
     events   ── 系统视角：谁对它做了什么（有 TTL，1 小时）
     logs     ── 进程视角：它自己喊了什么（在节点磁盘）
     metrics  ── 用量采样：它吃了多少（当前值，无历史）

   排障时的数据流：
     症状（Pod status）→ events → logs（current→previous）→ metrics → node → workload
```

## 4. 从入门到实战的学习路线

阶段一 会用（1 周）：Pod/Deployment/Service/ConfigMap 四个对象 + `kubectl get/describe/logs/exec/events`。
最小实践：起一个 Deployment，暴露 Service，改 ConfigMap 看滚动更新。

```yaml
apiVersion: apps/v1
kind: Deployment
metadata: {name: web}
spec:
  replicas: 2
  selector: {matchLabels: {app: web}}
  template:
    metadata: {labels: {app: web}}
    spec:
      containers:
      - name: web
        image: nginx:1.27
        resources: {requests: {cpu: 100m, memory: 64Mi}, limits: {memory: 128Mi}}
```

阶段二 懂原理（2-3 周）：控制面四件套、调度约束、探针与生命周期、Service/EndpointSlice/DNS、Volume/PVC。
最小实践：故意制造 Pending（超大 requests）、ImagePullBackOff（错 tag）、Probe Failed（错路径），各看一遍事件与状态字段。

阶段三 能排障（持续）：把"症状 → 事件 → 日志 → 指标 → 节点 → 工作负载"这条链走顺，六类症状各练一遍。
最小实践：用我们 `Zoo/k8s-lab` 的场景跑 `smoke.sh up`，然后自己看 `kubectl -n diag-lab describe pod badimg` 与 events。

阶段四 能设计（长期）：QoS 与 PDB、HPA、NetworkPolicy、StorageClass、GitOps 与可观测栈。
最小实践：给一个服务加 HPA + PDB，压测看扩缩与驱逐行为。

## 5. 三个常见面试题 / 排障问题

1. 一个 Pod 一直 Pending，你怎么定位？
   答：先看事件（`FailedScheduling` 的 message 直接说原因），再分四类核对——资源（requests 是否超节点 allocatable）、约束（nodeSelector/affinity/taint）、存储（PVC 是否绑定）、配额（ResourceQuota/LimitRange）。不要先看日志：容器根本没起来，没有日志。

2. 怎么区分"容器被 OOMKilled"和"节点内存压力"？
   答：看证据位置。容器级：`containerStatuses[].lastState.terminated.reason=OOMKilled`（exit 137），事件里有对应记录，且 Pod spec 里有 memory limit；节点级：`Node.status.conditions` 出现 `MemoryPressure=True`，事件里出现 kubelet 驱逐（Evicted/NodeHasMemoryPressure）。两者的处置完全不同：前者改 limit 或修泄漏，后者要扩节点或调 requests。

3. CrashLoopBackOff 但 `logs` 是空的，可能是什么原因？
   答：常见四种——(a) 镜像拉不下来（waiting.reason=ImagePullBackOff，日志根本不会有）；(b) 容器起来了但立即退出，当前实例还没输出，要看 `--previous`；(c) init 容器失败的更早（主容器从未启动，查 initContainers 的状态与日志）；(d) command/args 被覆盖导致进程直接退出（看 spec.command 与 args）。定位顺序：先看 state 与 lastState，再决定看谁的日志。

## 6. 附：你列的名词归位表（一句话 + 归属锚点）

核心对象（锚点 2.1/2.4/2.5/2.7/2.8/2.9）
Pod 最小调度单元；Node 集群成员；Namespace 逻辑隔离与权限边界；Deployment 无状态工作负载；ReplicaSet 副本控制器（Deployment 的中间层）；StatefulSet 有状态、有序、稳定标识；DaemonSet 每节点一个；Job 跑完即止；CronJob 定时 Job；Service 稳定虚拟入口；Ingress 七层入口；Gateway API Ingress 的后继标准；Endpoint 旧版后端列表（已被 EndpointSlice 取代）；EndpointSlice 后端切片；ConfigMap 非机密配置；Secret 机密（base64，不等于加密）；Volume Pod 级挂载点；PV 集群存储资源；PVC 存储申请；StorageClass 动态供给模板；ServiceAccount Pod 身份；Role/ClusterRole 权限集合；RoleBinding/ClusterRoleBinding 绑定；NetworkPolicy 流量规则；ResourceQuota 命名空间配额；LimitRange 默认/边界资源；PDB 自愿中断预算；HPA 水平扩缩；VPA 垂直扩缩；CRD 自定义资源类型；Operator 带控制逻辑的 CRD 实践。

控制面（锚点 2.2）
kube-apiserver 唯一入口；etcd 唯一持久存储；kube-scheduler 绑定节点；kube-controller-manager 控制循环集合；cloud-controller-manager 云资源对接。

节点组件与运行时（锚点 2.3）
kubelet 节点代理；kube-proxy Service 转发；containerd/CRI-O 运行时；CRI 运行时接口；OCI 容器标准；CSI 存储接口；CNI 网络接口；cgroup 资源限制；Linux Namespace 隔离。

网络模型（锚点 2.7）
Pod IP 每 Pod 一个；扁平网络 Pod 间直连；Service VIP 稳定虚拟 IP；ClusterIP/NodePort/LoadBalancer/ExternalName 四种暴露方式；Headless Service 无 VIP 直接解析到 Pod；Ingress/Egress 入向/出向；DNS/CoreDNS 服务发现；kube-proxy/iptables/IPVS/eBPF 转发实现；NetworkPolicy 东西向策略；IPAM IP 分配；Overlay/Underlay 叠加/底层网络；VXLAN/Geneve 隧道封装；BGP 路由发布；MTU 隧道下的包大小；veth/bridge/cni0 节点上的虚拟网卡与网桥；Pod Sandbox 共享网络的载体。

CNI 插件（锚点 2.7）
Calico（BGP + 策略）、Cilium（eBPF）、Flannel（简单 overlay）、Weave Net、Antrea、Kube-OVN、OVN-Kubernetes、Multus（多网卡）、Macvlan、SR-IOV、Bridge、Host-local、DHCP、Static、Plugin Chain 与 conflist（配置链）。

层级模型（锚点 2.1/2.2/2.3）
集群 / Namespace / 工作负载 / Pod / 容器 / 控制平面 / 数据平面 / Master-Worker / API 对象 / Namespace 与 cgroup——对应本文第 3 节的关系图。

调度（锚点 2.6）
Request/Limit、QoS、Taint/Toleration、Affinity/Anti-Affinity、nodeSelector、TopologySpreadConstraints、Eviction、Preemption——见 2.6。

存储（锚点 2.8）
emptyDir（随 Pod 消失）、hostPath（绑节点路径，慎用）、ConfigMap/Secret Volume、PV/PVC/StorageClass/CSI、动态供给、StatefulSet + volumeClaimTemplates——见 2.8。

安全（锚点 2.9）
RBAC、ServiceAccount、PodSecurity/PSA、OPA-Gatekeeper、Kyverno、Admission Webhook、Secret、TLS、mTLS、NetworkPolicy——见 2.9。

可观测（锚点 2.10）
metrics-server（即时用量）、Prometheus（时序与告警）、Grafana（看板）、Loki/ELK（日志）、Jaeger/OpenTelemetry（链路）。

发布与运维（锚点 2.10）
滚动更新、回滚、蓝绿、金丝雀、Helm、Kustomize、GitOps、Argo CD、Flux。

服务发现（锚点 2.7）
Service + EndpointSlice + CoreDNS + Ingress + Gateway API——见 2.7。

运行时（锚点 2.3）
containerd/CRI-O/Docker（Docker 不再是 k8s 运行时，仅作镜像构建）、OCI、runc、Kata（强隔离）、gVisor（用户态内核）。

## 7. 附：NodeView 各字段的溯源（字段 ↔ k8s 概念 ↔ 命令）

先给结论：NodeView 是两次 API 调用的合成——`GET /api/v1/nodes/{name}` 与 `GET /api/v1/pods?fieldSelector=spec.nodeName={name}`。前九个字段直接来自 Node 对象，中间四个是"节点 + 该节点 Pod 列表"的派生量（k8s 里没有现成字段），最后一个是我们的降级标记。它是唯一"多来源"的视图，这一点决定了它的边界最多。

| 视图字段 | 来源 | 命令 | 含义 |
|---|---|---|---|
| `name` | `Node.metadata.name` | `kubectl get nodes` | 节点标识（人类命名，非机器身份） |
| `ready` | `status.conditions[type=Ready].status` | `kubectl get nodes`（STATUS 列） | kubelet 是否在正常上报 |
| `kubelet_version` | `status.nodeInfo.kubeletVersion` | `kubectl get nodes -o wide` | 该节点的 kubelet 版本 |
| `created_at` | `metadata.creationTimestamp` | `kubectl get node X -o jsonpath='{.metadata.creationTimestamp}'` | Node 对象的创建时间（≠ 机器开机时间） |
| `conditions` | `status.conditions` 全量 | `kubectl describe node X` | Ready + 三类压力 + NetworkUnavailable |
| `allocatable` | `status.allocatable` | `kubectl describe node X`（Allocatable 段） | 能分给 Pod 的量（已扣系统预留） |
| `capacity` | `status.capacity` | 同上（Capacity 段） | 机器总量 |
| `taints` | `spec.taints`（注意在 spec） | `kubectl describe node X`（Taints 行） | 拒绝条件（需与 Pod 的 tolerations 配对看） |
| `labels` | `metadata.labels` 的白名单子集 | `kubectl get node X --show-labels` | 拓扑/机型线索（我们只留 hostname/os/arch/instance-type/node-role/zone/region） |
| `pods_on_node` | 计算：List pods 后统计非终态数量 | `kubectl get pods -A --field-selector spec.nodeName=X` | 占资源的 Pod 数 |
| `requests_on_node` | 计算：非终态 Pod 的 requests 按资源名求和 | `kubectl describe node X`（Allocated resources） | 调度账本（判"还放得下吗"的分母） |
| `limits_on_node` | 计算：同上但取 limits | 同上（Limits 列） | 仅参考：调度不看 limits |
| `free_on_node` | 计算：`allocatable − requests_on_node` | 无现成命令（要手算，所以我们算好） | 余量（Pending 归因的核心数字） |
| `allocation_error` | 我们自己的降级标记 | 无（k8s 里没有这个概念） | 有值则上面四项不可信 |

单 Pod 的 requests 口径（求和时的细节）：每项资源取 `max(普通容器之和, init 容器最大值)`——init 容器不是相加；终态（Succeeded/Failed）Pod 不计入。

minikube 实测（2026-09-15）：

```text
allocatable == capacity == {"cpu":"16","memory":"30462460Ki","pods":"110","ephemeral-storage":"982240026624"}
nodeInfo: kubelet=v1.37.0 runtime=containerd://2.3.4 bootID=f6314187-c0c5-...
created=2026-09-10T00:52:44Z（Node 对象创建时间）
conditions: MemoryPressure=False DiskPressure=False PIDPressure=False Ready=True
spec.taints: 空（单节点无污点）
该节点 Pod 数（含终态）: 13
kubectl describe 的 Allocated resources: cpu 950m (5%) / memory 420Mi (1%)   ← 分母用 allocatable（本机与 capacity 相同，看不出差别）
```

### 7.1 容易混淆的边界（十条）

1. 同一 `conditions` 数组里语义方向相反：`Ready=True` 是好事，`MemoryPressure/DiskPressure/PIDPressure=True` 是坏事。读反就会把"节点有压力"当成"节点健康"。
2. `Ready` ≠ 可调度：`kubectl cordon` 只是加一条 `node.kubernetes.io/unschedulable:NoSchedule` 污点，节点依然 `Ready=True`。排障要把 ready + taints + free_on_node 三个一起看。
3. `Ready=False` 与 `Ready=Unknown` 不同：前者是 kubelet 上报"我不健康"；后者通常是 kubelet 失联/节点不可达，此时 Node 上的 status 可能是陈旧值——别把陈旧数据当实时。
4. `allocatable` ≠ `capacity`：前者扣掉了 kube-reserved / system-reserved / 驱逐阈值，调度只看 allocatable。本机两者相同（没配预留），配了预留的集群里用 capacity 判余量会高估。
5. Node 对象的创建时间 ≠ 机器运行时长：节点重装或 Node 对象被重建会让它变新；机器身份看 `status.nodeInfo.bootID` / `systemUUID`。
6. `taints` 在 spec（人为/控制器的期望声明），`conditions`、`allocatable` 在 status（系统回报）。系统自动加的 `node.kubernetes.io/not-ready`、`unreachable`（NoExecute）会驱逐不容忍的 Pod；人工 cordon 加的是 unschedulable。
7. 我们的 `labels` 是白名单子集：`--show-labels` 显示得更多，两者不一致时是"裁剪"，不是"节点没有这个标签"。
8. `pods_on_node` 不是 `allocatable.pods`（默认 110 的额度）：一个是实际占用，一个是上限，密度判断要一起看。
9. 计数口径：终态 Pod 不算占用，而 `kubectl get pods` 默认把它们也列出来——直接数会偏大。`kubectl describe node` 的 Allocated resources 与我们的口径一致（都只算非终态 requests），但百分比的分母按容量算，可能与 `free_on_node` 看着对不上。
10. `free_on_node` 是账本余量，不是"实际还能跑多少"：真实调度还要过 max pods 额度、taint/affinity、拓扑分布、端口与卷约束——余量够也可能调度失败。

```
命令：kubectl get node minikube -o jsonpath='{.status.allocatable}{"\n"}{.status.capacity}{"\n"}'
{"cpu":"16","ephemeral-storage":"982240026624","hugepages-1Gi":"0","hugepages-2Mi":"0","memory":"30462460Ki","pods":"110"}
- cpu 16 ：16 个可分配核（k8s 的 CPU 单位 1 = 1 核 = 1000m，所以也可写成 16000m）。这是 minikube 拿宿主机的 16 个逻辑核。
- memory 30462460Ki ：Ki 是二进制单位（1Ki = 1024 字节，Mi/Gi 同理；而 k/M/G 是十进制的 1000）。30462460Ki ÷ 1024 ≈ 29748.5 MiB ≈ 29.05 GiB。
- ephemeral-storage 982240026624 ：临时存储（给 Pod 用的本地盘额度：容器可写层、日志、emptyDir），无后缀就是字节数，约 915 GiB。
- hugepages-1Gi / hugepages-2Mi 0 ：大页内存没配置（配了会用来跑 DPDK 这类负载）。
- pods 110 ：这台节点最多能跑 110 个 Pod（kubelet 默认 maxPods）。

命令：kubectl get node minikube -o jsonpath='{range .status.conditions[*]}{.type}={.status} {end}{"\n"}'
MemoryPressure=False DiskPressure=False PIDPressure=False Ready=True
- MemoryPressure=False：节点内存不紧张。kubelet 拿 memory.available 与驱逐阈值比，低于阈值就置 True。注意方向：False 才是好。
- DiskPressure=False：imagefs/nodefs 的可用空间与 inode 都没触阈值。同样是 False 才好。
- PIDPressure=False：进程号没被耗尽。同上。
- Ready=True：kubelet 在正常上报心跳。它是唯一"True 才是好"的那个

命令：kubectl get pods -A --field-selector spec.nodeName=minikube --no-headers | wc -l
13

命令：kubectl get pods -A --field-selector spec.nodeName=minikube,status.phase!=Succeeded,status.phase!=Failed --no-headers | wc -l
13

kubectl get pods 默认把终态（Succeeded/Failed）也列出来，所以 13 是"含终态"的数；我们的 pods_on_node 排除了终态，可能比 13 小

命令：kubectl describe node minikube | sed -n '/Allocated resources/,/Events/p'
(Total limits may be over 100 percent, i.e., overcommitted.)
Resource           Requests    Limits
cpu                950m (5%)   100m (0%)
memory             420Mi (1%)  220Mi (0%)
ephemeral-storage  0 (0%)      0 (0%)
- cpu Requests 950m = 0.95 核；括号里的百分比是它占容量的比例（950m/16000m ≈ 5.94，显示时截断成 5%）。
- cpu Limits 只有 100m ，比 requests 还小——说明绝大多数 Pod 没配 CPU limits（k8s 不要求配）。limits 是"上限"，允许汇总超过容量（就是那行 overcommitted 的意思），超卖时一旦都跑满，节点按 QoS 从低到高驱逐。
- memory Requests 420Mi（≈1.4%）、Limits 220Mi（≈0.7%）——同理。
- ephemeral-storage 与 hugepages 全是 0 ：不是"没占用"，而是"没有 Pod 申报这些资源"。Requests 为 0 的含义是"没申报"，不是"不使用"，这条最容易误读。
- 括号里百分比的分母按容量算，而不变的是分子（requests 合计）。
```