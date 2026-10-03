package k8s

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// 报告层单测：渲染是纯函数（不触达集群），落盘走临时目录。

// TestRootCauseCategories 验证词表契约：取值唯一且命名规范、图例覆盖全量、
// enum 字面量是合法 JSON（工具层直接把它拼进 schema，格式错会让工具注册就坏），
// 且校验函数与词表同源（含空串与带空格的自造值一律拒绝）。
func TestRootCauseCategories(t *testing.T) {
	if len(rootCauseCategories) == 0 {
		t.Fatal("词表为空")
	}
	seen := map[string]bool{}
	pattern := regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	for _, c := range rootCauseCategories {
		if !pattern.MatchString(c.Value) {
			t.Errorf("取值 %q 不符 snake_case（评测要精确比对，不接受中英混排）", c.Value)
		}
		if seen[c.Value] {
			t.Errorf("取值重复: %q", c.Value)
		}
		seen[c.Value] = true
		if c.Note == "" {
			t.Errorf("取值 %q 缺释义（schema 图例要靠它）", c.Value)
		}
		if !strings.Contains(CategoryLegend(), c.Value+"=") {
			t.Errorf("图例缺 %q", c.Value)
		}
	}
	if !seen["other"] {
		t.Error("词表必须有兜底取值 other")
	}

	var got []string
	if err := json.Unmarshal([]byte(CategoryValuesJSON()), &got); err != nil {
		t.Fatalf("CategoryValuesJSON 不是合法 JSON: %v（%s）", err, CategoryValuesJSON())
	}
	if len(got) != len(rootCauseCategories) {
		t.Fatalf("enum 项数 %d 与词表 %d 不一致", len(got), len(rootCauseCategories))
	}
	for i, c := range rootCauseCategories {
		if got[i] != c.Value {
			t.Errorf("enum[%d] = %q，want %q", i, got[i], c.Value)
		}
		if !IsRootCauseCategory(c.Value) {
			t.Errorf("IsRootCauseCategory(%q) = false", c.Value)
		}
	}
	for _, bad := range []string{"", "limit 过小", "OOM_KILLED", "oom_limit_too_small "} {
		if IsRootCauseCategory(bad) {
			t.Errorf("IsRootCauseCategory(%q) 不该通过（精确匹配，不做归一化）", bad)
		}
	}
}

// TestCategoriesCoveredByPlaybook 词表与诊断分支提示词的一致性：每个类别都要在 k8s-diag 分支里
// 出现（那里给出"这个类别长什么样"的判别线索）。词表是 schema enum 与评测比对的源，分支是模型
// 选类别的依据——加了类别却漏写进分支，模型就永远选不到它（枚举里有、线索里没有），
// 评测里表现为"这个类别从不出分"，而单看任何一边都发现不了。
func TestCategoriesCoveredByPlaybook(t *testing.T) {
	path := filepath.Join("..", "workflow", "workflows", "k8s-diag.md")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读分支资产 %s: %v（工作目录应为 internal/k8s）", path, err)
	}
	text := string(raw)
	for _, c := range rootCauseCategories {
		if !strings.Contains(text, c.Value) {
			t.Errorf("词表里的 %s 未在诊断分支里给出判别线索", c.Value)
		}
	}
}

