#!/usr/bin/env bash
# 第 3 批场景集：端到端评测驱动（起场景 → 跑 /diag → 打分 → 收场景）。
#
#   用法: bash Zoo/k8s-lab/eval.sh [--timeout <秒>] [--keep] [场景名...]
#         bash Zoo/k8s-lab/eval.sh                              # 跑全部场景
#         bash Zoo/k8s-lab/eval.sh oom-limit-too-small          # 只跑指定的
#         bash Zoo/k8s-lab/eval.sh --keep crashloop-app-error    # 跑完不收场景（留着排查）
#   前置: minikube 已启动、DEEPSEEK_API_KEY 已导出（脚本只检查，不代你起也不代你配）：
#         minikube start && minikube addons enable metrics-server
#         export DEEPSEEK_API_KEY=...
#
# 为什么走子进程驱动 CLI 而不是在 Go 测试里装配 agent（jjj §21.5）：端到端就该验真入口路径，
# 也不必把组合根装配复制一份（复制品必然漂移）。
#
# 产物落 Zoo/k8s-lab/out/<run-id>/：每场景一份 <场景>.score.json 与 <场景>.cli.log（对话原文，
# 排障用），末尾由 cmd/k8seval 生成 summary.md 与总表。
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
HERE="$ROOT/Zoo/k8s-lab"
SCENARIOS="$HERE/scenarios"
RUN_ID="$(date +%Y%m%d-%H%M%S)"
OUT="$HERE/out/$RUN_ID"
TIMEOUT=900
KEEP=0

usage() {
  echo "用法: bash Zoo/k8s-lab/eval.sh [--timeout <秒>] [--keep] [场景名...]" >&2
}

# 参数解析：--timeout 带值、--keep 是开关，其余位置参数当场景名。
names=()
while [ $# -gt 0 ]; do
  case "$1" in
    --timeout) [ $# -ge 2 ] || { usage; exit 2; }; TIMEOUT="$2"; shift 2 ;;
    --keep) KEEP=1; shift ;;
    -h|--help) usage; exit 0 ;;
    -*) usage; exit 2 ;;
    *) names+=("$1"); shift ;;
  esac
done

need_prereq() {
  if ! kubectl cluster-info >/dev/null 2>&1; then
    echo "连不上集群。先执行：minikube start && minikube addons enable metrics-server" >&2
    exit 1
  fi
  # 评测要真调 LLM：密钥只在组合根读（环境变量），这里只做存在性检查，不打印也不落盘。
  if [ -z "${DEEPSEEK_API_KEY:-}" ]; then
    echo "未设置 DEEPSEEK_API_KEY（组合根从环境变量读，脚本不代你配）" >&2
    exit 1
  fi
}

# all_scenarios 从目录枚举场景（与 scenario.sh list 同源：有 expect.json 的目录才算）。
all_scenarios() {
  local d
  for d in "$SCENARIOS"/*/; do
    [ -f "$d/expect.json" ] || continue
    basename "$d"
  done
}

scenario_dir() {
  local dir="$SCENARIOS/$1"
  if [ ! -f "$dir/expect.json" ]; then
    echo "场景不存在: $1（期望 $dir/expect.json）" >&2
    return 1
  fi
  printf '%s' "$dir"
}

# k8s_dir 读 config.yml 里的 k8s_dir（组合根只认这个文件，缺省 ~/.small/k8s）。
# 为什么脚本要自己读：产物目录按"会话 id"分子目录，我们得按同一规则去捞 report.json；
# 写死 ~/.small/k8s 会把"改过配置的人"变成静默的 no_report。
k8s_dir() {
  local cfg="$HOME/.small/config.yml" v=""
  if [ -f "$cfg" ]; then
    v=$(grep -m1 -E '^k8s_dir:' "$cfg" | sed -E 's/^k8s_dir:[[:space:]]*//' | sed -E 's/[[:space:]]*#.*$//') || true
  fi
  v="${v%\"}"; v="${v#\"}"; v="${v%\'}"; v="${v#\'}"
  case "$v" in "~"*) v="$HOME${v#\~}" ;; esac
  printf '%s' "${v:-$HOME/.small/k8s}"
}

