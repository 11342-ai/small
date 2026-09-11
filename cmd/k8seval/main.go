// Package main 场景评测打分器（k8seval）：读产物契约打分，不 import internal。
//
// 为什么独立 main 包、只用标准库（设计文档 §11、口径 jjj §21.5）：判分读的是产物文件
// （expect.json / report.json / evidence.json），复用 internal 的类型会把"实现漂移"变成
// "判分跟着漂移"——被测代码改了字段名，判分器也悄悄跟着改，评测就失去了独立基线。
//
// 用法:
//
//	go run ./cmd/k8seval score --expect <expect.json> --report <report.json> --evidence <evidence.json> [--out <run-dir>]
//	go run ./cmd/k8seval summary --out <run-dir>
//
// score 打一个场景的分，给了 --out 就落 <run-dir>/<场景名>.score.json；
// summary 汇总 run 目录下所有 score.json，打印一张表并写 summary.md。
// 报告缺失（模型没提交）记 status=no_report、总分 0：评测要能区分"诊断错"与"根本没产出"。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// --- 输入契约（只声明判分用得到的字段，其余字段判分不关心）---

type expectFile struct {
	Name   string `json:"name"`
	Target struct {
		Namespace string `json:"namespace"`
		Selector  string `json:"selector"`
	} `json:"target"`
	Report struct {
		SymptomsMustInclude []string `json:"symptoms_must_include"`
		RootCauseCategory   string   `json:"root_cause_category"`
		Confidence          struct {
			Min float64 `json:"min"`
		} `json:"confidence"`
		EvidenceKeywords []string `json:"evidence_keywords"`
		EvidenceMinHits  int      `json:"evidence_min_hits"`
		MustNotClaim     []string `json:"must_not_claim"`
	} `json:"report"`
}

type reportFile struct {
	Symptoms  []string `json:"symptoms"`
	RootCause struct {
		Summary    string  `json:"summary"`
		Category   string  `json:"category"`
		Confidence float64 `json:"confidence"`
	} `json:"root_cause"`
	Evidence []evidenceItem `json:"evidence"`
	RuledOut []struct {
		Summary string `json:"summary"`
		Reason  string `json:"reason"`
	} `json:"ruled_out"`
	MissingEvidence []struct {
		Item   string `json:"item"`
		Impact string `json:"impact"`
	} `json:"missing_evidence"`
}

type evidenceItem struct {
	Source   string `json:"source"`
	Ref      string `json:"ref"`
	Excerpt  string `json:"excerpt"`
	Supports string `json:"supports"`
}

// evidenceFile 证据包：判分只关心 notes（降级说明的类别是"缺失证据"的唯一客观基准）。
type evidenceFile struct {
	Notes []struct {
		Kind    string `json:"kind"`
		Message string `json:"message"`
	} `json:"notes"`
}

// --- 输出契约（score.json / summary 共用；字段名是给人看的，故保持扁平）---

type itemScore struct {
	Name     string  `json:"name"`
	Weight   float64 `json:"weight"`
	Score    float64 `json:"score"`    // 该项内的达成度 0-1
	Weighted float64 `json:"weighted"` // Weight*Score
	Detail   string  `json:"detail"`
}

type scenarioScore struct {
	Scenario string      `json:"scenario"`
	Status   string      `json:"status"` // scored / no_report
	Total    float64     `json:"total"`
	Veto     bool        `json:"veto,omitempty"`
	VetoHits []string    `json:"veto_hits,omitempty"`
	Items    []itemScore `json:"items,omitempty"`
	Notes    []string    `json:"notes,omitempty"` // 判分器的口径提醒（如按条数比对）
}

// 权重（jjj §21.5）：类别最重，因为它是结论本身。
const (
	wSymptom  = 0.15
	wCategory = 0.35
	wEvidence = 0.25
	wMissing  = 0.15
	wConf     = 0.10
)

