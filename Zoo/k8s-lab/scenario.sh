#!/usr/bin/env bash
# 第 3 批场景集：单场景生命周期（list / up / down / target / down-all）。
#
#   用法: bash Zoo/k8s-lab/scenario.sh up pending-pvc-unbound
#         bash Zoo/k8s-lab/scenario.sh target pending-pvc-unbound   # 打印 ns/pod，供 eval.sh 用
#   前置: minikube 已启动（脚本只检查，不代你起）：
#         minikube start && minikube addons enable metrics-server
#
# 与冒烟台（smoke.sh）的分工：smoke 是"采集链路能不能跑通"的快检（两个目标、无 LLM）；
# 这里的每个目录是评测集里的一个场景——就绪条件写在 expect.json 里（自描述），
# 起来之后交给采集层断言（lab_scenarios_test.go）与端到端打分（cmd/k8seval + eval.sh）。
#
# 目标定位一律走 expect.json 的 selector 而不是 Pod 名：Deployment 生成的 Pod 名带
# ReplicaSet hash，写死名字不可行；裸 Pod 也打同样的标签，两种场景走同一条路径。
#
# 可选钩子：场景目录里若有 setup.sh / teardown.sh，则 up 在 apply 前跑 setup、
# down 在清理后跑 teardown、down-all 对每个带 teardown 的场景都跑一遍。为什么需要它：
# 有些前置是节点级对象（如给节点打 taint），写不进 manifest.yaml——场景文件只能 apply
# 命名空间内的对象。钩子必须幂等：down-all 与重复 up 都会重复触发。
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
SCENARIOS="$ROOT/Zoo/k8s-lab/scenarios"
NS=diag-lab

usage() {
  echo "用法: bash Zoo/k8s-lab/scenario.sh list | up <name> | down <name> | target <name> | down-all" >&2
}

need_cluster() {
  if ! kubectl cluster-info >/dev/null 2>&1; then
    echo "连不上集群。先执行：minikube start && minikube addons enable metrics-server" >&2
    exit 1
  fi
}

# json_str/json_num 只读 expect.json 里的扁平字段（"key": "value" 与 "key": 数字）。
# 为什么不上 jq 或写个解析器：本机不一定有 jq，而 expect.json 是我们自己维护的扁平结构，
# 四个键的读取不值得引依赖。读不到键一律报错退出——宁可脚本失败，也不要"读到空值
# 当成条件不满足"，那会把脚本 bug 伪装成"场景没就绪"。
json_str() {
  local file="$1" key="$2" val
  val=$(grep -m1 -o "\"$key\"[[:space:]]*:[[:space:]]*\"[^\"]*\"" "$file" \
    | sed -E 's/.*:[[:space:]]*"([^"]*)".*/\1/') || true
  if [ -z "$val" ]; then
    echo "expect.json 缺字段 $key（$file）" >&2
    return 1
  fi
  printf '%s' "$val"
}

json_num() {
  local file="$1" key="$2" val
  val=$(grep -m1 -o "\"$key\"[[:space:]]*:[[:space:]]*[0-9]\+" "$file" | grep -o '[0-9]\+$') || true
  if [ -z "$val" ]; then
    echo "expect.json 缺数字字段 $key（$file）" >&2
    return 1
  fi
  printf '%s' "$val"
}

# json_opt_str 读可选字段（不存在返回空，不报错）——wait 段允许只给其中几个条件。
json_opt_str() {
  local file="$1" key="$2" val
  val=$(grep -m1 -o "\"$key\"[[:space:]]*:[[:space:]]*\"[^\"]*\"" "$file" \
    | sed -E 's/.*:[[:space:]]*"([^"]*)".*/\1/') || true
  printf '%s' "$val"
}

# json_opt_num 同上，读可选数字字段（wait 里的计数类条件，如 container_restart_count_min）。
json_opt_num() {
  local file="$1" key="$2" val
  val=$(grep -m1 -o "\"$key\"[[:space:]]*:[[:space:]]*[0-9]\+" "$file" | grep -o '[0-9]\+$') || true
  printf '%s' "$val"
}

