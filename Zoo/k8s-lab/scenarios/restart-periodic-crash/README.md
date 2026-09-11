# restart-periodic-crash

症状 ContainerRestart；应用起来正常服务，跑 25 秒后以非 0 退出码自行退出，restartPolicy 把它拉起来，
如此周期往复（restartCount 持续上涨）。

人工复核：`kubectl -n diag-lab get pod <pod> -o jsonpath='{.status.containerStatuses[0]}'`：
restartCount 在涨、lastState.terminated 是 `reason: Error` / `exitCode: 1`，且 startedAt 与 finishedAt
差约 25 秒；`kubectl -n diag-lab describe pod <pod>` 的事件里没有 probe 失败（spec 里压根没配探针）。

期望结论：`root_cause_category=restart_app_crash`，置信度 ≥ 0.8；证据要覆盖 restartCount/lastState
与"没有探针配置、也没有 OOMKilled"这两面——前者是它与 restart-liveness-kill 的分界，后者排除 OOM。

注意本场景的 `must_not_claim` 是空的：判别点正是"没有探针"，任何以探针为词的否决短语都会把
"事件里没有探针失败"这种正确表述误判成错答（口径见 jjj §21.3）。
