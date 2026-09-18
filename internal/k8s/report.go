package k8s

// 报告层：把诊断结论固化成"结构化 JSON + 人读 markdown"双产物（设计文档 §7）。
//
// 与证据包的分工：证据包是"看到的原始证据"（Evidence），报告是"据此得出的结论与置信度"
// （ReportView）。三者同目录同前缀（<ns>-<pod>.evidence.json / .report.json / .report.md），
// 便于配对回放与评测。
//
// 为什么结构与渲染放采集包而不是工具层：ReportView 与 PodView 等是同一类东西（对外数据的
// 形状），落盘入口 SaveReport 也在这里；工具层只解码转发（jjj §10.8 决策 1）。

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ReportSchemaVersion 报告结构版本：字段增减时递增，供回放与评测对齐口径。
// 2：target 增 api_server（2026-09-19，见 k8s-diagnosis.md §17.6）。
const ReportSchemaVersion = 2

// RootCauseCategory 一个根因类别：值（进报告 category 字段与 schema enum）与一行释义
// （进 schema 图例——strict 子集里 enum 没有逐值说明，含义只能写在 description 里）。
type RootCauseCategory struct {
	Value string
	Note  string
}

// rootCauseCategories 根因类别词表（唯一维护点，清单与取舍见 Zoo/temp/jjj.md §22）。
// 为什么把 category 收紧成词表：报告走的是"结构化 JSON 承载、不解析自由文本"的路线
// （设计文档 §5），类别是最后一个还靠模糊匹配的字段——固定取值既给模型明确的选择范围，
// 也让评测能精确比对（打分器只做字符串相等，不做同义词归并）。
// 分组顺序与设计文档 §7.2 的症状清单一致，便于人对着看。
var rootCauseCategories = []RootCauseCategory{
	// CrashLoopBackOff
	{"crashloop_app_exit", "应用自身启动即失败（非零退出码/panic/命令不存在）"},
	{"crashloop_missing_dependency", "依赖不可达（DB、缓存、下游、DNS）"},
	{"crashloop_config_error", "配置错误（非法 flag、必填项缺失、env 引用的 cm/secret 缺 key）"},
	{"crashloop_volume_mount_error", "卷挂载或权限问题（FailedMount、只读文件系统）"},
	// OOMKilled
	{"oom_limit_too_small", "limit 太小（用量贴限被杀，无长期增长趋势）"},
	{"oom_memory_leak", "内存持续增长（随重启次数单调上升）"},
	{"oom_heap_misconfig", "运行时堆配置超过 limit（JVM/Node 等）"},
	// ImagePullBackOff
	{"imagepull_name_invalid", "镜像名或仓库地址错"},
	{"imagepull_tag_missing", "tag 不存在（manifest unknown）"},
	{"imagepull_missing_secret", "私有仓库缺凭据（Pod spec 与 ServiceAccount 两处都没有）"},
	{"imagepull_registry_unreachable", "网络或 DNS 导致拉不到（timeout / no such host）"},
	// Pending
	{"pending_insufficient_resources", "节点余量不足（requests 超 allocatable）"},
	{"pending_node_selector", "nodeSelector 或 affinity 的 required 与节点标签不匹配"},
	{"pending_taint_not_tolerated", "节点 taint 未被 Pod 容忍"},
	{"pending_pvc_unbound", "PVC 未绑定（storageClass 不存在、供给失败、无可用 PV）"},
	{"pending_quota_exceeded", "命名空间配额或 LimitRange 拒绝创建"},
	// ContainerRestart（非 OOM）
	{"restart_probe_kill", "被 liveness 探针杀，但探针配置看不出具体错配（能看出端口/路径/时序错的，用 probe_* 类）"},
	{"restart_app_crash", "应用自身崩溃退出（非 OOM，由 restartPolicy 拉起）"},
	{"restart_unknown", "周期性重启但证据不足（无日志/无终态原因/无事件）"},
	// Probe Failed（端口/路径/时序这三类也覆盖 liveness 探针失败导致重启的情形：
	// 探针配置能指出具体错配时用具体类，restart_probe_kill 只作看不出错配时的兜底）
	{"probe_path_wrong", "探针路径错（HTTP 404）"},
	{"probe_port_wrong", "探针端口错（connection refused；liveness 因此杀容器也用它）"},
	{"probe_timing_too_short", "探针时序太短（initialDelay/period/timeout 低于启动耗时；liveness 因此杀容器也用它）"},
	// 兜底
	{"other", "证据不足以归类，或属于列表外原因——选它必须在 summary 里说明并列出缺的证据"},
}

