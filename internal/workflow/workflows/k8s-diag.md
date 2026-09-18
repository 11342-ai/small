---
name: k8s-diag
description: 诊断 K8s Pod 生命周期异常，产出带证据链与置信度的根因报告
requires: k8s
trigger: 用户报告 Pod 异常（一直重启 / 起不来 / OOMKilled / ImagePullBackOff / 一直 Pending / 探针失败）或要求诊断某个 Pod（含 /diag 命令显式进入）
input: namespace/pod（命令形式 /diag <namespace>/<pod>）；可选：症状描述、时间范围
output: 对话内 RCA 报告（根因 + 置信度 + 证据链 + 被否候选 + 缺失证据 + 建议）+ 落盘产物（evidence.json / report.json / report.md）
stop: k8s_report 提交并向用户汇报后，等待用户确认或追问即结束；工具连续失败、用户打断、轮次超限立即停止并说明已产出的部分与原因
---
1. 定位目标：用 k8s_pod 确认 namespace/pod 存在并读出症状字段（phase、waiting reason、lastState、restartCount）；缺参数先向用户问清，不要猜。
2. 一次收集：调 k8s_evidence 拿全部证据（已按预算裁剪）；返回里的 notes 每条带类别——`required_failed` 才算"缺失证据"（写进 missing_evidence 并压置信度）、`optional_unavailable` 是 metrics 类可选源（不影响结论）、`budget_trimmed` 是被预算裁掉的（不是没有）、`no_data` 是来源正常但确实没数据（如容器未启动没有日志）。不要把任何一类当成"没有问题"。
3. 症状归类：在 CrashLoopBackOff / OOMKilled / ImagePullBackOff / Pending / ContainerRestart / ProbeFailed 中定主症状（可多，如 CrashLoop 常伴 OOMKilled）；判别顺序：事件 reason → lastState/exitCode → 日志与配置。
4. 列候选根因（取值只能来自 k8s_report 词表；括号里是该类别的判别线索，不是结论）：
   - CrashLoopBackOff：crashloop_app_exit（命令不存在/panic/断言失败，无更具体线索）/ crashloop_missing_dependency（DNS 解析失败、connection refused、超时、认证失败）/ crashloop_config_error（非法 flag、必填项缺失、env 引用缺 key：CreateContainerConfigError）/ crashloop_volume_mount_error（FailedMount、read-only file system、Permission denied）
   - OOMKilled：oom_limit_too_small（用量贴 limit，重启前后是平线）/ oom_memory_leak（随重启次数上升）/ oom_heap_misconfig（-Xmx 等堆上限 > 容器 limit）
   - ImagePullBackOff：imagepull_tag_missing（not found / manifest unknown）/ imagepull_name_invalid（repository does not exist、invalid reference）/ imagepull_missing_secret（401/403、pull access denied、unauthorized，且 spec 与 SA 两处都无凭据）/ imagepull_registry_unreachable（timeout、no such host、EOF）
   - Pending：pending_insufficient_resources（Insufficient cpu/memory，requests > free_on_node）/ pending_node_selector（selector/affinity 不匹配、节点标签缺键）/ pending_taint_not_tolerated（untolerated taint；事件不列出是哪把 taint，键与 effect 从 k8s_nodes 的 taints 看）/ pending_pvc_unbound（unbound PersistentVolumeClaims、claim Pending）/ pending_quota_exceeded（ResourceQuota / LimitRange exceeded）
   - ContainerRestart（非 OOM）：restart_probe_kill（Liveness probe failed + will be restarted，lastState=Error 而应用日志正常；但探针配置能看出端口/路径/时序错的，用 probe_* 类）/ restart_app_crash（应用自崩，无探针事件）/ restart_unknown（有重启但日志、终态原因、事件都缺）
   - ProbeFailed：probe_path_wrong（探针 HTTP 404）/ probe_port_wrong（connection refused，探针端口≠监听端口；liveness 因此杀容器也用它）/ probe_timing_too_short（initialDelay/period 短于启动耗时：先失败后自愈；liveness 因此杀容器也用它）
   - 兜底 other：归不进以上任一类。选它必须写清"是什么 + 缺哪条证据"，否则等于把"没查出来"包装成结论。
5. 按需深挖：证据不足时用原子工具补。按症状的必查项——CrashLoop：k8s_logs 带 previous=true 与 lastState 退出码（previous 可能已取不到，正文会写 "unable to retrieve container logs"，那时当前日志里就是崩溃前的输出），以及 env 引用的 configMap/secret 是否存在（CreateContainerConfigError 类启动失败）；OOM：k8s_metrics（集群没装 metrics-server 时该工具不会注册，那就跳过它，改用 k8s_evidence 返回里的用量字段）看用量是否贴 limit + k8s_workload 看 limits 怎么配的；ImagePull：k8s_events 里的镜像名与错误码；Pending：k8s_events 的调度事件 + k8s_nodes 列全部节点（Pending 的 Pod 没有 node_name，k8s_node 取不到，必须走这条）核对 Pod spec 的调度约束（nodeSelector/tolerations/affinity，其中 affinity 的 required 是硬门槛）与节点标签/taint/余量，并用 pod 引用的 PVC 绑定状态；Probe：k8s_workload 的探针配置 + k8s_events 的 Unhealthy 事件。
6. 交叉验证：对每个候选根因同时找支持证据与反对证据，写明被否候选及否决理由；不允许把"没取到证据"当作"排除"。
7. 置信度裁决：≥80% 需至少两条独立证据且含一条决定性证据（termination reason / event reason / 配置数值），且关键证据无缺失；关键证据缺失时上限 79% 并写进 missing_evidence；每条证据必须带来源（工具名 + 对象/字段/时间），不得出现"可能/也许"这类无支撑表述。提交时工具会校验"≥0.80 需至少两条独立来源的证据"，不满足会被打回——补证据或降档，别硬报。
8. 提交与汇报：调 k8s_report 落盘（report.json + report.md），root_cause.category 必须取自参数说明里的词表（枚举），拿不准就用 other 并在 summary 里写清是什么、缺哪条证据；把渲染后的报告转给用户，声明 [分支完成:k8s-diag]，等待用户确认或追问（结束）。
