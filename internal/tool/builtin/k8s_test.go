package builtin

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"small/internal/k8s"
)

// 单元 3 的参数校验与失败语义（不触达集群）：校验都在解码阶段，不通过就走不到采集/落盘。
// 零值 Collector 够用——注意它的 Dir 为空，所以"结构合法但落盘失败"也应当回灌业务失败。

// validReportArgs 一份结构合法的报告参数，各用例在它上面做删改。
// 证据给两条、来源两种：高置信度（0.9）要过 checkConfidenceRules 就必须这样——单条单源自 2026-09-15
// 起会被这条规则打回（见 TestK8sReportConfidenceRule）。
func validReportArgs() map[string]any {
	return map[string]any{
		"namespace":  "default",
		"pod":        "web-0",
		"symptoms":   []string{"OOMKilled"},
		"root_cause": map[string]any{"summary": "容器内存超限被杀", "category": "oom_limit_too_small", "confidence": 0.9},
		"evidence": []map[string]any{
			{"source": "k8s_logs", "supports": "上次运行堆内存耗尽"},
			{"source": "k8s_pod", "supports": "lastState.terminated.reason=OOMKilled"},
		},
		"suggestions": []map[string]any{{"action": "提高 memory limit"}},
	}
}

// TestK8sReportArgsValidation 逐项验证必填与范围校验：全部应是业务失败（err=nil）。
func TestK8sReportArgsValidation(t *testing.T) {
	cases := []struct {
		name  string
		build func() map[string]any
		want  string
	}{
		{"缺 namespace", func() map[string]any { m := validReportArgs(); delete(m, "namespace"); return m }, "namespace"},
		{"缺 pod", func() map[string]any { m := validReportArgs(); delete(m, "pod"); return m }, "pod"},
		{"缺 symptoms", func() map[string]any { m := validReportArgs(); delete(m, "symptoms"); return m }, "symptoms"},
		{"缺 root_cause.summary", func() map[string]any {
			m := validReportArgs()
			m["root_cause"] = map[string]any{"category": "oom_limit_too_small", "confidence": 0.9}
			return m
		}, "root_cause.summary"},
		{"category 不在词表", func() map[string]any {
			m := validReportArgs()
			m["root_cause"] = map[string]any{"summary": "x", "category": "limit 过小", "confidence": 0.9}
			return m
		}, "category"},
		{"缺 category", func() map[string]any {
			m := validReportArgs()
			m["root_cause"] = map[string]any{"summary": "x", "confidence": 0.9}
			return m
		}, "category"},
		{"confidence 越界", func() map[string]any {
			m := validReportArgs()
			m["root_cause"] = map[string]any{"summary": "x", "category": "other", "confidence": 1.5}
			return m
		}, "confidence"},
		{"缺 evidence", func() map[string]any { m := validReportArgs(); delete(m, "evidence"); return m }, "evidence"},
		{"evidence 缺 source", func() map[string]any {
			m := validReportArgs()
			m["evidence"] = []map[string]any{{"supports": "没有来源的断言"}}
			return m
		}, "evidence[0].source"},
		{"缺 suggestions", func() map[string]any { m := validReportArgs(); delete(m, "suggestions"); return m }, "suggestions"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			raw, err := json.Marshal(c.build())
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			res, err := K8sReport(&k8s.Collector{}).Execute(context.Background(), raw)
			if err != nil {
				t.Fatalf("参数问题应是业务失败而非框架错误: %v", err)
			}
			if !res.IsError {
				t.Fatalf("应回灌业务失败: %+v", res)
			}
			if !strings.Contains(res.Data, c.want) {
				t.Errorf("错误信息应含 %q，实际: %s", c.want, res.Data)
			}
		})
	}
}

