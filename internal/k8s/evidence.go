package k8s

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// Collect 组装证据包（Context Builder）。Pod 是主目标——拉不到即失败，其余证据都依赖它；
// 其余来源逐个独立采集，任一来源失败只记 Note 不中断：降级必须显式可见，模型据此把
// 缺失项写进 Missing Evidence 并压低置信度（设计文档 §4.4）。
func (c *Collector) Collect(ctx context.Context, ns, pod string) (*Evidence, error) {
	ev := &Evidence{
		CollectedAt: time.Now().Format(time.RFC3339),
		// Context/APIServer 取归一化后的值（New 已把 kubeconfig 的 current-context 与 server 补进来，
		// 见 Zoo/model/k8s-diagnosis.md §16.3、§17.6）：回放时要能分辨这份证据来自哪个集群的哪个端点。
		Target: Target{APIServer: c.apiServer, Context: c.cfg.Context, Namespace: ns, Pod: pod},
	}
	pv, err := c.Pod(ctx, ns, pod)
	if err != nil {
		return nil, err
	}
	ev.Pod = pv

	if w, err := c.WorkloadOf(ctx, ns, pod); err != nil {
		// 裸 Pod（无 controller owner）本来就没有上层工作负载这一层：Pod spec 里已有全部规格，
		// 缺的只是"模板真源"这个视角。这属于"来源正常但没有数据"，记成 required_failed 会让模型
		// 凭空写一条 missing_evidence 并压低置信度（真集群首轮跑场景集时踩到，见 jjj §30）。
		if !hasControllerOwner(pv) {
			ev.note(NoteNoData, "该 Pod 无 controller owner（裸 Pod），没有上层工作负载可读")
		} else {
			ev.note(NoteRequired, "工作负载规格未取到: %s", ExplainError(err))
		}
	} else {
		ev.Workload = w
		ev.Target.Workload = w.Kind + "/" + w.Name
	}

	if events, err := c.Events(ctx, ns, pod, pv.UID, 0, 0); err != nil {
		ev.note(NoteRequired, "事件未取到: %s", ExplainError(err))
	} else {
		ev.Events = events
	}

	// 子对象（PVC）：Pod 侧只看得到"我引用了哪块 claim"，绑没绑上、为什么没绑上都在 claim 自己身上。
	collectPVCs(ctx, c, ev, pv)

	for _, q := range LogTargets(pv) {
		lv, err := c.Logs(ctx, ns, pod, q)
		if err != nil {
			ev.note(logFailureKind(err), "日志未取到（容器 %s，previous=%v）: %s", q.Container, q.Previous, ExplainError(err))
			continue
		}
		ev.Logs = append(ev.Logs, *lv)
	}
	if len(ev.Logs) == 0 {
		ev.note(NoteNoData, "无可用日志（容器可能尚未启动，或日志不可读）")
	}

	if m, err := c.PodMetrics(ctx, ns, pod); err != nil {
		ev.note(NoteOptional, "Pod 指标未取到: %s", ExplainError(err))
	} else {
		ev.Metrics = m
	}

	if pv.NodeName != "" {
		if n, err := c.Node(ctx, pv.NodeName); err != nil {
			ev.note(NoteRequired, "节点信息未取到（%s）: %s", pv.NodeName, ExplainError(err))
		} else {
			// 节点分配汇总失败时，Node 自身摘要仍有效，但余量不可信——单独记一条降级说明。
			if n.AllocationError != "" {
				ev.note(NoteRequired, "节点分配汇总失败（%s）: %s", pv.NodeName, n.AllocationError)
			}
			ev.Node = n
		}
		if nm, err := c.NodeMetrics(ctx, pv.NodeName); err != nil {
			ev.note(NoteOptional, "节点指标未取到（%s）: %s", pv.NodeName, ExplainError(err))
		} else {
			ev.NodeMetrics = nm
		}
	}

	c.fitBudget(ev)
	return ev, nil
}

// collectPVCs 采集 Pod 引用的存储申请及其子对象事件。逐个独立采集，任一失败只记 Note：
// 卷引用了一块取不到的 claim，本身就是"Pending/挂载失败"的证据，不能静默跳过。
func collectPVCs(ctx context.Context, c *Collector, ev *Evidence, pv *PodView) {
	for _, ref := range pvcRefs(pv.Volumes) {
		pvc, err := c.PVC(ctx, pv.Namespace, ref.Name)
		if err != nil {
			ev.note(NoteRequired, "PVC %s 未取到: %s", ref.Name, ExplainError(err))
			continue
		}
		pvc.UsedByVolumes = ref.Volumes
		// 子对象事件必须按 claim 自己的 uid 再取一次：服务端只按 involvedObject 过滤，
		// Pod 的事件里不含 PVC 的进展（"waiting for first consumer"/"provisioning failed"）。
		if events, err := c.Events(ctx, pv.Namespace, ref.Name, pvc.UID, 0, 0); err != nil {
			pvc.EventsError = ExplainError(err)
		} else {
			pvc.Events = events
		}
		ev.PVCs = append(ev.PVCs, *pvc)
	}
}

