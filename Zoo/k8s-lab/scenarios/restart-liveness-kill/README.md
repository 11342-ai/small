# restart-liveness-kill

症状 ContainerRestart；应用（busybox httpd）监听 8080 且运行正常，但 liveness 探针指向 8081，
连接被拒达 failureThreshold 次后 kubelet 杀容器重启，restartCount 持续上涨。

人工复核：`kubectl -n diag-lab describe pod <pod>` 事件里有
`Liveness probe failed: ... :8081 ... connection refused` 与 `will be restarted`；
`kubectl -n diag-lab get pod <pod> -o jsonpath='{.status.containerStatuses[0].lastState.terminated}'`
是 reason=Error、exitCode 137。

期望结论：`root_cause_category=probe_port_wrong`（探针端口 8081 ≠ 应用监听的 8080），置信度 ≥ 0.8。
这是本批最容易误判的一条：137 与 OOMKilled 同码，但 lastState 的 reason 与事件能区分开；只看退出码就报"内存超限被杀"会被一票否决。

类别口径（2026-09-15 定）：探针失败导致重启时，优先判"具体错配"那一类——探针端口/路径/时序能看出错的用
`probe_port_wrong` / `probe_path_wrong` / `probe_timing_too_short`；只有确认被 liveness 探针杀、但配置本身
看不出错配时，才用兜底的 `restart_probe_kill`。理由：具体错配更有指向性（直接告诉你改哪一行配置）。
