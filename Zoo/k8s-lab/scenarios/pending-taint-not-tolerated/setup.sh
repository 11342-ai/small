#!/usr/bin/env bash
# 场景前置：给集群里所有节点打一个 NoSchedule taint。
#
# 为什么必须由脚本做：taint 挂在节点对象上，写不进 manifest.yaml（那是命名空间内对象的清单），
# 所以这个场景在 scenario.sh 的 setup 钩子里打 taint、在 teardown 钩子里摘掉。
# 键值要与 README 与 expect.json 的关键词（证据里出现的 taint 文本）保持一致。
# --overwrite 保证重复 up 幂等（taint 已存在时不会报错，只是刷新）。
set -euo pipefail

kubectl taint nodes --all diag-lab-scenario=taint-not-tolerated:NoSchedule --overwrite