// TestK8sReportConfidenceRule 校验 §7.2 里代码判得了的那半条：置信度 ≥0.80 要至少两条独立证据
// （条数 ≥2 且来源去重 ≥2）。不通过是业务失败（err=nil、IsError=true），把"差在哪"回灌给模型自己
// 补证据或降档——工具不替它改数字。另一半（"含一条决定性证据"、关键证据缺失去顶）判不了语义，
// 仍在提示词与评测扣分里管，见 checkConfidenceRules 的注释。
func TestK8sReportConfidenceRule(t *testing.T) {
	cases := []struct {
		name    string
		build   func() map[string]any
		want    string
		notWant string
	}{
		{"单条证据 + 高置信度被打回", func() map[string]any {
			m := validReportArgs()
			m["evidence"] = []map[string]any{{"source": "k8s_logs", "supports": "只有一条证据"}}
			return m
		}, "独立证据", ""},
		{"两条证据同一来源被打回（拆条不算独立）", func() map[string]any {
			m := validReportArgs()
			m["evidence"] = []map[string]any{
				{"source": "k8s_logs", "supports": "a"},
				{"source": "k8s_logs", "supports": "b"},
			}
			return m
		}, "独立证据", ""},
		{"低置信度不受这条规则约束", func() map[string]any {
			m := validReportArgs()
			m["root_cause"] = map[string]any{"summary": "仅现象吻合", "category": "other", "confidence": 0.5}
			m["evidence"] = []map[string]any{{"source": "k8s_logs", "supports": "只有现象"}}
			return m
		}, "报告落盘失败", "独立证据"},
		{"两条独立来源可过校验（随后才因零值 Dir 落盘失败）", func() map[string]any {
			return validReportArgs()
		}, "报告落盘失败", "独立证据"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			raw, err := json.Marshal(c.build())
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			res, err := K8sReport(&k8s.Collector{}).Execute(context.Background(), raw)
			if err != nil {
				t.Fatalf("规则不通过应是业务失败而非框架错误: %v", err)
			}
			if !res.IsError {
				t.Fatalf("应回灌业务失败: %+v", res)
			}
			if !strings.Contains(res.Data, c.want) {
				t.Errorf("回灌应含 %q，实际: %s", c.want, res.Data)
			}
			if c.notWant != "" && strings.Contains(res.Data, c.notWant) {
				t.Errorf("回灌不该含 %q（说明规则误伤了这条正确报告），实际: %s", c.notWant, res.Data)
			}
		})
	}
}

// TestK8sReportSchemaEnums 验证参数契约里的根因类别由 internal/k8s 的词表生成：
// schema 是合法 JSON（拼装走 Sprintf，格式错要在这里拦下）、enum 与词表逐项一致、
// category 必填、描述里带图例（strict 子集的 enum 没有逐值说明，模型只能从描述读含义）。
func TestK8sReportSchemaEnums(t *testing.T) {
	spec := K8sReport(nil).Spec()
	var schema struct {
		Properties struct {
			RootCause struct {
				Properties struct {
					Category struct {
						Enum        []string `json:"enum"`
						Description string   `json:"description"`
					} `json:"category"`
				} `json:"properties"`
				Required []string `json:"required"`
			} `json:"root_cause"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(spec.Parameters, &schema); err != nil {
		t.Fatalf("k8s_report 参数契约不是合法 JSON: %v", err)
	}
	cat := schema.Properties.RootCause.Properties.Category
	var want []string
	if err := json.Unmarshal([]byte(k8s.CategoryValuesJSON()), &want); err != nil {
		t.Fatalf("词表 JSON 不合法: %v", err)
	}
	if strings.Join(cat.Enum, ",") != strings.Join(want, ",") {
		t.Errorf("enum 与词表不一致:\n got %v\nwant %v", cat.Enum, want)
	}
	if !strings.Contains(cat.Description, k8s.CategoryLegend()) {
		t.Errorf("category 描述里缺图例: %s", cat.Description)
	}
	if !slices.Contains(schema.Properties.RootCause.Required, "category") {
		t.Error("category 应为必填（评分靠它，不给就无从比对）")
	}
}

// TestK8sReportSaveFailureIsBusinessFailure 结构合法但落盘失败（零值 Collector 的 Dir 为空）
// 也必须是业务失败：落盘 IO 问题不该中断 agent 循环。
func TestK8sReportSaveFailureIsBusinessFailure(t *testing.T) {
	raw, err := json.Marshal(validReportArgs())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	res, err := K8sReport(&k8s.Collector{}).Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("落盘失败应是业务失败而非框架错误: %v", err)
	}
	if !res.IsError || !strings.Contains(res.Data, "落盘失败") {
		t.Errorf("应回灌落盘失败，实际: %+v", res)
	}
}

// TestK8sEvidenceArgsValidation evidence 复用 podArgs：缺必填或 JSON 非法都回灌业务失败。
func TestK8sEvidenceArgsValidation(t *testing.T) {
	for _, args := range []string{`{}`, `{"namespace":"default"}`, `{"pod":"web-0"}`, `{`} {
		res, err := K8sEvidence(&k8s.Collector{}).Execute(context.Background(), json.RawMessage(args))
		if err != nil {
			t.Fatalf("参数问题应是业务失败而非框架错误（args=%s）: %v", args, err)
		}
		if !res.IsError {
			t.Errorf("args=%s 应回灌业务失败: %+v", args, res)
		}
	}
}
