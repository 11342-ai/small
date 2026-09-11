package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 打分器是"读产物契约"的独立程序，所以这里不 import internal，也不连集群：
// 用例直接在评分函数与产物 JSON 契约上打（口径见 jjj §21.5）。

// testExpect 一份最小可用的 expect 契约（各用例在它上面改）。
func testExpect() expectFile {
	var e expectFile
	e.Name = "demo"
	e.Target.Namespace = "diag-lab"
	e.Target.Selector = "app=demo"
	e.Report.SymptomsMustInclude = []string{"OOMKilled"}
	e.Report.RootCauseCategory = "oom_limit_too_small"
	e.Report.Confidence.Min = 0.8
	e.Report.EvidenceKeywords = []string{"OOMKilled", "limits"}
	e.Report.EvidenceMinHits = 2
	return e
}

// reportOf 造一份报告（证据只填 ref/excerpt/supports 三段，判分把四段拼起来做关键词匹配）。
func reportOf(symptoms []string, category string, conf float64, excerpt string) reportFile {
	var r reportFile
	r.Symptoms = symptoms
	r.RootCause.Summary = "容器内存超限被杀"
	r.RootCause.Category = category
	r.RootCause.Confidence = conf
	r.Evidence = append(r.Evidence, evidenceItem{Source: "k8s_logs", Ref: "web-0.limits", Excerpt: excerpt, Supports: excerpt})
	return r
}

// addMissing 追加一条"缺失证据"声明（匿名结构体的字面量太啰嗦，收在这里）。
func addMissing(r reportFile, item string) reportFile {
	r.MissingEvidence = append(r.MissingEvidence, struct {
		Item   string `json:"item"`
		Impact string `json:"impact"`
	}{Item: item, Impact: "无法排除其它候选"})
	return r
}