# first_match 取通配符命中的第一个文件（没有则空，不报错）。
first_match() {
  local f
  for f in $1; do
    [ -f "$f" ] && { printf '%s' "$f"; return 0; }
  done
  return 0
}

# run_cli 跑一个场景的诊断：/diag 命令进 k8s 分支，跑完再喂 exit（stdin 到 EOF 也会退出，
# 写 exit 只是让日志里有个明确的收尾）。输出全进 cli.log，stdout 只留状态行。
run_cli() {
  local sid="$1" target="$2" log="$3" bin="$4"
  if command -v timeout >/dev/null 2>&1; then
    printf '/diag %s\nexit\n' "$target" | timeout "$TIMEOUT" "$bin" --session "$sid" >"$log" 2>&1 || true
  else
    printf '/diag %s\nexit\n' "$target" | "$bin" --session "$sid" >"$log" 2>&1 || true
  fi
}

need_prereq
mkdir -p "$OUT"
if [ ${#names[@]} -eq 0 ]; then
  mapfile -t names < <(all_scenarios)
fi

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
# 一次编译、跑多个场景：go run 每个场景都要重编一遍，纯浪费。二进制放临时目录，
# 不进仓库（仓库里只留评测产物）。
echo "编译 CLI 与打分器…"
(cd "$ROOT" && go build -o "$TMP/small" . && go build -o "$TMP/k8seval" ./cmd/k8seval)

K8S_DIR="$(k8s_dir)"
skipped=()
for name in "${names[@]}"; do
  echo "=== $name ==="
  scenario_dir "$name" >/dev/null || { skipped+=("$name（无此场景）"); continue; }
  if ! bash "$HERE/scenario.sh" up "$name"; then
    # 场景没起来就不打分：把它记进"未参与"清单，不写 score.json（汇总只覆盖真正跑过的场景）。
    echo "！场景 $name 未就绪，跳过" >&2
    skipped+=("$name（未就绪）")
    [ "$KEEP" = 1 ] || bash "$HERE/scenario.sh" down "$name" >/dev/null 2>&1 || true
    continue
  fi
  target="$(bash "$HERE/scenario.sh" target "$name")"
  sid="eval-$RUN_ID-$name"
  art="$K8S_DIR/$sid"
  echo "  目标 $target（会话 $sid）"
  run_cli "$sid" "$target" "$OUT/$name.cli.log" "$TMP/small"

  # 产物文件名是 <ns>-<pod>.{report,evidence}.json（见 internal/k8s 的 Save）；
  # 用通配符而不是拼名字：Pod 名带 ReplicaSet hash，拼不出来。
  report="$(first_match "$art/*.report.json")"
  evidence="$(first_match "$art/*.evidence.json")"
  score_args=()
  [ -n "$report" ] && score_args+=(--report "$report")
  [ -n "$evidence" ] && score_args+=(--evidence "$evidence")
  "$TMP/k8seval" score --expect "$SCENARIOS/$name/expect.json" "${score_args[@]}" --out "$OUT"
  if [ -z "$report" ]; then
    echo "  （没找到 report.json，记 no_report；对话原文见 $OUT/$name.cli.log）" >&2
  fi
  if [ "$KEEP" = 1 ]; then
    echo "  --keep：保留场景 $name（对象仍在 diag-lab）"
  else
    bash "$HERE/scenario.sh" down "$name" >/dev/null
  fi
done

echo
"$TMP/k8seval" summary --out "$OUT"

if [ ${#skipped[@]} -gt 0 ]; then
  echo
  echo "未参与的场景（不在上面的汇总里）："
  printf '  - %s\n' "${skipped[@]}"
fi
echo
echo "本轮 run 目录：$OUT"
