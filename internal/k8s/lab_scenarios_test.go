//go:build k8slab

package k8s

// 第 3 批场景集的采集层断言（确定性、不涉 LLM、不进合并门槛；口径见 jjj §21.4）。
//
// 跑法：
//
//	bash Zoo/k8s-lab/scenario.sh up <name>    # 起单个场景；未起的场景是 Skip 不是失败
//	bash Zoo/k8s-lab/scenario.sh list         # 看有哪些场景
//	go test -tags k8slab ./internal/k8s/ -run TestScenarios -count=1 -v
//
// 与冒烟台（lab_test.go）的分工：冒烟台验"采集链路能不能跑通"，这里按场景验"该拿到的证据拿到了没有"。
// 每场景断言三类：症状字段对；该症状的关键证据在；notes 里没有 required_failed。

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// scenarioCase 一个场景的采集层断言（名字即 scenarios/ 下的目录名）。
// check 拿得到采集器：Pending 类场景的节点侧证据要另走 Nodes()（见 scenarioNodes）。
type scenarioCase struct {
	name  string
	check func(t *testing.T, c *Collector, ev *Evidence)
}

// TestScenarios 逐场景跑断言。集群或场景缺失一律 Skip（没起集群的人不该被这个 tag 卡住）。
func TestScenarios(t *testing.T) {
	c := labCollector(t)
	for _, sc := range scenarioCases {
		t.Run(sc.name, func(t *testing.T) {
			ev := collectScenario(t, c, sc.name)
			// 通用断言：必需来源全取到。缺了它，下面的断言都建在"证据不完整"的地基上，
			// 报出来的失败会指向错误的方向。
			if req := notesOfKind(ev.Notes, NoteRequired); len(req) > 0 {
				t.Errorf("必需来源失败: %v", req)
			}
			sc.check(t, c, ev)
		})
	}
}

// scenarioNodes 列全部节点（Pending 归因的证据面）：Pending 的 Pod 没有 node_name，
// 证据包里的 Node 恒为空，节点侧只能靠这条路径——它是候选节点标签与余量的唯一来源。
func scenarioNodes(t *testing.T, c *Collector) []NodeView {
	t.Helper()
	views, err := c.Nodes(context.Background())
	if err != nil {
		t.Fatalf("Nodes: %v", err)
	}
	if len(views) == 0 {
		t.Fatal("集群里没有任何节点")
	}
	return views
}

// scenarioSelector 从 expect.json 读目标 selector——标签只在 expect.json 里维护一份，
// 避免 Go 侧再抄一遍后两处漂移（go test 的工作目录就是本包目录，故上溯两级到仓库根）。
func scenarioSelector(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join("..", "..", "Zoo", "k8s-lab", "scenarios", name, "expect.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s: %v（工作目录应为 internal/k8s）", path, err)
	}
	var exp struct {
		Target struct {
			Selector string `json:"selector"`
		} `json:"target"`
	}
	if err := json.Unmarshal(raw, &exp); err != nil {
		t.Fatalf("解析 %s: %v", path, err)
	}
	if exp.Target.Selector == "" {
		t.Fatalf("%s 里没有 target.selector", path)
	}
	return exp.Target.Selector
}

// collectScenario 找到场景的 Pod 并采一次证据；没起就 Skip。
func collectScenario(t *testing.T, c *Collector, name string) *Evidence {
	t.Helper()
	ctx := context.Background()
	selector := scenarioSelector(t, name)
	list, err := c.core.CoreV1().Pods(labNamespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		t.Fatalf("list pods(%s): %v", selector, err)
	}
	if len(list.Items) == 0 {
		t.Skipf("跳过：场景未起，先跑 bash Zoo/k8s-lab/scenario.sh up %s", name)
	}
	ev, err := c.Collect(ctx, labNamespace, list.Items[0].Name)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	return ev
}

// --- 断言助手（场景都是单容器、单目标，取首项即可）---

