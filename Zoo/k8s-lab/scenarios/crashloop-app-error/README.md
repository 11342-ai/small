# crashloop-app-error

症状 CrashLoopBackOff；容器启动后立刻 `exit 1`（脚本里打印一行 fatal）。六类里最基础的一条：
应用自己起不来，不是被调度、存储或探针挡住的。

人工复核：`kubectl -n diag-lab logs <pod> --previous` 能看到那行 fatal——但 minikube + containerd
下 previous 日志常常已被运行时回收（正文是 `unable to retrieve container logs`），那种时候直接看当前
日志即可，那行 fatal 就在里面；`kubectl -n diag-lab get pod <pod> -o jsonpath='{.status.containerStatuses[0].lastState}'`
是 `Error` / exitCode 1。

期望结论：`root_cause_category=crashloop_app_exit`，置信度 ≥ 0.8（证据齐全：崩溃输出 + 终态退出码）。