// CategoryValuesJSON 把词表渲染成 JSON 数组字面量，供 k8s_report 的 schema 拼 enum。
// 放在采集包而不是工具层：词表只有这一处定义，工具层只管取用，避免两处各自漂移。
// 不用 json.Marshal 是为了免掉一个不可能发生的错误分支（值全是 ASCII 常量）。
func CategoryValuesJSON() string {
	var b strings.Builder
	b.WriteByte('[')
	for i, c := range rootCauseCategories {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(strconv.Quote(c.Value))
	}
	b.WriteByte(']')
	return b.String()
}

// CategoryLegend 渲染一行图例（"值=释义"），供 schema 的 description 用。
func CategoryLegend() string {
	parts := make([]string, 0, len(rootCauseCategories))
	for _, c := range rootCauseCategories {
		parts = append(parts, c.Value+"="+c.Note)
	}
	return strings.Join(parts, "；")
}

// IsRootCauseCategory 判断取值是否在词表内。工具层用它做参数校验：schema 的 enum 在非
// strict 模式下不保证生效，Go 侧再挡一道——把"自造类别"变成明确的参数错误（模型当轮就能改），
// 而不是让一个列表外的值静静落进报告，等到评测时才发现。
func IsRootCauseCategory(v string) bool {
	for _, c := range rootCauseCategories {
		if c.Value == v {
			return true
		}
	}
	return false
}

// ReportView 一次诊断的结论（设计文档 §7.3 全字段）。
// 数组元素一律用 object（不用 $ref/oneOf），与工具参数 schema 的 strict 子集保持一致。
type ReportView struct {
	SchemaVersion   int               `json:"schema_version"`
	GeneratedAt     string            `json:"generated_at"`
	Target          Target            `json:"target"`
	Symptoms        []string          `json:"symptoms"`
	RootCause       RootCauseView     `json:"root_cause"`
	Evidence        []EvidenceItem    `json:"evidence"`
	RuledOut        []RuledOutItem    `json:"ruled_out,omitempty"`
	MissingEvidence []MissingItem     `json:"missing_evidence,omitempty"`
	Suggestions     []SuggestionItem  `json:"suggestions"`
	Alternatives    []AlternativeItem `json:"alternatives,omitempty"`
}

// RootCauseView 根因结论：置信度是模型自评（0-1），工具层不做静默修正——
// 完备度规则写在 workflow 分支提示词里，报告里的数字必须与模型声明一致（jjj §10.8 决策 4）。
type RootCauseView struct {
	Summary string `json:"summary"`
	// Category 取值只能来自 rootCauseCategories 词表（工具层用它的 JSON 做 schema enum 约束），
	// 这样评测能精确比对，不用做同义词归并。
	Category   string  `json:"category,omitempty"`
	Confidence float64 `json:"confidence"`
}

// EvidenceItem 一条证据：必须能溯源（source + ref），excerpt 是原文片段而非复述。
type EvidenceItem struct {
	Source   string `json:"source"`   // 来源工具名，如 k8s_logs
	Ref      string `json:"ref"`      // 对象/字段/时间，如 badimg.spec.containers[0].resources.limits
	Excerpt  string `json:"excerpt"`  // 原文片段（日志一行 / 事件 message）
	Supports string `json:"supports"` // 这条证据支持什么判断
}

// RuledOutItem 被否定的候选根因：写出来才能让读者知道排查面cover到哪里。
type RuledOutItem struct {
	Summary string `json:"summary"`
	Reason  string `json:"reason"`
}

// MissingItem 缺失证据：缺哪项、影响什么判断——它也是"置信度为何不高"的交代。
type MissingItem struct {
	Item   string `json:"item"`
	Impact string `json:"impact"`
}

// SuggestionItem 建议动作：动作 + 理由（+ 风险，可选）。
type SuggestionItem struct {
	Action    string `json:"action"`
	Rationale string `json:"rationale,omitempty"`
	Risk      string `json:"risk,omitempty"`
}

// AlternativeItem 备选根因（按置信度排序的第二可能）。
type AlternativeItem struct {
	Summary    string  `json:"summary"`
	Confidence float64 `json:"confidence"`
}