// firstContainer 取采集视图里的第一个容器（缺 Pod/容器证据直接结束该子测试）。
func firstContainer(t *testing.T, ev *Evidence) ContainerView {
	t.Helper()
	if ev.Pod == nil || len(ev.Pod.Containers) == 0 {
		t.Fatalf("Pod/容器证据缺失: %+v", ev.Pod)
	}
	return ev.Pod.Containers[0]
}

// hasEvent 事件里是否有一条 message 或 reason 含 substr。
func hasEvent(ev *Evidence, substr string) bool {
	for _, e := range ev.Events {
		if strings.Contains(e.Message, substr) || strings.Contains(e.Reason, substr) {
			return true
		}
	}
	return false
}

// logText 拼某类日志（previous=true 取上次运行的，false 取当前）的全文，便于做子串断言。
func logText(ev *Evidence, previous bool) string {
	var b strings.Builder
	for _, l := range ev.Logs {
		if l.Previous == previous {
			b.WriteString(l.Text)
		}
	}
	return b.String()
}

// podCondition 取某个 Pod condition 的 status（不存在返回空）。
func podCondition(ev *Evidence, typ string) string {
	if ev.Pod == nil {
		return ""
	}
	for _, c := range ev.Pod.Conditions {
		if c.Type == typ {
			return c.Status
		}
	}
	return ""
}

// probeOf 取指定类型的探针配置（不存在返回 nil）。
func probeOf(ev *Evidence, typ string) *ProbeView {
	if ev.Pod == nil {
		return nil
	}
	for i := range ev.Pod.Containers {
		for j := range ev.Pod.Containers[i].Probes {
			if p := &ev.Pod.Containers[i].Probes[j]; p.Type == typ {
				return p
			}
		}
	}
	return nil
}

// scenarioCases 全部场景的断言（顺序与 Zoo/k8s-lab/scenarios 的 list 一致）。
var scenarioCases = []scenarioCase{
	{"crashloop-app-error", checkCrashLoopAppError},
	{"crashloop-missing-dependency", checkCrashLoopMissingDependency},
	{"crashloop-config-missing-env", checkCrashLoopConfigMissingEnv},
	{"oom-limit-too-small", checkOOMLimitTooSmall},
	{"oom-heap-misconfig", checkOOMHeapMisconfig},
	{"imagepull-tag-missing", checkImagePullTagMissing},
	{"imagepull-missing-secret", checkImagePullMissingSecret},
	{"pending-insufficient-resources", checkPendingInsufficientResources},
	{"pending-node-selector", checkPendingNodeSelector},
	{"pending-taint-not-tolerated", checkPendingTaintNotTolerated},
	{"pending-pvc-unbound", checkPendingPVCUnbound},
	{"restart-periodic-crash", checkRestartPeriodicCrash},
	{"restart-liveness-kill", checkRestartLivenessKill},
	{"probefailed-readiness-port", checkProbeFailedReadinessPort},
}

// checkCrashLoopAppError 应用启动即失败：重启计数与上次终止原因要在，且崩溃输出能读到。
// 两处不写死的理由（真集群首轮跑出来的）：
//   - 当前状态在"退避中"与"刚被杀"之间摆动（kubelet 的状态在两个 phase 之间切），只认 CrashLoopBackOff
//     会让断言随采样时刻红绿；
//   - minikube + containerd 下 previous 日志常常取不到（正文是 "unable to retrieve container logs"），
//     而那行 fatal 就在当前日志里——所以断言"两次日志里能看到"，不断言必须来自 previous。
func checkCrashLoopAppError(t *testing.T, _ *Collector, ev *Evidence) {
	ct := firstContainer(t, ev)
	if strings.Contains(ct.StateReason, "CrashLoop") || ct.StateReason == "Error" {
		// 正常形态：退避中或刚终止
	} else {
		t.Errorf("症状不对: state_reason=%q（期望 CrashLoopBackOff 或刚终止的 Error）", ct.StateReason)
	}
	if ct.RestartCount < 1 {
		t.Errorf("重启次数该 ≥1: %d", ct.RestartCount)
	}
	if ct.LastStateReason != "Error" {
		t.Errorf("上次终止原因不对: %q（期望 Error）", ct.LastStateReason)
	}
	if ct.LastStateExitCode == nil || *ct.LastStateExitCode != 1 {
		t.Errorf("上次退出码不对: %v（期望 1）", ct.LastStateExitCode)
	}
	if !strings.Contains(logText(ev, true)+logText(ev, false), "fatal") {
		t.Errorf("两次日志里都没有那行 fatal: %+v", ev.Logs)
	}
}