# scenario_dir 校验场景存在且两个文件齐全（缺文件是最常见的拼错目录名，直接报清楚）。
scenario_dir() {
  local name="$1" dir="$SCENARIOS/$1"
  if [ ! -f "$dir/manifest.yaml" ] || [ ! -f "$dir/expect.json" ]; then
    echo "场景不存在或不完整: $name（期望 $dir/{manifest.yaml,expect.json}）" >&2
    exit 1
  fi
  printf '%s' "$dir"
}

ensure_ns() {
  kubectl get ns "$NS" >/dev/null 2>&1 || kubectl create ns "$NS" >/dev/null
}

# run_hook 跑场景目录里的可选钩子（setup.sh / teardown.sh），没有就静默跳过。
# 钩子失败不让调用方中断（teardown 失败时对象已经删了，中断只会把"清理没干净"
# 伪装成"命令失败"）；要真的没清干净，由钩子自己打警告。
run_hook() {
  local dir="$1" hook="$2"
  [ -f "$dir/$hook" ] || return 0
  bash "$dir/$hook" || true
}

# pod_of 按 selector 取该场景的 Pod 名（场景内 replicas=1，且 up/down 都清旧对象，不会有多个）。
pod_of() {
  kubectl -n "$NS" get pod -l "$1" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true
}

# target_of 打印 "namespace/pod"，是 eval.sh 取诊断目标的唯一入口（解析逻辑只留这一处）。
target_of() {
  local name="$1" dir selector pod
  dir="$(scenario_dir "$name")"
  selector="$(json_str "$dir/expect.json" selector)"
  pod="$(pod_of "$selector")"
  if [ -z "$pod" ]; then
    echo "场景 $name 没有 Pod（先 up，或它已被清理）" >&2
    return 1
  fi
  printf '%s/%s' "$NS" "$pod"
}

# wait_ready 按 expect.json 的 wait 段轮询：给出哪几个条件就等哪几个（全满足才算就绪）。
# 为什么等而不是 sleep：拉镜像、CrashLoop 退避、调度都有各自的延迟，拍脑袋的 sleep
# 在慢机器上必然误判（冒烟台那条 529 的老经验）。
wait_ready() {
  local name="$1" dir selector timeout want_reason want_last want_phase want_restarts want_ready want_event t=0
  dir="$(scenario_dir "$name")"
  selector="$(json_str "$dir/expect.json" selector)"
  timeout="$(json_num "$dir/expect.json" timeout_seconds)"
  want_reason="$(json_opt_str "$dir/expect.json" container_state_reason)"
  want_last="$(json_opt_str "$dir/expect.json" container_last_state_reason)"
  want_phase="$(json_opt_str "$dir/expect.json" phase)"
  want_restarts="$(json_opt_num "$dir/expect.json" container_restart_count_min)"
  # pod_ready 是"没重启但也不可用"类场景（readiness 探针错）的唯一判据：
  # phase 在容器还没拉起来时也是 Running，光看 phase 会把"还没起"当成就绪。
  want_ready="$(json_opt_str "$dir/expect.json" pod_ready)"
  # event_reason 是 Pending 类场景的判别依据（如调度失败的具体原因）：Pod 一创建 phase 就是
  # Pending，只看它会在事件还没写出来时就宣布"就绪"——诊断那边正好缺的就是那条事件。
  want_event="$(json_opt_str "$dir/expect.json" event_reason)"
  if [ -z "$want_reason$want_last$want_phase$want_restarts$want_ready$want_event" ]; then
    echo "expect.json 的 wait 段至少要给一个条件（container_state_reason / container_last_state_reason / phase / container_restart_count_min / pod_ready / event_reason）" >&2
    return 1
  fi
  while [ "$t" -lt "$timeout" ]; do
    local pod cur_reason cur_last cur_phase cur_restarts cur_ready ok=1
    pod="$(pod_of "$selector")"
    if [ -n "$pod" ]; then
      cur_reason=$(kubectl -n "$NS" get pod "$pod" -o jsonpath='{.status.containerStatuses[0].state.waiting.reason}' 2>/dev/null || true)
      cur_last=$(kubectl -n "$NS" get pod "$pod" -o jsonpath='{.status.containerStatuses[0].lastState.terminated.reason}' 2>/dev/null || true)
      cur_phase=$(kubectl -n "$NS" get pod "$pod" -o jsonpath='{.status.phase}' 2>/dev/null || true)
      cur_restarts=$(kubectl -n "$NS" get pod "$pod" -o jsonpath='{.status.containerStatuses[0].restartCount}' 2>/dev/null || true)
      cur_ready=$(kubectl -n "$NS" get pod "$pod" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || true)
      if [ -n "$want_reason" ] && [ "$cur_reason" != "$want_reason" ]; then ok=0; fi
      if [ -n "$want_last" ] && [ "$cur_last" != "$want_last" ]; then ok=0; fi
      if [ -n "$want_phase" ] && [ "$cur_phase" != "$want_phase" ]; then ok=0; fi
      if [ -n "$want_restarts" ] && [ "${cur_restarts:-0}" -lt "$want_restarts" ]; then ok=0; fi
      if [ -n "$want_ready" ] && [ "$cur_ready" != "$want_ready" ]; then ok=0; fi
      if [ -n "$want_event" ] && ! kubectl -n "$NS" get events \
        --field-selector "involvedObject.name=$pod,reason=$want_event" -o name 2>/dev/null | grep -q .; then ok=0; fi
      if [ "$ok" = 1 ]; then
        echo "  $name → $pod 就绪（reason=${cur_reason:-无} last=${cur_last:-无} phase=${cur_phase:-无} restarts=${cur_restarts:-0} ready=${cur_ready:-无}）"
        return 0
      fi
    fi
    sleep 3
    t=$((t + 3))
  done
  echo "场景 $name 等了 ${timeout}s 未就绪，排查：" >&2
  echo "  kubectl -n $NS get pod -l $selector" >&2
  echo "  kubectl -n $NS describe pod -l $selector | tail -n 30" >&2
  return 1
}

