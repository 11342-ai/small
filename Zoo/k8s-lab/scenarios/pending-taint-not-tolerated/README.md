# pending-taint-not-tolerated

症状 Pending；集群节点被打了 `diag-lab-scenario=taint-not-tolerated:NoSchedule`，Pod 没有对应容忍，
调度器把唯一节点判为不可用（`0/1 nodes are available: 1 node(s) had untolerated taint(s)`），
Pod 一直 Pending。

taint 由本目录的 `setup.sh` 打、`teardown.sh` 摘（`scenario.sh up/down/down-all` 自动调用）：
它是节点级对象，写不进 `manifest.yaml`。这也是 `scenario.sh` 加钩子机制的原因。

人工复核：`kubectl get nodes -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.spec.taints}{"\n"}{end}'`
能看到 taint；`kubectl -n diag-lab describe pod <pod>` 的事件里是 untolerated taint，不是 Insufficient、
也不是 selector 不匹配；`kubectl -n diag-lab get deploy pending-taint -o yaml` 里没有 tolerations
（Pod 上会有 not-ready / unreachable 两条系统默认容忍，那是 API server 注入的，不算数）。

期望结论：`root_cause_category=pending_taint_not_tolerated`；本版调度器的事件文案不列出 taint 键，
所以"是哪把 taint"必须从节点侧证据看出来——证据要把节点的 taints 与 Pod 未容忍对上，
且不能因为节点上还有余量数字就走了"资源不足"。