// checkCrashLoopMissingDependency 依赖不可达：日志里要有"哪个目标 + 哪种失败"。
// 不断言 wget 的具体文案（bad address / no such host 随 libc 与 busybox 版本变），
// 只要求出现目标主机名 + 一次解析/连接类报错——判别力就在这两点上。
func checkCrashLoopMissingDependency(t *testing.T, _ *Collector, ev *Evidence) {
	ct := firstContainer(t, ev)
	assertCrashing(t, ct)
	logs := strings.ToLower(logText(ev, true) + logText(ev, false))
	if !strings.Contains(logs, "db-svc") {
		t.Errorf("日志里没有目标主机名（判“依赖不可达”的凭据）: %+v", ev.Logs)
	}
	if !strings.Contains(logs, "bad address") && !strings.Contains(logs, "no such host") &&
		!strings.Contains(logs, "connection refused") && !strings.Contains(logs, "timed out") {
		t.Errorf("日志里没有解析/连接类报错: %+v", ev.Logs)
	}
}

// checkCrashLoopConfigMissingEnv 必填配置缺失：日志说要什么，且 env 证据面能看出"一个都没配"。
// 后者是这条场景的额外价值——模型得能从证据里看出"缺配置"，而不是猜某个 configMap 有问题。
func checkCrashLoopConfigMissingEnv(t *testing.T, _ *Collector, ev *Evidence) {
	ct := firstContainer(t, ev)
	assertCrashing(t, ct)
	if !strings.Contains(strings.ToLower(logText(ev, true)+logText(ev, false)), "missing required env") {
		t.Errorf("日志里没有缺配置那行: %+v", ev.Logs)
	}
	if len(ct.Env) != 0 {
		t.Errorf("本场景 spec 里不该有 env（有值就说明场景构造错了）: %+v", ct.Env)
	}
}

// assertCrashing 崩溃类场景的公共形态：状态在退避与刚终止之间摆动、上次是 Error、退出码非 0。
func assertCrashing(t *testing.T, ct ContainerView) {
	t.Helper()
	if !strings.Contains(ct.StateReason, "CrashLoop") && ct.StateReason != "Error" {
		t.Errorf("症状不对: state_reason=%q（期望 CrashLoopBackOff 或刚终止的 Error）", ct.StateReason)
	}
	if ct.LastStateReason != "Error" {
		t.Errorf("上次终止原因不对: %q（期望 Error）", ct.LastStateReason)
	}
	if ct.LastStateExitCode == nil || *ct.LastStateExitCode == 0 {
		t.Errorf("上次退出码不对: %v（期望非 0）", exitCodeText(ct.LastStateExitCode))
	}
}