// noteRequired/noteOptional 证据包降级说明的两个类别值（与 internal/k8s 的 NoteKind 取值对齐，
// 但不 import 它：判分不跟自己测的实现耦合）。required 漏声明要扣，optional 如实申报不扣。
const (
	noteRequired = "required_failed"
	noteOptional = "optional_unavailable"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "score":
		runScore(os.Args[2:])
	case "summary":
		runSummary(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `用法:
  k8seval score --expect <expect.json> --report <report.json> --evidence <evidence.json> [--out <run-dir>]
  k8seval summary --out <run-dir>
`)
}

func runScore(args []string) {
	fs := flag.NewFlagSet("score", flag.ExitOnError)
	expectPath := fs.String("expect", "", "expect.json 路径（必填）")
	reportPath := fs.String("report", "", "产物 report.json 路径（缺文件记 no_report）")
	evidencePath := fs.String("evidence", "", "产物 evidence.json 路径（缺文件则缺失声明项按“无基准”算）")
	outDir := fs.String("out", "", "run 目录；给了才落 <场景名>.score.json")
	_ = fs.Parse(args)

	exp, err := loadJSON[expectFile](*expectPath)
	if err != nil {
		die("读 expect.json: %v", err)
	}
	if exp.Name == "" {
		die("expect.json 缺 name（场景名是 score.json 的文件名，不能空）")
	}

	var sc scenarioScore
	rep, err := loadJSON[reportFile](*reportPath)
	switch {
	case err != nil:
		// 报告缺失：不是打分器的错，而是"这轮没产出"——单独一种状态，别与低分混在一起。
		sc = scenarioScore{Scenario: exp.Name, Status: "no_report",
			Notes: []string{fmt.Sprintf("报告未产出或不可读（%v）", err)}}
	default:
		ev, evErr := loadJSON[evidenceFile](*evidencePath)
		if evErr != nil {
			// 证据包缺失会让"缺失声明"失去客观基准，但它不该让整条打分失败：按无基准处理并写明。
			ev = evidenceFile{}
			sc.Notes = append(sc.Notes, fmt.Sprintf("证据包不可读（%v）：缺失声明项按“报告未声明”计", evErr))
		}
		sc = scoreScenario(exp, rep, ev)
	}

	data, err := json.MarshalIndent(sc, "", "  ")
	if err != nil {
		die("序列化打分结果: %v", err)
	}
	if *outDir != "" {
		if err := os.MkdirAll(*outDir, 0o755); err != nil {
			die("建 run 目录: %v", err)
		}
		path := filepath.Join(*outDir, exp.Name+".score.json")
		if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
			die("写 score.json: %v", err)
		}
	}
	fmt.Println(line(sc))
}

