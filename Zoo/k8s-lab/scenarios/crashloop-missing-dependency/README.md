# crashloop-missing-dependency

症状 CrashLoopBackOff；容器启动后去访问一个不存在的 Service（`db-svc`），DNS 解析失败后非零退出，
kubelet 退避重启。与 `crashloop-app-error` 是一对：都是"起来就退"，但一个败在依赖，一个败在自身。

人工复核：`kubectl -n diag-lab logs <pod>` 里能看到 `bad address 'db-svc...'`（或 `no such host`）；
`kubectl -n diag-lab get endpoints db-svc` 为空——这个 Service 根本不存在。

期望结论：`root_cause_category=crashloop_missing_dependency`，置信度 ≥ 0.8；证据要落到日志里的
目标主机名与解析失败原文，不能只凭"退出码非 0"就说是应用自身的问题。