// checkRestartPeriodicCrash 应用自身周期崩溃：重启计数在涨、上次异常终止，且没有探针。
// 三处断言各有分工（都是真集群跑出来的）：
//   - "没有探针配置 + 事件里没有探针失败"：与 restart-liveness-kill 的唯一分界；
//   - 上次终止原因非 OOMKilled：排除 OOM 那条；
//   - 上次运行时长 ≥ 20s（startedAt→finishedAt）：与"启动即退"的 crashloop 分开——这条场景的
//     本质是"先正常服务再崩"。
func checkRestartPeriodicCrash(t *testing.T, _ *Collector, ev *Evidence) {
	ct := firstContainer(t, ev)
	if ct.RestartCount < 2 {
		t.Errorf("重启次数不对: %d（期望 ≥2，就绪条件就是它）", ct.RestartCount)
	}
	switch ct.LastStateReason {
	case "":
		t.Errorf("上次终止原因缺失（没有它就分不清自崩与探针杀）")
	case "OOMKilled":
		t.Errorf("上次终止原因成了 OOMKilled，场景本身构造错了")
	}
	if ct.LastStateExitCode == nil || *ct.LastStateExitCode == 0 {
		t.Errorf("上次退出码不对: %v（期望非 0）", exitCodeText(ct.LastStateExitCode))
	}
	if run := lastRunDuration(ct); run < 20*time.Second {
		t.Errorf("上次运行时长 %v，期望 ≥20s（先服务再崩，不是启动即退）", run)
	}
	if len(ct.Probes) != 0 {
		t.Errorf("本场景不该配探针: %+v", ct.Probes)
	}
	for _, e := range ev.Events {
		if strings.Contains(strings.ToLower(e.Message+e.Reason), "probe") {
			t.Errorf("事件里出现了探针相关记录（那说明场景构造错了）: %+v", e)
		}
	}
}

// lastRunDuration 上次运行的时长（startedAt→finishedAt）；任一时刻缺失或不合法时返回 0。
func lastRunDuration(ct ContainerView) time.Duration {
	start, err1 := time.Parse(time.RFC3339, ct.LastStateStartedAt)
	end, err2 := time.Parse(time.RFC3339, ct.LastStateFinishedAt)
	if err1 != nil || err2 != nil {
		return 0
	}
	return end.Sub(start)
}

// exitCodeText 打印退出码（指针对直接打是地址，调试时看不出值）。
func exitCodeText(p *int32) string {
	if p == nil {
		return "nil"
	}
	return strconv.Itoa(int(*p))
}

// checkOOMLimitTooSmall 内存超限被杀：上次终止原因是 OOMKilled（137），且模板里的 memory limit
// 能读到——判"limit 太小"必须拿它跟用量比，只看 Pod 上的值看不出是它配错了。
func checkOOMLimitTooSmall(t *testing.T, _ *Collector, ev *Evidence) {
	ct := firstContainer(t, ev)
	if ct.LastStateReason != "OOMKilled" {
		t.Errorf("症状不对: last_state_reason=%q（期望 OOMKilled）", ct.LastStateReason)
	}
	if ct.LastStateExitCode == nil || *ct.LastStateExitCode != 137 {
		t.Errorf("上次退出码不对: %v（期望 137）", ct.LastStateExitCode)
	}
	if ev.Workload == nil || len(ev.Workload.Template.Containers) == 0 {
		t.Fatalf("工作负载模板缺失（limits 只能从这里看）: %+v", ev.Workload)
	}
	if got := ev.Workload.Template.Containers[0].Limits["memory"]; got != "64Mi" {
		t.Errorf("模板 memory limit 不对: %q（期望 64Mi）", got)
	}
}

// checkOOMHeapMisconfig JVM 堆配置超过 limit：被杀原因与退出码只说明症状，判据是"命令行里的
// 堆上限超过容器 limit"这条配置矛盾——与 oom-limit-too-small（无堆配置、用量贴 limit）的分界在这里。
func checkOOMHeapMisconfig(t *testing.T, _ *Collector, ev *Evidence) {
	ct := firstContainer(t, ev)
	if ct.LastStateReason != "OOMKilled" {
		t.Errorf("症状不对: last_state_reason=%q（期望 OOMKilled）", ct.LastStateReason)
	}
	if ct.LastStateExitCode == nil || *ct.LastStateExitCode != 137 {
		t.Errorf("上次退出码不对: %v（期望 137）", ct.LastStateExitCode)
	}
	if cmd := strings.Join(ct.Command, " "); !strings.Contains(cmd, "-Xmx256m") {
		t.Errorf("容器命令行里没有堆配置（本场景判据的来源）: %v", ct.Command)
	}
	if got := ct.Limits["memory"]; got != "128Mi" {
		t.Errorf("memory limit 不对: %q（期望 128Mi）", got)
	}
}