// scoreScenario 六个打分项：症状、类别、证据覆盖、缺失声明、置信度档位，最后一票否决。
func scoreScenario(exp expectFile, rep reportFile, ev evidenceFile) scenarioScore {
	sc := scenarioScore{Scenario: exp.Name, Status: "scored"}

	// 1. 症状识别：必需词全中得满分，缺一按比例扣。
	sc.Items = append(sc.Items, item("症状识别", wSymptom,
		ratio(hitCount(exp.Report.SymptomsMustInclude, strings.Join(rep.Symptoms, " / ")),
			len(exp.Report.SymptomsMustInclude)),
		fmt.Sprintf("报告症状 %v；要求含 %v", rep.Symptoms, exp.Report.SymptomsMustInclude)))

	// 2. 根因类别：精确比对（词表已冻结，不做同义词归并）。
	gotCat := strings.TrimSpace(rep.RootCause.Category)
	catHit := gotCat == exp.Report.RootCauseCategory
	sc.Items = append(sc.Items, item("根因类别", wCategory, boolScore(catHit),
		fmt.Sprintf("期望 %s，实得 %q", exp.Report.RootCauseCategory, gotCat)))

	// 3. 证据覆盖：关键词命中数 ÷ min_hits（封顶 1）。用关键词而非精确 ref——ref 是模型自由文本。
	text := evidenceText(rep)
	hits := hitCount(exp.Report.EvidenceKeywords, text)
	want := exp.Report.EvidenceMinHits
	if want <= 0 {
		want = len(exp.Report.EvidenceKeywords)
	}
	sc.Items = append(sc.Items, item("证据覆盖", wEvidence,
		ratio(hits, want),
		fmt.Sprintf("命中 %d/%d 个关键词（达标线 %d）：%s", hits, len(exp.Report.EvidenceKeywords), want,
			joinedWords(exp.Report.EvidenceKeywords, text))))

	// 4. 缺失声明：只看漏报（必需来源失败必须如实申报），不看多报。
	// 为什么不管多报（2026-09-15 端到端实测后改的口径）：证据包的 notes 只覆盖"采集器自己的来源
	// 读失败"，而报告里的 missing_evidence 是模型视角的证据缺口，天然可以超出这个集合——实测中
	// 模型申报的"容器内监听端口清单""ResourceQuota 明细""previous 日志正文不可读"都属工具集外的
	// 合法缺口，且模型自己写明"不影响结论"。多报的对错要读语义才判得出，按条数比必然错杀；而扣
	// 多报等于训练"少说话"，与"降级必须可见"的设计原则相悖。故多报只提示、不扣分。
	// 基准按条数而非逐条配对，见 §21.5（报告里是模型的自由表述，逐条配对必然要上一套同义词启发式）。
	req, opt := noteCounts(ev)
	m := len(rep.MissingEvidence)
	score := 1.0
	if req > 0 {
		score = ratio(m, req)
	}
	sc.Items = append(sc.Items, item("缺失声明", wMissing, score,
		fmt.Sprintf("证据包 required_failed=%d、optional_unavailable=%d，报告 missing_evidence=%d 条", req, opt, m)))
	if req > 0 {
		sc.Notes = append(sc.Notes, "本场景证据包有必需来源失败，缺失声明按条数比对（非逐条配对）")
	}
	if allowed := req + opt; m > allowed {
		sc.Notes = append(sc.Notes,
			fmt.Sprintf("报告申报 %d 条缺失、证据包只有 %d 条降级说明：多报不扣分，超出的部分是工具集之外的缺口，值得人工扫一眼", m, allowed))
	}

	// 5. 置信度档位：达到下限得满分，低于下限按偏离度线性扣（下限处 1，0 处 0）。
	conf, min := rep.RootCause.Confidence, exp.Report.Confidence.Min
	if conf > 1 {
		conf = 1
	}
	confScore := 1.0
	if min > 0 && conf < min {
		confScore = conf / min
	}
	sc.Items = append(sc.Items, item("置信度", wConf, confScore,
		fmt.Sprintf("实得 %.2f，下限 %.2f", rep.RootCause.Confidence, min)))

	// 6. 一票否决：只在根因结论上匹配（summary + category 值），不扫 ruled_out——那里是
	// "被排除的候选"的地盘，扫它会把"已排除 OOMKilled"这种正确推理判错（§21.3 的收紧）。
	// 代价是 summary 里的否定句（"不是 OOMKilled"）仍会被算中，故 must_not_claim 的词要写成结论式短语。
	for _, bad := range exp.Report.MustNotClaim {
		if containsFold(rep.RootCause.Summary, bad) || containsFold(gotCat, bad) {
			sc.VetoHits = append(sc.VetoHits, bad)
		}
	}
	for _, it := range sc.Items {
		sc.Total += it.Weighted
	}
	if len(sc.VetoHits) > 0 {
		sc.Veto = true
		sc.Total = 0
		sc.Notes = append(sc.Notes, "命中 must_not_claim，本场景总分置 0（一票否决）")
	}
	return sc
}

// --- 判分小工具（都是纯函数，单测直接打）---

// item 组装一项打分（Weighted 在这里算，免得每个调用点各算一遍）。
func item(name string, weight, score float64, detail string) itemScore {
	return itemScore{Name: name, Weight: weight, Score: score, Weighted: weight * score, Detail: detail}
}