// TestScoreScenario 逐项验分：症状/类别/证据/缺失/置信度各自扣分，以及一票否决归零。
func TestScoreScenario(t *testing.T) {
	notesOf := func(kind string, n int) evidenceFile {
		var ev evidenceFile
		for i := 0; i < n; i++ {
			ev.Notes = append(ev.Notes, struct {
				Kind    string `json:"kind"`
				Message string `json:"message"`
			}{Kind: kind, Message: "来源未取到"})
		}
		return ev
	}

	cases := []struct {
		name     string
		mutate   func(exp expectFile, rep reportFile) (expectFile, reportFile, evidenceFile)
		want     float64
		wantVeto bool
	}{
		{"全对", func(exp expectFile, rep reportFile) (expectFile, reportFile, evidenceFile) {
			return exp, rep, evidenceFile{}
		}, 1, false},
		{"类别错", func(exp expectFile, rep reportFile) (expectFile, reportFile, evidenceFile) {
			rep.RootCause.Category = "other"
			return exp, rep, evidenceFile{}
		}, 0.65, false}, // 1 − 0.35
		{"证据只命中一半", func(exp expectFile, rep reportFile) (expectFile, reportFile, evidenceFile) {
			rep.Evidence[0] = evidenceItem{Source: "k8s_logs", Excerpt: "OOMKilled"} // 命中 1/2，达标线 2
			return exp, rep, evidenceFile{}
		}, 0.875, false}, // 1 − 0.25 + 0.25*0.5
		{"置信度低于下限", func(exp expectFile, rep reportFile) (expectFile, reportFile, evidenceFile) {
			rep.RootCause.Confidence = 0.4 // 0.4/0.8 = 0.5
			return exp, rep, evidenceFile{}
		}, 0.95, false},
		{"多报缺失不扣分（工具集外的缺口）", func(exp expectFile, rep reportFile) (expectFile, reportFile, evidenceFile) {
			// 证据包里啥都没缺，报告却声明了一条"容器内监听端口清单"这类采集器不覆盖的缺口：
			// 只提示不扣分（口径见 scoreScenario 第 4 项注释与 §21.5）。
			return exp, addMissing(rep, "容器内实际监听端口清单"), evidenceFile{}
		}, 1, false},
		{"如实申报可选源不可用不扣分", func(exp expectFile, rep reportFile) (expectFile, reportFile, evidenceFile) {
			// metrics 这类可选源取不到，报告把它写进 missing_evidence：这是正当交代，不是"凭空报缺失"。
			return exp, addMissing(rep, "Pod 实时 metrics"), notesOf(noteOptional, 1)
		}, 1, false},
		{"两个必需源失败只声明一个", func(exp expectFile, rep reportFile) (expectFile, reportFile, evidenceFile) {
			return exp, addMissing(rep, "日志"), notesOf(noteRequired, 2)
		}, 0.925, false}, // 缺失项 1/2 = 0.5
		{"必需源失败一条没声明", func(exp expectFile, rep reportFile) (expectFile, reportFile, evidenceFile) {
			return exp, rep, notesOf(noteRequired, 1) // 证据包明说必需源失败，报告 missing_evidence 为空
		}, 0.85, false}, // 缺失项 0（这条是"隐瞒降级"，正是该项要抓的失效模式）
		{"命中一票否决词", func(exp expectFile, rep reportFile) (expectFile, reportFile, evidenceFile) {
			exp.Report.MustNotClaim = []string{"内存超限被杀"}
			return exp, rep, evidenceFile{}
		}, 0, true},
		{"ruled_out 里的候选不算否决", func(exp expectFile, rep reportFile) (expectFile, reportFile, evidenceFile) {
			exp.Report.MustNotClaim = []string{"OOMKilled"}
			rep.RootCause.Summary = "被 liveness 探针杀掉"
			rep.RuledOut = append(rep.RuledOut, struct {
				Summary string `json:"summary"`
				Reason  string `json:"reason"`
			}{Summary: "OOMKilled", Reason: "lastState.terminated.reason 是 Error 而非 OOMKilled"})
			return exp, rep, evidenceFile{}
		}, 1, false}, // 否决只在根因结论上匹配，排除项写进 ruled_out 是安全写法
		{"summary 里的否定句也算命中（口径如此）", func(exp expectFile, rep reportFile) (expectFile, reportFile, evidenceFile) {
			exp.Report.MustNotClaim = []string{"OOMKilled"}
			rep.RootCause.Summary = "被 liveness 探针杀掉，不是 OOMKilled"
			return exp, rep, evidenceFile{}
		}, 0, true}, // 所以 must_not_claim 的词要写成结论式短语，模型也别在 summary 里写否定句
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			exp, rep, ev := c.mutate(testExpect(), reportOf([]string{"OOMKilled"}, "oom_limit_too_small", 0.9, "OOMKilled limits"))
			sc := scoreScenario(exp, rep, ev)
			if sc.Status != "scored" {
				t.Fatalf("状态不对: %+v", sc)
			}
			if diff := sc.Total - c.want; diff > 0.001 || diff < -0.001 {
				t.Errorf("总分 %.4f，期望 %.4f（各项 %+v）", sc.Total, c.want, sc.Items)
			}
			if sc.Veto != c.wantVeto {
				t.Errorf("veto=%v，期望 %v", sc.Veto, c.wantVeto)
			}
			if len(sc.Items) != 5 {
				t.Errorf("应有 5 个打分项: %+v", sc.Items)
			}
		})
	}
}

// TestScoreScenarioOverDeclareNote 多报的处置：不扣分，但要在 score.json 里留一条人工复核提示。
func TestScoreScenarioOverDeclareNote(t *testing.T) {
	exp, rep := testExpect(), reportOf([]string{"OOMKilled"}, "oom_limit_too_small", 0.9, "OOMKilled limits")
	rep = addMissing(rep, "容器内实际监听端口清单")
	sc := scoreScenario(exp, rep, evidenceFile{})
	if sc.Total != 1 {
		t.Errorf("多报不该扣分，总分 %.4f", sc.Total)
	}
	if !strings.Contains(strings.Join(sc.Notes, " "), "多报不扣分") {
		t.Errorf("缺多报提示: %v", sc.Notes)
	}
}

// TestScoreScenarioSymptomPartial 症状缺项按比例扣（0.15 权重内按命中比例）。
func TestScoreScenarioSymptomPartial(t *testing.T) {
	exp := testExpect()
	exp.Report.SymptomsMustInclude = []string{"OOMKilled", "CrashLoopBackOff"}
	sc := scoreScenario(exp, reportOf([]string{"OOMKilled（退避重启）"}, "oom_limit_too_small", 0.9, "OOMKilled limits"), evidenceFile{})
	symptom := sc.Items[0]
	if symptom.Score != 0.5 {
		t.Errorf("症状项应得 0.5，实得 %.2f（%s）", symptom.Score, symptom.Detail)
	}
	// 括号补充说明不该影响命中：模型写 "OOMKilled（退避重启）" 也算中
	if !strings.Contains(symptom.Detail, "报告症状") {
		t.Errorf("明细应回显报告症状: %s", symptom.Detail)
	}
}