// checkImagePullTagMissing 拉镜像失败：症状字段与报错原文要在，日志该是空（容器从未启动）。
// 不断言报错的具体文案（随 CRI/registry 变），只断言它含请求的镜像引用、且不含凭据类字样——
// 后者正是本场景与"缺 secret"的分界（凭据问题会带 pull access denied / unauthorized）。
func checkImagePullTagMissing(t *testing.T, _ *Collector, ev *Evidence) {
	ct := firstContainer(t, ev)
	if !strings.Contains(ct.StateReason, "ImagePull") {
		t.Errorf("症状不对: state_reason=%q（期望 ImagePullBackOff 或 ErrImagePull）", ct.StateReason)
	}
	if !strings.Contains(ct.StateMessage, "busybox:1.36-nope") {
		t.Errorf("报错原文里没有请求的镜像引用: %q", ct.StateMessage)
	}
	low := strings.ToLower(ct.StateMessage)
	if strings.Contains(low, "pull access denied") || strings.Contains(low, "unauthorized") {
		t.Errorf("报错是凭据类的，本场景没配凭据但失败原因应是 tag 不存在: %q", ct.StateMessage)
	}
	if len(ev.Logs) != 0 {
		t.Errorf("容器未启动，不该有日志: %+v", ev.Logs)
	}
}

// checkImagePullMissingSecret 私有仓库缺凭据：报错要是鉴权失败（403 / authorize），
// 且 Pod spec 与 ServiceAccount 两处都没有可用凭据——两处都空才叫"缺 secret"，
// 只断言报错文案会把"配了却用不上的 secret"也放进来（那是另一类问题）。
func checkImagePullMissingSecret(t *testing.T, _ *Collector, ev *Evidence) {
	ct := firstContainer(t, ev)
	if !strings.Contains(ct.StateReason, "ImagePull") {
		t.Errorf("症状不对: state_reason=%q（期望 ImagePullBackOff 或 ErrImagePull）", ct.StateReason)
	}
	msg := ct.StateMessage
	if !strings.Contains(msg, "403") && !strings.Contains(msg, "authorize") {
		t.Errorf("报错里没有鉴权失败字样（本场景区别于 tag/name 类的唯一依据）: %q", msg)
	}
	if ev.Pod == nil {
		t.Fatalf("Pod 证据缺失")
	}
	if len(ev.Pod.ImagePullSecrets) != 0 || len(ev.Pod.ServiceAccountPullSecrets) != 0 {
		t.Errorf("凭据两处该都是空的，场景本身构造错了: spec=%v sa=%v",
			ev.Pod.ImagePullSecrets, ev.Pod.ServiceAccountPullSecrets)
	}
}

// checkPendingInsufficientResources 资源不足：Pod 卡 Pending、requests 大于节点可分配、
// 事件里是 Insufficient。注意 Pending 的 Pod 没有 node_name，证据包里 Node 恒为空——
// 节点侧的数字（free_on_node 等）只能靠 Nodes() 列节点拿到，这条一起验掉。
func checkPendingInsufficientResources(t *testing.T, c *Collector, ev *Evidence) {
	if ev.Pod == nil || ev.Pod.Phase != "Pending" {
		t.Fatalf("phase 不对: %+v", ev.Pod)
	}
	if ev.Pod.NodeName != "" {
		t.Errorf("Pending 的 Pod 不该有 node_name: %q", ev.Pod.NodeName)
	}
	if got := firstContainer(t, ev).Requests["cpu"]; got != "100" {
		t.Errorf("Pod requests.cpu 不对: %q（期望 100）", got)
	}
	if !hasEvent(ev, "Insufficient") {
		t.Errorf("事件里没有 Insufficient（调度失败的关键证据）: %+v", ev.Events)
	}
	// 节点侧：余量必须能自己核（"requests 超可分配"不能只靠事件里的半句话）
	var freeFound bool
	for _, n := range scenarioNodes(t, c) {
		if n.FreeOnNode["cpu"] != "" {
			freeFound = true
		}
	}
	if !freeFound {
		t.Errorf("列出的节点里没有 free_on_node（Pending 归因的分母）")
	}
}