// SaveReportView 补齐元信息后落盘 JSON 与 markdown，返回两个路径。
// 补的是"本该由系统给"的四项：schema_version、generated_at、target.context 与 target.api_server
// ——模型只负责 namespace/pod 与结论本身，"这份报告来自哪个集群的哪个端点"由采集器回答。
func (c *Collector) SaveReportView(r ReportView) (string, string, error) {
	if r.SchemaVersion == 0 {
		r.SchemaVersion = ReportSchemaVersion
	}
	if r.GeneratedAt == "" {
		r.GeneratedAt = time.Now().Format(time.RFC3339)
	}
	if r.Target.Context == "" {
		r.Target.Context = c.cfg.Context
	}
	// 端点地址与 context 一样属于"本该由系统给"的信息：模型不知道它，产物却要能回答"这份证据来自哪"。
	if r.Target.APIServer == "" {
		r.Target.APIServer = c.apiServer
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return "", "", fmt.Errorf("k8s: marshal report: %w", err)
	}
	return c.SaveReport(r.Target, data, RenderReportMD(r))
}

// RenderReportMD 按设计文档 §7.1 的模板渲染人读报告。
// 空小节直接省略（没有就整段不写，别用"无"占注意力）；根因与目标始终输出。
func RenderReportMD(r ReportView) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Root Cause: %s（置信度 %.0f%%）\n", r.RootCause.Summary, r.RootCause.Confidence*100)
	if r.RootCause.Category != "" {
		fmt.Fprintf(&b, "类别: %s\n", r.RootCause.Category)
	}
	fmt.Fprintf(&b, "Target: %s/%s", r.Target.Namespace, r.Target.Pod)
	if r.Target.Workload != "" {
		fmt.Fprintf(&b, "（%s）", r.Target.Workload)
	}
	// 端点地址要出现在人读报告里：只有 context 名看不出连的是哪个 IP（§17.6）。
	if r.Target.APIServer != "" {
		fmt.Fprintf(&b, " @ %s", r.Target.APIServer)
	}
	b.WriteString("\n")
	if len(r.Symptoms) > 0 {
		fmt.Fprintf(&b, "Symptoms: %s\n", strings.Join(r.Symptoms, ", "))
	}

	if len(r.Evidence) > 0 {
		b.WriteString("\nEvidence:\n")
		for i, e := range r.Evidence {
			fmt.Fprintf(&b, "%d. %s（来源：%s", i+1, e.Supports, e.Source)
			if e.Ref != "" {
				fmt.Fprintf(&b, " / %s", e.Ref)
			}
			b.WriteString("）\n")
			if e.Excerpt != "" {
				// 多行片段缩进对齐，保证 markdown 里读起来仍是一条证据。
				fmt.Fprintf(&b, "   %s\n", strings.ReplaceAll(strings.TrimSpace(e.Excerpt), "\n", "\n   "))
			}
		}
	}
	if len(r.RuledOut) > 0 {
		b.WriteString("\nRuled Out:\n")
		for _, x := range r.RuledOut {
			fmt.Fprintf(&b, "- %s：%s\n", x.Summary, x.Reason)
		}
	}
	if len(r.MissingEvidence) > 0 {
		b.WriteString("\nMissing Evidence:\n")
		for _, m := range r.MissingEvidence {
			fmt.Fprintf(&b, "- %s：%s\n", m.Item, m.Impact)
		}
	}
	if len(r.Suggestions) > 0 {
		b.WriteString("\nSuggestion:\n")
		for _, s := range r.Suggestions {
			fmt.Fprintf(&b, "- %s", s.Action)
			var extra []string
			if s.Rationale != "" {
				extra = append(extra, "理由："+s.Rationale)
			}
			if s.Risk != "" {
				extra = append(extra, "风险："+s.Risk)
			}
			if len(extra) > 0 {
				fmt.Fprintf(&b, "（%s）", strings.Join(extra, "；"))
			}
			b.WriteString("\n")
		}
	}
	if len(r.Alternatives) > 0 {
		b.WriteString("\nAlternatives:\n")
		for _, a := range r.Alternatives {
			fmt.Fprintf(&b, "- %s（%.0f%%）\n", a.Summary, a.Confidence*100)
		}
	}
	return b.String()
}
