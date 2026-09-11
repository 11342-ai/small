# probefailed-readiness-port

症状 ProbeFailed；应用（busybox httpd）在 8080 上服务正常，readiness 探针却探 8081，
kubelet 每次探测都连接被拒 → Pod 永远 NotReady（Ready=False），容器既不退出也不重启
（restartCount=0），这正是它区别于 restart-liveness-kill 的地方。

人工复核：`kubectl -n diag-lab get pod <pod>` 的 READY 列为 `0/1` 而 STATUS 是 Running；
`kubectl -n diag-lab describe pod <pod>` 事件里是 `Readiness probe failed: ... 8081 ... connection refused`。

期望结论：`root_cause_category=probe_port_wrong`，置信度 ≥ 0.8；证据要同时覆盖探针配置里的端口
（8081）与容器实际监听的端口（8080，来自 spec 的 containerPort），单说"探针失败"不算定位。