// ratio 命中比例，封顶 1；分母为 0 视为满分（没有要求就是达标）。
func ratio(hit, total int) float64 {
	if total <= 0 {
		return 1
	}
	if hit >= total {
		return 1
	}
	if hit <= 0 {
		return 0
	}
	return float64(hit) / float64(total)
}

func boolScore(ok bool) float64 {
	if ok {
		return 1
	}
	return 0
}

// hitCount 逐词做不区分大小写的子串命中，返回命中个数（不去重：两个关键词同一处命中算两分，
// 因为 expect.json 里每个关键词代表一个独立的证据面）。
func hitCount(words []string, text string) int {
	n := 0
	for _, w := range words {
		if w != "" && containsFold(text, w) {
			n++
		}
	}
	return n
}

// containsFold 归一化后的子串判定：模型的 ref/症状写法很杂（lastState / last_state / last state、
// CrashLoopBackOff 后面还常跟括号补充），判分不该因为下划线、空格或大小写而扣分。
func containsFold(haystack, needle string) bool {
	return strings.Contains(normText(haystack), normText(needle))
}

// normText 小写 + 去掉下划线/连字符/空格（只用于比较，不改原文本）。
func normText(s string) string {
	return strings.NewReplacer("_", "", "-", "", " ", "").Replace(strings.ToLower(s))
}

// evidenceText 把证据各字段拼成一段文本供关键词匹配（ref 是模型自由文本，不能拿来做精确比对）。
func evidenceText(rep reportFile) string {
	var b strings.Builder
	for _, e := range rep.Evidence {
		b.WriteString(e.Source)
		b.WriteString(" ")
		b.WriteString(e.Ref)
		b.WriteString(" ")
		b.WriteString(e.Excerpt)
		b.WriteString(" ")
		b.WriteString(e.Supports)
		b.WriteString("\n")
	}
	return b.String()
}

// joinedWords 把"哪个关键词命中/没命中"拼成一行，供人核对（分数之外要能看出差在哪）。
func joinedWords(words []string, text string) string {
	parts := make([]string, 0, len(words))
	for _, w := range words {
		if containsFold(text, w) {
			parts = append(parts, w+"✓")
		} else {
			parts = append(parts, w+"✗")
		}
	}
	return strings.Join(parts, " ")
}

// noteCounts 数证据包里的必需/可选降级条目——缺失声明的客观基准（见 scoreScenario 第 4 项）。
func noteCounts(ev evidenceFile) (required, optional int) {
	for _, n := range ev.Notes {
		switch n.Kind {
		case noteRequired:
			required++
		case noteOptional:
			optional++
		}
	}
	return required, optional
}

// --- summary ---

func runSummary(args []string) {
	fs := flag.NewFlagSet("summary", flag.ExitOnError)
	outDir := fs.String("out", "", "run 目录（必填，读其中的 *.score.json）")
	_ = fs.Parse(args)
	if *outDir == "" {
		die("summary 需要 --out <run-dir>")
	}
	paths, err := filepath.Glob(filepath.Join(*outDir, "*.score.json"))
	if err != nil {
		die("扫 run 目录: %v", err)
	}
	if len(paths) == 0 {
		die("run 目录里没有 *.score.json：%s", *outDir)
	}
	scores := make([]scenarioScore, 0, len(paths))
	for _, p := range paths {
		sc, err := loadJSON[scenarioScore](p)
		if err != nil {
			die("读 %s: %v", p, err)
		}
		scores = append(scores, sc)
	}
	sort.Slice(scores, func(i, j int) bool { return scores[i].Scenario < scores[j].Scenario })

	md := renderSummary(scores)
	path := filepath.Join(*outDir, "summary.md")
	if err := os.WriteFile(path, []byte(md), 0o644); err != nil {
		die("写 summary.md: %v", err)
	}
	fmt.Print(md)
	fmt.Printf("\n汇总写到 %s\n", path)
}