// pvcRef 一处卷引用：claim 名 + 引用它的卷名（同一块 claim 可被多个卷复用，如挂两处）。
type pvcRef struct {
	Name    string
	Volumes []string
}

// pvcRefs 从 Pod 的卷列表挑出 persistentVolumeClaim 引用，按首次出现顺序聚合成 slice。
// 用 slice 而非 map 承载结果是有意的：证据包是给模型读的，顺序不稳定会让同一现场
// 每次输出都不一样——排查时难以对照两次采集，评测也无法固定基线。
func pvcRefs(vols []VolumeView) []pvcRef {
	const prefix = "persistentVolumeClaim/"
	idx := map[string]int{}
	var out []pvcRef
	for _, v := range vols {
		name := strings.TrimPrefix(v.Source, prefix)
		if name == "" || name == v.Source {
			continue
		}
		i, ok := idx[name]
		if !ok {
			i = len(out)
			idx[name] = i
			out = append(out, pvcRef{Name: name})
		}
		out[i].Volumes = append(out[i].Volumes, v.Name)
	}
	return out
}

// JSON 把证据包序列化成回灌文本（工具层用）。
func (ev *Evidence) JSON() (string, error) {
	data, err := json.MarshalIndent(ev, "", "  ")
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// hasControllerOwner Pod 是否由控制器管理。为什么要问它：工作负载取不到有两种完全不同的原因——
// 裸 Pod 本来就没有这一层（no_data），与"有 owner 但读不到"（required_failed）。混成一种
// 会让模型把"这个对象没有这一层"当成"证据缺失"，凭空写 missing_evidence 并压低置信度。
func hasControllerOwner(pv *PodView) bool {
	for _, r := range pv.OwnerRefs {
		if r.Controller {
			return true
		}
	}
	return false
}

// note 追加一条降级说明（采集期的事实记录，不是错误），类别决定它算不算"证据缺失"。
// 传错误时统一用 %s + ExplainError(err)：集群在采集过程中断开的话，notes 里也该是
// "集群不可达"这个结论，而不是让模型自己去读 dial 文本（§17.1 的动机换成 Notes 通道同样成立）。
func (ev *Evidence) note(kind NoteKind, format string, args ...any) {
	ev.Notes = append(ev.Notes, NoteView{Kind: kind, Message: fmt.Sprintf(format, args...)})
}

// logFailureKind 给日志读取失败定性。400 表示"这个容器的日志本来就不存在"
// （容器尚未启动 / 没有上一次运行——kubelet 用 BadRequest 回这种请求），属于 NoData；
// 其余（无权限、超时、连接失败）才是真失败。两者混谈的代价：ImagePullBackOff 这类
// "压根没有日志"的场景会凭空多出一条"缺失证据"，把模型的缺失清单与置信度带偏。
func logFailureKind(err error) NoteKind {
	if apierrors.IsBadRequest(err) {
		return NoteNoData
	}
	return NoteRequired
}

// fitBudget 按预算裁剪证据包。优先级：Pod 现状 → 工作负载 → 事件 → 日志 → 指标 → 节点；
// 超预算时从优先级最低处开始砍（"症状与规格"这类不可替代证据一定留着）。
// 每次实际裁剪都记 Note——被预算裁掉的证据与"本来就没有"是两回事，模型必须能分辨。
func (c *Collector) fitBudget(ev *Evidence) {
	budget := c.cfg.EvidenceMaxTokens
	if budget <= 0 {
		return
	}
	steps := []struct {
		note string
		drop func() bool
	}{
		{"节点指标因预算不足被裁掉", func() bool {
			if ev.NodeMetrics == nil {
				return false
			}
			ev.NodeMetrics = nil
			return true
		}},
		{"节点信息因预算不足被裁掉", func() bool {
			if ev.Node == nil {
				return false
			}
			ev.Node = nil
			return true
		}},
		{"Pod 指标因预算不足被裁掉", func() bool {
			if ev.Metrics == nil {
				return false
			}
			ev.Metrics = nil
			return true
		}},
		{"日志因预算不足被缩短（保留尾部）", func() bool { return shrinkLogs(ev) }},
		{"事件因预算不足被裁剪（保留 Warning 与最新）", func() bool { return shrinkEvents(ev) }},
	}
	for _, s := range steps {
		if evidenceTokens(ev) <= budget {
			return
		}
		applied := false
		for evidenceTokens(ev) > budget {
			if !s.drop() {
				break
			}
			applied = true
		}
		if applied {
			ev.note(NoteTrimmed, "%s", s.note)
		}
	}
}

// evidenceTokens 估算证据包的 token 量（按序列化后的字符数近似，见 estimateTokens）。
func evidenceTokens(ev *Evidence) int {
	data, err := json.Marshal(ev)
	if err != nil {
		return 0
	}
	return estimateTokens(string(data))
}

// shrinkLogs 逐步缩短日志：先把超长日志砍半（保留尾部）；都短了以后先丢"当前"日志
// （previous 的现场价值更高），最后才丢 previous。返回是否发生了缩减。
func shrinkLogs(ev *Evidence) bool {
	for i := range ev.Logs {
		if len(ev.Logs[i].Text) > 2048 {
			ev.Logs[i].Text = "（日志超长已截断，以下为尾部片段）\n" + tail(ev.Logs[i].Text, len(ev.Logs[i].Text)/2)
			ev.Logs[i].Truncated = true
			ev.Logs[i].Lines = countLines(ev.Logs[i].Text)
			return true
		}
	}
	for i := range ev.Logs {
		if !ev.Logs[i].Previous {
			ev.Logs = append(ev.Logs[:i], ev.Logs[i+1:]...)
			return true
		}
	}
	if len(ev.Logs) > 0 {
		ev.Logs = ev.Logs[1:]
		return true
	}
	return false
}

// shrinkEvents 逐步裁剪事件：先丢最早的非 Warning，全剩 Warning 时丢最旧的一条。
func shrinkEvents(ev *Evidence) bool {
	if len(ev.Events) <= 1 {
		return false
	}
	for i := len(ev.Events) - 1; i >= 0; i-- {
		if ev.Events[i].Type != "Warning" {
			ev.Events = append(ev.Events[:i], ev.Events[i+1:]...)
			return true
		}
	}
	ev.Events = ev.Events[:len(ev.Events)-1]
	return true
}

// tail 取字符串尾部 n 字节并对齐到行首（避免半行开头）。
func tail(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	s = s[len(s)-n:]
	if i := indexNewline(s); i >= 0 && i < len(s)-1 {
		s = s[i+1:]
	}
	return s
}

// indexNewline 找第一个换行位置（无则 -1）。
func indexNewline(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			return i
		}
	}
	return -1
}

