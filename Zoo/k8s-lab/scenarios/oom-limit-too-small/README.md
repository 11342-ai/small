# oom-limit-too-small

症状 OOMKilled；容器一次性申请 100MB 匿名内存而 limit 只有 64Mi，被 cgroup 杀掉后按 Always 重启。

人工复核：`kubectl -n diag-lab get pod <pod> -o jsonpath='{.status.containerStatuses[0].lastState.terminated.reason}'`
应为 `OOMKilled`（exitCode 137）。

期望结论：`root_cause_category=oom_limit_too_small`（不是内存泄漏、更不是节点内存不足）；
证据要覆盖 lastState 的 OOMKilled 与 spec 里的 limits（64Mi）。