// renderSummary 出表 + 一行总体，并单独点出两类需要人看的结果：0 分场景与证据覆盖不足场景。
// 本批不设通过阈值（基线未稳），所以只标注、不判定通过与否。
func renderSummary(scores []scenarioScore) string {
	var b strings.Builder
	b.WriteString("# 场景评测汇总\n\n")
	b.WriteString("| 场景 | 状态 | 总分 | 症状 | 类别 | 证据 | 缺失 | 置信 | 备注 |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|---|\n")
	var sum float64
	var zero, weak []string
	for _, sc := range scores {
		sum += sc.Total
		mark := ""
		switch {
		case sc.Veto:
			mark = "一票否决：" + strings.Join(sc.VetoHits, "、")
		case sc.Status != "scored":
			mark = sc.Status
		case sc.Total == 0:
			mark = "0 分"
		}
		zero = appendIf(zero, mark != "" && sc.Status == "scored" && sc.Total == 0, sc.Scenario)
		weak = appendIf(weak, itemScoreOf(sc, "证据覆盖") < 1, sc.Scenario)
		fmt.Fprintf(&b, "| %s | %s | %.3f | %s | %s | %s | %s | %s | %s |\n",
			sc.Scenario, sc.Status, sc.Total,
			cell(sc, "症状识别"), cell(sc, "根因类别"), cell(sc, "证据覆盖"),
			cell(sc, "缺失声明"), cell(sc, "置信度"), mark)
	}
	fmt.Fprintf(&b, "\n场景数 %d，平均总分 %.3f（本批不设阈值，只看 0 分与证据覆盖不足两类）。\n",
		len(scores), sum/float64(len(scores)))
	if len(zero) > 0 {
		fmt.Fprintf(&b, "\n0 分场景：%s\n", strings.Join(zero, "、"))
	}
	if len(weak) > 0 {
		fmt.Fprintf(&b, "\n证据覆盖不足场景：%s（关键词命中数未达 expect 的达标线）\n", strings.Join(weak, "、"))
	}
	return b.String()
}

// appendIf 小助手：条件成立才追加（汇总里的两三处标注共用，省掉三份 if）。
func appendIf(dst []string, ok bool, v string) []string {
	if ok {
		return append(dst, v)
	}
	return dst
}

// cell 取某项的分数（缺项显示 "-"：no_report 的场景没有逐项分）。
func cell(sc scenarioScore, name string) string {
	for _, it := range sc.Items {
		if it.Name == name {
			return fmt.Sprintf("%.2f", it.Score)
		}
	}
	return "-"
}

func itemScoreOf(sc scenarioScore, name string) float64 {
	for _, it := range sc.Items {
		if it.Name == name {
			return it.Score
		}
	}
	return 1 // 缺项不当作"覆盖不足"（no_report 已单独标注）
}

// line 单场景一行的 stdout 输出（eval.sh 跑的时候人盯着看的就是它）。
func line(sc scenarioScore) string {
	mark := ""
	if sc.Veto {
		mark = fmt.Sprintf("  一票否决：%s", strings.Join(sc.VetoHits, "、"))
	} else if sc.Status != "scored" {
		mark = "  " + sc.Status
	}
	return fmt.Sprintf("%-32s total=%.3f 症状=%s 类别=%s 证据=%s 缺失=%s 置信=%s%s",
		sc.Scenario, sc.Total, cell(sc, "症状识别"), cell(sc, "根因类别"),
		cell(sc, "证据覆盖"), cell(sc, "缺失声明"), cell(sc, "置信度"), mark)
}

// --- 文件与错误 ---

// loadJSON 读 JSON 文件并解析成 T（泛型省掉四份几乎一样的读+解析样板）。
func loadJSON[T any](path string) (T, error) {
	var v T
	if path == "" {
		return v, fmt.Errorf("未给路径")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return v, err
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return v, fmt.Errorf("解析 %s: %w", path, err)
	}
	return v, nil
}

// die 打分器是给人用的命令行工具，出错直接退出（fail fast，不吞错）。
func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "k8seval: "+format+"\n", args...)
	os.Exit(1)
}
