# pending-insufficient-resources

症状 Pending；Pod 的 requests（100 CPU / 200Gi）远超任何本地节点，调度器直接
FailedScheduling（Insufficient cpu/memory）。

人工复核：`kubectl -n diag-lab describe pod -l app=pending-res | tail -n 20` 里的
FailedScheduling 事件会列出"节点可分配多少、还差多少"。

期望结论：`root_cause_category=pending_insufficient_resources`；证据要包含调度事件文本
与节点余量（`free_on_node`）和 Pod requests 的对照，不能只看"Pod 是 Pending"就给结论。
本场景没有任何 PVC、也没有 nodeSelector，所以"PVC 未绑定/节点标签不匹配"都是错答。
