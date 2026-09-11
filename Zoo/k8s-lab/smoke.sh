#!/usr/bin/env bash
# 第 1 批（采集包 internal/k8s）真集群冒烟环境：起场景 / 跑断言 / 收场景。
#
#   用法: bash Zoo/k8s-lab/smoke.sh up | test | down
#   前置: minikube 已启动（脚本只检查，不代你起）：
#         minikube start && minikube addons enable metrics-server
#
# 为什么要有它：fake clientset 的单测只验证逻辑，真实传输语义（ctx 取消、限流、
# 字段实际取值、metrics server 有无数据）只有连真集群才暴露——上一批就靠它抓到过一个
# ctx 提前取消的 bug（见 Zoo/temp/jjj.md 的验证记录）。
set -euo pipefail

# 仓库根：脚本在 Zoo/k8s-lab/ 下，故上溯两级。
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
NS=diag-lab
MANIFEST="$ROOT/Zoo/k8s-lab/smoke.yaml"

need_cluster() {
  if ! kubectl cluster-info >/dev/null 2>&1; then
    echo "连不上集群。先执行：minikube start && minikube addons enable metrics-server" >&2
    exit 1
  fi
}

# wait_reason 等某个 Pod 的 waiting reason 到达期望值：拉镜像失败是异步发生的
# （先 ErrImagePull，退避后才 ImagePullBackOff），所以要等而不是 sleep 一个拍脑袋的值。
wait_reason() {
  local name="$1" want="$2" r=""
  for _ in $(seq 1 30); do
    r=$(kubectl -n "$NS" get pod "$name" -o jsonpath='{.status.containerStatuses[0].state.waiting.reason}' 2>/dev/null || true)
    if [ "$r" = "$want" ]; then
      echo "  $name → $want"
      return 0
    fi
    sleep 2
  done
  echo "  $name 等 $want 超时（当前 ${r:-未知}）" >&2
  return 1
}

# metrics_ready 探测 metrics server 是否真的能出数：APIService Available=True 只说明
# aggregation 层注册好了，还要 kubectl top 能拿到数才算就绪——刚重启的 metrics server
# 需要一两个 scrape 周期（约 30-60 秒）才出数。返回 0=就绪，1=未就绪（原因打到 stderr）。
metrics_ready(){
  local avail
  avail=$(kubectl get apiservice v1beta1.metrics.k8s.io \
    -o jsonpath='{.status.conditions[?(@.type=="Available")].status}' 2>/dev/null || true)
  if [ "$avail" != "True" ]; then
    echo "  APIService v1beta1.metrics.k8s.io 未 Available（当前 ${avail:-未知}）" >&2
    return 1
  fi
  if ! kubectl top nodes >/dev/null 2>&1; then
    echo "  APIService 已 Available，但 kubectl top 还拿不到数（多半在重启后的首个 scrape 窗口）" >&2
    return 1
  fi
  return 0
}

# wait_metrics 等 metrics 就绪（最多约 90 秒）：把"环境前提"等到位，
# 否则 tag 测试会撞上 metrics server 的重启窗口，把环境抖动记成采集缺陷。
wait_metrics() {
  for _ in $(seq 1 30); do
    if metrics_ready; then
      echo "  metrics server 就绪（APIService Available 且 kubectl top 有数）"
      return 0
    fi
    sleep 3
  done
  echo "metrics server 等了约 90 秒仍未就绪，排查：" >&2
  echo "  kubectl -n kube-system get pods -l k8s-app=metrics-server" >&2
  echo "  kubectl -n kube-system describe pod -l k8s-app=metrics-server | grep -A3 'Last State'" >&2
  echo "  minikube addons enable metrics-server" >&2
  return 1
}

# wait_job_succeeded 等 Job 至少成功一次：终态 Pod 是"计数口径"回归的前提
# （pods_on_node 排除终态，而 kubectl get pods 会把它列出来）。
wait_job_succeeded() {
  local name="$1" s=""
  for _ in $(seq 1 30); do
    s=$(kubectl -n "$NS" get job "$name" -o jsonpath='{.status.succeeded}' 2>/dev/null || true)
    if [ "${s:-0}" -ge 1 ]; then
      echo "  $name → Succeeded（终态 Pod 就绪）"
      return 0
    fi
    sleep 2
  done
  echo "  $name 等成功超时（当前 succeeded=${s:-0}）；镜像拉不动时会一直卡在这" >&2
  return 1
}

case "${1:-up}" in
  up)
    need_cluster
    # 一次性 Job 必须重建才会产生新的终态 Pod（apply 同一 spec 不会重跑已完成的 Job）。
    kubectl -n "$NS" delete job once-done --ignore-not-found --wait=true >/dev/null 2>&1 || true
    kubectl apply -f "$MANIFEST"
    echo "等待故障状态就绪…"
    wait_reason badimg ImagePullBackOff
    dep_pod=$(kubectl -n "$NS" get pods -l app=badweb -o jsonpath='{.items[0].metadata.name}')
    wait_reason "$dep_pod" ImagePullBackOff
    echo "等待一次性 Job 跑成功（终态 Pod）…"
    wait_job_succeeded once-done
    echo "场景就绪：故障目标 $NS/badimg 与 $NS/$dep_pod；终态目标 $NS/once-done-*；健康目标复用 kube-system 的 kube-dns Pod"
    echo "等待 metrics server 就绪"
    wait_metrics
    ;;
  test)
    need_cluster
    if ! metrics_ready; then
      echo "跳过 tag 测试：metrics server 未就绪（原因见上）。" >&2
      echo "先跑 bash Zoo/k8s-lab/smoke.sh up（它会等 metrics 就绪）" >&2
      exit 0
    fi
    cd "$ROOT"
    go test -tags k8slab ./internal/k8s/ -run Lab -count=1 -v
    ;;
  down)
    kubectl delete ns "$NS" --ignore-not-found
    ;;
  *)
    echo "用法: bash Zoo/k8s-lab/smoke.sh up | test | down" >&2
    exit 2
    ;;
esac