cmd_list() {
  local dir name
  for dir in "$SCENARIOS"/*/; do
    [ -f "$dir/expect.json" ] || continue
    name="$(basename "$dir")"
    printf '  %-32s %-18s %s\n' "$name" \
      "$(json_str "$dir/expect.json" symptom)" "$(json_str "$dir/expect.json" selector)"
  done
}

cmd_up() {
  local name="$1" dir
  dir="$(scenario_dir "$name")"
  need_cluster
  ensure_ns
  # 幂等：先按标签清掉同名场景的旧对象（apply 不会重置已退避的容器，也不会重跑已完成的 Job），
  # 再 apply。清完等一秒，避免与随后的 apply 抢同名对象。
  kubectl -n "$NS" delete all,pvc -l "lab=$name" --ignore-not-found >/dev/null 2>&1 || true
  run_hook "$dir" setup.sh
  kubectl apply -f "$dir/manifest.yaml" >/dev/null
  echo "  场景 $name 已 apply"
  wait_ready "$name"
}

cmd_down() {
  local name="$1" dir
  dir="$(scenario_dir "$name")"
  kubectl delete -f "$dir/manifest.yaml" --ignore-not-found >/dev/null 2>&1 || true
  kubectl -n "$NS" delete all,pvc -l "lab=$name" --ignore-not-found >/dev/null 2>&1 || true
  run_hook "$dir" teardown.sh
  echo "  场景 $name 已清理（含它的 PVC；集群级对象如 PV 交给 storageclass 回收策略）"
}

# cmd_down_all 清理所有带 lab 标签的对象，并把每个场景的 teardown 都跑一遍——钩子可能改的是
# 节点级对象（taint），删命名空间内的对象清不掉它，漏了会让后续场景全部 Pending。
cmd_down_all() {
  local dir name
  need_cluster
  kubectl -n "$NS" delete all,pvc -l lab --ignore-not-found >/dev/null 2>&1 || true
  for dir in "$SCENARIOS"/*/; do
    name="$(basename "$dir")"
    run_hook "$dir" teardown.sh
  done
  echo "  diag-lab 下所有带 lab 标签的对象已清理（冒烟目标 badimg/badweb/once-done 不带该标签，需 smoke.sh down）"
}

case "${1:-}" in
  list) cmd_list ;;
  up) [ $# -ge 2 ] || { usage; exit 1; }; cmd_up "$2" ;;
  down) [ $# -ge 2 ] || { usage; exit 1; }; cmd_down "$2" ;;
  target) [ $# -ge 2 ] || { usage; exit 1; }; target_of "$2" ;;
  down-all) cmd_down_all ;;
  *) usage; exit 1 ;;
esac