// checkPendingNodeSelector 调度约束不匹配：Pod spec 的 nodeSelector 要读得到，
// 节点侧要能证实"没有任何节点带这个标签"，事件里是 affinity/selector 不匹配而非资源不足。
func checkPendingNodeSelector(t *testing.T, c *Collector, ev *Evidence) {
	if ev.Pod == nil || ev.Pod.Phase != "Pending" {
		t.Fatalf("phase 不对: %+v", ev.Pod)
	}
	if got := ev.Pod.NodeSelector["lab-node-role"]; got != "does-not-exist" {
		t.Errorf("Pod spec 的 nodeSelector 不对: %v", ev.Pod.NodeSelector)
	}
	// 节点侧：标签是这条根因的判据，采集必须把节点标签原样给出来（自定义键不在白名单里就无从证实）
	for _, n := range scenarioNodes(t, c) {
		if v, ok := n.Labels["lab-node-role"]; ok {
			t.Errorf("节点 %s 竟然带 lab-node-role=%s，场景本身构造错了", n.Name, v)
		}
	}
	if !hasEvent(ev, "didn't match") {
		t.Errorf("事件里没有 selector 不匹配（本场景区别于资源不足的唯一依据）: %+v", ev.Events)
	}
	if hasEvent(ev, "Insufficient") {
		t.Errorf("事件里出现了 Insufficient，场景本身构造错了: %+v", ev.Events)
	}
}

