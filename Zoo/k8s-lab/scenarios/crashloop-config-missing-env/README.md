# crashloop-config-missing-env

症状 CrashLoopBackOff；应用脚本要求 `MODE` 环境变量，而 Pod spec 里没有任何 env，脚本打印
`fatal: missing required env MODE` 后 `exit 1`，kubelet 退避重启。

人工复核：`kubectl -n diag-lab logs <pod>` 里是那行 missing required env；
`kubectl -n diag-lab get pod <pod> -o jsonpath='{.spec.containers[0].env}'` 为空（不是引用了某个
configMap/secret 缺 key——那种情况的状态是 CreateContainerConfigError，容器压根不会被创建）。

期望结论：`root_cause_category=crashloop_config_error`，置信度 ≥ 0.8；证据要同时覆盖日志里的
缺失项名（MODE）与 spec 里 env 为空这个事实，不能与"依赖不可达"混为一谈。
