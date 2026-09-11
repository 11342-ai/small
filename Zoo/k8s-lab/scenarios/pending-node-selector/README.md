# pending-node-selector

症状 Pending；Pod 要求 `lab-node-role=does-not-exist`，而集群里没有任何节点带这个标签，
调度器按硬门槛直接拒绝（`node(s) didn't match Pod's node affinity/selector`），Pod 一直 Pending。

人工复核：`kubectl -n diag-lab get nodes --show-labels` 看不到 lab-node-role；
`kubectl -n diag-lab describe pod <pod>` 的事件里是 selector 不匹配而非 Insufficient。

期望结论：`root_cause_category=pending_node_selector`；证据要把 Pod spec 的 nodeSelector
与节点的实际标签对上（这是它区别于"节点余量不足"的唯一依据），且不能因为余量数字不好看就改判。