// Save 把证据包落盘为 <ns>-<pod>.evidence.json。Dir 是本次运行的产物目录
// （组合根按会话 id 建，如 ~/.small/k8s/<sid>）；同一会话内重复诊断同一 Pod 以最新一次为准。
func (c *Collector) Save(ev *Evidence) (string, error) {
	data, err := json.MarshalIndent(ev, "", "  ")
	if err != nil {
		return "", err
	}
	return c.write(ev.Target.Namespace, ev.Target.Pod, "evidence.json", data)
}

// SaveReport 落盘报告：结构化 JSON（机器用/评测用）+ 渲染后的 markdown（人读），
// 与证据包同目录同前缀，便于按目标对齐查看。返回两个路径。
func (c *Collector) SaveReport(t Target, reportJSON []byte, reportMD string) (string, string, error) {
	jp, err := c.write(t.Namespace, t.Pod, "report.json", reportJSON)
	if err != nil {
		return "", "", err
	}
	mp, err := c.write(t.Namespace, t.Pod, "report.md", []byte(reportMD))
	if err != nil {
		return jp, "", err
	}
	return jp, mp, nil
}

// write 落盘公共路径：文件名片段经 safeName 清洗（目标名来自用户输入，防路径注入），
// 目录不存在则创建。
func (c *Collector) write(ns, pod, suffix string, data []byte) (string, error) {
	if c.cfg.Dir == "" {
		return "", fmt.Errorf("k8s: 产物目录未配置（Config.Dir）")
	}
	if err := os.MkdirAll(c.cfg.Dir, 0o755); err != nil {
		return "", fmt.Errorf("k8s: 建产物目录 %s: %w", c.cfg.Dir, err)
	}
	path := filepath.Join(c.cfg.Dir, safeName(ns)+"-"+safeName(pod)+"."+suffix)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", fmt.Errorf("k8s: 写产物 %s: %w", path, err)
	}
	return path, nil
}