// checkPendingTaintNotTolerated 节点 taint 未被容忍：节点侧要有 taints、Pod 侧不能容忍它，
// 事件是 untolerated taint——这三条一起才算把"taint 分支"与另外两个 Pending 分支分开。
// 注意不能断言"Pod 的 tolerations 为空"：API server 会给每个 Pod 注入 not-ready / unreachable
// 两条 NoExecute 默认容忍（tolerationSeconds=300），真正要证伪的是"容忍了本场景那把 taint"。
func checkPendingTaintNotTolerated(t *testing.T, c *Collector, ev *Evidence) {
	if ev.Pod == nil || ev.Pod.Phase != "Pending" {
		t.Fatalf("phase 不对: %+v", ev.Pod)
	}
	for _, tol := range ev.Pod.Tolerations {
		if strings.HasPrefix(tol, "diag-lab-scenario") {
			t.Errorf("Pod 容忍了本场景的 taint，场景本身构造错了: %q", tol)
		}
	}
	found := false
	for _, n := range scenarioNodes(t, c) {
		for _, tt := range n.Taints {
			if strings.HasPrefix(tt, "diag-lab-scenario") {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("没有节点带 diag-lab-scenario taint（先跑 setup.sh，或它被别的场景的 teardown 摘掉了）")
	}
	// 本版调度器的事件文案是 "1 node(s) had untolerated taint(s)"——不带 taint 键，
	// 所以"是哪把 taint"只能从节点侧证据看出来（判分关键词也因此要落在节点 taint 上）。
	if !hasEvent(ev, "untolerated taint") {
		t.Errorf("事件里没有 untolerated taint: %+v", ev.Events)
	}
	if hasEvent(ev, "Insufficient") {
		t.Errorf("事件里出现了 Insufficient，场景本身构造错了: %+v", ev.Events)
	}
}

// checkPendingPVCUnbound 存储未就绪：claim 的 phase 与 storageClass 要在（判"类不存在"的判据），
// Pod 侧事件是 unbound PVC。这条覆盖的是 §4.1 的子对象采集链（PVC 自己的事件不在 Pod 的事件里）。
func checkPendingPVCUnbound(t *testing.T, _ *Collector, ev *Evidence) {
	if ev.Pod == nil || ev.Pod.Phase != "Pending" {
		t.Fatalf("phase 不对: %+v", ev.Pod)
	}
	if len(ev.PVCs) == 0 {
		t.Fatalf("PVC 证据缺失（Pod 引用的卷没被采到）: volumes=%+v", ev.Pod.Volumes)
	}
	pvc := ev.PVCs[0]
	if pvc.Name != "lab-pending-claim" {
		t.Errorf("claim 名不对: %q", pvc.Name)
	}
	if pvc.Phase != "Pending" {
		t.Errorf("claim phase 不对: %q（期望 Pending）", pvc.Phase)
	}
	if pvc.StorageClass != "lab-no-such-sc" {
		t.Errorf("claim storageClass 不对: %q（期望 lab-no-such-sc）", pvc.StorageClass)
	}
	if !hasEvent(ev, "unbound") {
		t.Errorf("事件里没有 unbound PVC: %+v", ev.Events)
	}
}

// checkRestartLivenessKill 探针杀的：重启次数要涨，上次终止不是 OOMKilled（137 同码的坑），
// 事件里明写 liveness 失败。这条与 oom-limit-too-small 成对，专门盯"只看退出码就下结论"。
func checkRestartLivenessKill(t *testing.T, _ *Collector, ev *Evidence) {
	ct := firstContainer(t, ev)
	if ct.RestartCount < 2 {
		t.Errorf("重启次数不对: %d（期望 ≥2，就绪条件就是它）", ct.RestartCount)
	}
	switch ct.LastStateReason {
	case "":
		t.Errorf("上次终止原因缺失（没有它就分不清探针杀与应用崩）")
	case "OOMKilled":
		t.Errorf("上次终止原因成了 OOMKilled，场景本身构造错了（liveness 杀应是 Error + 非 0 退出码）")
	}
	if ct.LastStateExitCode == nil || *ct.LastStateExitCode == 0 {
		t.Errorf("上次退出码不对: %v（探针杀应有非 0 退出码）", ct.LastStateExitCode)
	}
	if !hasEvent(ev, "Liveness probe failed") {
		t.Errorf("事件里没有 liveness 失败: %+v", ev.Events)
	}
}

// checkProbeFailedReadinessPort 探针不可用但不重启：Running + 不 Ready + 零重启，
// 且探针配置里的端口（8081）与应用实际监听的端口（8080，在 args 里）都能读到——两个数字
// 对不上才是这个根因，只说"探针失败"没定位。
func checkProbeFailedReadinessPort(t *testing.T, _ *Collector, ev *Evidence) {
	ct := firstContainer(t, ev)
	if ev.Pod.Phase != "Running" {
		t.Errorf("phase 不对: %q（期望 Running）", ev.Pod.Phase)
	}
	if got := podCondition(ev, "Ready"); got != "False" {
		t.Errorf("Pod Ready 条件不对: %q（期望 False）", got)
	}
	if ct.RestartCount != 0 {
		t.Errorf("重启次数不对: %d（readiness 失败不该触发重启）", ct.RestartCount)
	}
	p := probeOf(ev, "readiness")
	if p == nil {
		t.Fatalf("探针配置缺失: %+v", ct.Probes)
	}
	if !strings.Contains(p.Handler, "8081") {
		t.Errorf("探针端口不对: %q（期望指向 8081）", p.Handler)
	}
	if !strings.Contains(strings.Join(ct.Args, " "), "8080") {
		t.Errorf("容器实际监听的端口读不到: %+v（判端口错要拿它与探针端口对）", ct.Args)
	}
}
