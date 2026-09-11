#!/usr/bin/env bash
# 场景后置：摘掉 setup 打的 taint。
#
# 幂等是关键：down-all 会对每个带 teardown 的场景都跑一遍，重复执行必须无害。
# taint 已经不在时 kubectl 会返回非 0（既不在也不报错的分支不值得写），这里吞掉。
# 但"没摘掉"必须是响的：留着它会让后续所有场景的 Pod 一律 Pending（单节点集群，
# NoSchedule 直接封掉唯一可调度目标），静默下去会变成"另一个场景坏了"的误诊。
set -uo pipefail

kubectl taint nodes --all diag-lab-scenario- 2>/dev/null || true

if kubectl get nodes -o jsonpath='{range .items[*]}{.spec.taints[*].key}{"\n"}{end}' | grep -qx 'diag-lab-scenario'; then
  echo "警告：taint diag-lab-scenario 仍在，后续场景会被挡住。手动执行：" >&2
  echo "  kubectl taint nodes --all diag-lab-scenario-" >&2
  exit 1
fi
