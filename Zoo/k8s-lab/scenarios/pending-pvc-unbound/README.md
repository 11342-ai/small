# pending-pvc-unbound

症状 Pending；Pod 引用一个指向不存在 storageClass 的 PVC，claim 永远 Pending，调度被卡在
"unbound immediate PersistentVolumeClaims"。这条与 pending-insufficient-resources 是一对：
两者都是 Pending，但一个是节点放不下，一个是存储没就绪，判据完全不同。

人工复核：`kubectl -n diag-lab get pvc lab-pending-claim` 应为 Pending；
`kubectl -n diag-lab describe pod <pod>` 的事件里是 unbound PVC 而非 Insufficient。

期望结论：`root_cause_category=pending_pvc_unbound`；证据要落到 claim 名与它的
storageClass 取值（`lab-no-such-sc`），而不是笼统地说"存储有问题"。