// TestRenderReportMD 验证人读报告的模板与"空小节不出现"。
func TestRenderReportMD(t *testing.T) {
	r := ReportView{
		Target:   Target{Namespace: "diag-lab", Pod: "badimg", Workload: "Deployment/badweb"},
		Symptoms: []string{"OOMKilled", "CrashLoopBackOff"},
		RootCause: RootCauseView{
			Summary: "容器内存超限被杀", Category: "oom_limit_too_small", Confidence: 0.9,
		},
		Evidence: []EvidenceItem{{
			Source: "k8s_logs", Ref: "badimg.previous",
			Excerpt: "OutOfMemoryError: Java heap space", Supports: "上次运行因堆内存耗尽退出",
		}},
		RuledOut:        []RuledOutItem{{Summary: "节点内存不足", Reason: "Node 无 MemoryPressure"}},
		MissingEvidence: []MissingItem{{Item: "容器 limits", Impact: "无法确认限值与用量的比值"}},
		Suggestions:     []SuggestionItem{{Action: "提高 memory limit", Rationale: "用量长期贴限值", Risk: "节点余量下降"}},
		Alternatives:    []AlternativeItem{{Summary: "应用内存泄漏", Confidence: 0.4}},
	}
	md := RenderReportMD(r)
	for _, want := range []string{
		"Root Cause: 容器内存超限被杀（置信度 90%）",
		"类别: oom_limit_too_small",
		"Target: diag-lab/badimg（Deployment/badweb）",
		"Symptoms: OOMKilled, CrashLoopBackOff",
		"Evidence:",
		"1. 上次运行因堆内存耗尽退出（来源：k8s_logs / badimg.previous）",
		"OutOfMemoryError: Java heap space",
		"Ruled Out:",
		"- 节点内存不足：Node 无 MemoryPressure",
		"Missing Evidence:",
		"- 容器 limits：无法确认限值与用量的比值",
		"Suggestion:",
		"- 提高 memory limit（理由：用量长期贴限值；风险：节点余量下降）",
		"Alternatives:",
		"- 应用内存泄漏（40%）",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("报告缺少片段 %q\n---\n%s", want, md)
		}
	}

	// 只有根因时，可选小节整段不出现（不写"无"占注意力）。
	minimal := RenderReportMD(ReportView{
		Target:    Target{Namespace: "default", Pod: "web"},
		RootCause: RootCauseView{Summary: "镜像 tag 不存在", Confidence: 0.8},
	})
	for _, unwanted := range []string{"Ruled Out:", "Missing Evidence:", "Alternatives:", "Evidence:", "Symptoms:"} {
		if strings.Contains(minimal, unwanted) {
			t.Errorf("空小节不应出现 %q\n---\n%s", unwanted, minimal)
		}
	}
}

// TestSaveReportViewFillsMetaAndWritesBothFiles 验证落盘：命名规则、元信息补齐、内容可反序列化。
func TestSaveReportViewFillsMetaAndWritesBothFiles(t *testing.T) {
	c := newTestCollector(t, nil, nil, nil, Config{Context: "minikube-test"})
	c.apiServer = "https://10.0.0.1:6443" // New 才会填（fake 构造路径不经过它），这里直填以断言契约
	r := ReportView{
		Target:      Target{Namespace: "diag-lab", Pod: "badimg"},
		Symptoms:    []string{"ImagePullBackOff"},
		RootCause:   RootCauseView{Summary: "镜像 tag 不存在", Confidence: 0.9},
		Evidence:    []EvidenceItem{{Source: "k8s_events", Supports: "拉取失败", Excerpt: "not found"}},
		Suggestions: []SuggestionItem{{Action: "改用存在的 tag"}},
	}
	jsonPath, mdPath, err := c.SaveReportView(r)
	if err != nil {
		t.Fatalf("SaveReportView: %v", err)
	}
	if filepath.Base(jsonPath) != "diag-lab-badimg.report.json" || filepath.Base(mdPath) != "diag-lab-badimg.report.md" {
		t.Errorf("落盘命名不对: %s / %s", jsonPath, mdPath)
	}

	raw, err := os.ReadFile(jsonPath)
	if err != nil {
		t.Fatalf("读回 report.json: %v", err)
	}
	var back ReportView
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("report.json 不是合法 JSON: %v", err)
	}
	// 四项"本该由系统给"的元信息必须被补齐。
	if back.SchemaVersion != ReportSchemaVersion {
		t.Errorf("schema_version 未补齐: %d", back.SchemaVersion)
	}
	if back.GeneratedAt == "" {
		t.Error("generated_at 未补齐")
	}
	if back.Target.Context != "minikube-test" {
		t.Errorf("target.context 未从采集器配置补齐: %q", back.Target.Context)
	}
	// 端点地址要落进产物：只有 context 名看不出连的是哪个 IP（§17.6）。
	if back.Target.APIServer != "https://10.0.0.1:6443" {
		t.Errorf("target.api_server 未补齐: %q", back.Target.APIServer)
	}
	if back.RootCause.Summary != "镜像 tag 不存在" || len(back.Evidence) != 1 {
		t.Errorf("报告内容不完整: %+v", back)
	}

	mdRaw, err := os.ReadFile(mdPath)
	if err != nil {
		t.Fatalf("读回 report.md: %v", err)
	}
	if !strings.Contains(string(mdRaw), "Root Cause: 镜像 tag 不存在") {
		t.Errorf("report.md 内容不对:\n%s", mdRaw)
	}
	// 人读报告也要能看到端点：Target 行带 @ 地址（§17.6）。
	if !strings.Contains(string(mdRaw), "Target: diag-lab/badimg @ https://10.0.0.1:6443") {
		t.Errorf("report.md 的 Target 行缺端点地址:\n%s", mdRaw)
	}
}
