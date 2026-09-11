# oom-heap-misconfig

症状 OOMKilled；容器是 JVM（`eclipse-temurin:17`），命令行写着 `-Xmx256m -Xms256m
-XX:MaxMetaspaceSize=64m -XX:+AlwaysPreTouch`，而容器 memory limit 只有 `128Mi`——
堆上限加元空间是 limit 的两倍半，`AlwaysPreTouch` 让 JVM 启动时就把堆整段摸一遍，
于是容器在启动瞬间被内核杀掉（退出码 137、reason OOMKilled），反复退避重启。

人工复核：`kubectl -n diag-lab get pod <pod> -o jsonpath='{.items[0].spec.containers[0].command}'`
能看到 `-Xmx256m`；`kubectl -n diag-lab describe pod <pod>` 的 Last State 是 OOMKilled (137)。

镜像准备（本机一次性，之后场景完全离线）：主机 docker 里已有 `eclipse-temurin:17`，执行
`minikube image load eclipse-temurin:17` 即可；tag 不是 latest，默认 `IfNotPresent` 直接用本地镜像。

期望结论：`root_cause_category=oom_heap_misconfig`；判据是"命令行里的堆上限（+元空间）超过了
容器 limit"这条配置矛盾，而不是"limit 相对于应用需求太小"。与 `oom_limit_too_small` 的分界就在这里：
那边没有任何堆配置、用量是贴着 limit 涨上去的；这边堆是显式配过且配大的。