// TestEvidenceKeywordNormalization 关键词匹配要吃掉写法差异：模型的 ref 里字段名写成
// last_state / last state 也算命中 lastState（首批端到端实测：这条曾把一个满分的证据项判成半分）。
func TestEvidenceKeywordNormalization(t *testing.T) {
	exp := testExpect()
	exp.Report.EvidenceKeywords = []string{"restartCount", "lastState", "exitCode"}
	exp.Report.EvidenceMinHits = 3
	rep := reportOf([]string{"OOMKilled"}, "oom_limit_too_small", 0.9, "x")
	rep.Evidence[0] = evidenceItem{
		Source: "k8s_pod",
		Ref:    "container app last_state.terminated: restart_count 2, exit_code 137",
	}
	sc := scoreScenario(exp, rep, evidenceFile{})
	if got := sc.Items[2].Score; got != 1 {
		t.Errorf("下划线/大小写差异不该影响关键词命中，证据项得 %.2f（%s）", got, sc.Items[2].Detail)
	}
}

// TestExpectFilesParse 真场景契约的解析回归：判分器与 Zoo/k8s-lab/scenarios 下的 expect.json
// 是两端契约，字段名漂移（改名/改层级）在打分侧是静默的——空列表会让比例项直接满分。
// 所以这里断言"该有的都有值"，把漂移变成硬失败。
func TestExpectFilesParse(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("..", "..", "Zoo", "k8s-lab", "scenarios", "*", "expect.json"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("没找到场景 expect.json: %v", err)
	}
	for _, p := range paths {
		t.Run(filepath.Base(filepath.Dir(p)), func(t *testing.T) {
			exp, err := loadJSON[expectFile](p)
			if err != nil {
				t.Fatalf("解析: %v", err)
			}
			if exp.Name == "" || exp.Target.Selector == "" {
				t.Errorf("name/target.selector 解析为空: %+v", exp)
			}
			r := exp.Report
			if len(r.SymptomsMustInclude) == 0 || r.RootCauseCategory == "" {
				t.Errorf("症状/类别解析为空: %+v", r)
			}
			if len(r.EvidenceKeywords) == 0 || r.EvidenceMinHits <= 0 {
				t.Errorf("证据关键词/达标线解析为空: %+v", r)
			}
			if r.Confidence.Min <= 0 {
				t.Errorf("置信度下限解析为空: %+v", r.Confidence)
			}
		})
	}
}

// TestRenderSummary 汇总表要能一眼看出三类结果：正常分、0 分（一票否决）、证据覆盖不足。
func TestRenderSummary(t *testing.T) {
	good := scoreScenario(testExpect(), reportOf([]string{"OOMKilled"}, "oom_limit_too_small", 0.9, "OOMKilled limits"), evidenceFile{})
	badExp := testExpect()
	badExp.Report.MustNotClaim = []string{"容器内存超限被杀"}
	bad := scoreScenario(badExp, reportOf([]string{"Pending"}, "other", 0.3, "没提到关键词"), evidenceFile{})
	if !bad.Veto || bad.Total != 0 {
		t.Fatalf("这条用例要的是 0 分（一票否决）: %+v", bad)
	}
	md := renderSummary([]scenarioScore{good, bad})

	for _, want := range []string{"demo", "平均总分", "一票否决", "0 分场景", "证据覆盖不足场景"} {
		if !strings.Contains(md, want) {
			t.Errorf("汇总缺 %q:\n%s", want, md)
		}
	}
}

// TestRunScoreNoReport 报告缺失记 no_report（与"诊断错"区分开），并落 score.json。
func TestRunScoreNoReport(t *testing.T) {
	out := t.TempDir()
	repo := filepath.Join("..", "..", "Zoo", "k8s-lab", "scenarios", "oom-limit-too-small", "expect.json")
	if _, err := os.Stat(repo); err != nil {
		t.Skipf("场景契约不在（%v）", err)
	}
	runScore([]string{"--expect", repo, "--report", filepath.Join(t.TempDir(), "missing.json"), "--out", out})

	raw, err := os.ReadFile(filepath.Join(out, "oom-limit-too-small.score.json"))
	if err != nil {
		t.Fatalf("score.json 未落盘: %v", err)
	}
	var sc scenarioScore
	if err := json.Unmarshal(raw, &sc); err != nil {
		t.Fatalf("score.json 不是合法 JSON: %v", err)
	}
	if sc.Status != "no_report" || sc.Total != 0 || len(sc.Items) != 0 {
		t.Errorf("no_report 的形态不对: %+v", sc)
	}
}
