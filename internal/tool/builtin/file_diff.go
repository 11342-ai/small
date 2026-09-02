package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"small/internal/tool"
)

// FileDiff 比对两个文本文件的内容差异（Zoo/model/tool-diff.md，唯一技术索引）。
// 两级输出：内容块（空行分隔）粒度定位增/删/改，改动块内再做字/词级细标并回显原文。
// 敏感度可按类降低：ignore 枚举 space（空白）/punct（标点）/symbol（特殊字符）。
// 归一化只用于判定与定位，报告始终可对回原文；纯格式差异折叠为尾部计数不刷屏。
// 只读工具（权限表 Pass），路径/大小/.env 防护与 file_read 同一套（resolveReadPath）。
func FileDiff(cfg *FileConfig) tool.Tool {
	return tool.NewFunc(
		tool.Spec{
			Name:        "file_diff",
			Description: "比对两个文本文件的内容差异：先定位哪些段落（空行分隔的内容块）有增删改，再对改动段落细标到字/词并回显原文。默认全敏感（标点/空白/特殊字符的差异也算）；只需看实质内容差异时传 ignore（可组合：space 空白、punct 标点、symbol 特殊字符如 ①② 等圈码/符号），被忽略类别的差异折叠为计数。path 相对工作区根或用户明确要求访问的工作区外绝对路径（同 file_read；.env 与系统敏感目录拒绝；单文件超 10MB 拒绝）。limit 控制细标明细条数（缺省 10，0 = 不限）。用于核对两个版本/两份文档改了什么、整理产物是否丢内容。",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"file_a": {"type": "string", "description": "被比对文件 A（相对工作区根或显式绝对路径）"},
					"file_b": {"type": "string", "description": "被比对文件 B（相对工作区根或显式绝对路径）"},
					"ignore": {
						"type": "array",
						"items": {"type": "string", "enum": ["space", "punct", "symbol"]},
						"description": "忽略的差异类别（可组合）：space 空白、punct 标点、symbol 特殊字符。缺省空 = 全敏感"
					},
					"limit": {"type": "integer", "description": "细标明细最多条数，缺省 10，0 = 不限"}
				},
				"required": ["file_a", "file_b"]
			}`),
		},
		func(_ context.Context, args json.RawMessage) (tool.Result, error) {
			return runFileDiff(cfg, args)
		},
	)
}

// diffIgnore 忽略类别位集（tool-diff.md §3.2：以 Unicode 属性归类，无词表）。
type diffIgnore uint8

const (
	ignSpace  diffIgnore = 1 << iota // 空白与排版符（Z* 及 \t\r\n\f 等控制空白）
	ignPunct                         // 标点（P*，含中文全角标点）
	ignSymbol                        // 符号/特殊字符（S* 及非 Nd 数字类圈码 ①② 等）
)

// diffIgnoredRune 判定单个 rune 是否属被忽略类别。文字（L* + Nd + '_'）永不被忽略；
// No 类圈码（① 等）归 symbol 可忽略——它们是"符号性数字"，与正文文字语义不同。
func diffIgnoredRune(r rune, ign diffIgnore) bool {
	if ign&ignSpace != 0 && unicode.IsSpace(r) {
		return true
	}
	if ign&ignPunct != 0 && unicode.IsPunct(r) {
		return true
	}
	if ign&ignSymbol != 0 && (unicode.IsSymbol(r) || (unicode.IsNumber(r) && !unicode.IsDigit(r))) {
		return true
	}
	return false
}

// diffNormalize 归一化：剔除 ignore 对应类别的字符序列，用于判同与定位（不回显）。
func diffNormalize(s string, ign diffIgnore) string {
	if ign == 0 {
		return s // 全敏感时零拷贝短路（大头路径）
	}
	var b strings.Builder
	for _, r := range s {
		if diffIgnoredRune(r, ign) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// diffBlock 一个内容块：空行分隔的连续行（tool-diff.md §4 步骤 2）。
type diffBlock struct {
	ord     int    // 文件内块序号（1-based，报告"第 k 段"定位用）
	start   int    // 起始行号（1-based，含）
	end     int    // 结束行号（1-based，含）
	text    string // 块内原文（行以 \n 连接；已去行尾 \r）
	ntext   string // 归一化文本（判同与指纹用）
	rawDiff bool   // text != ntext（存在被忽略类差异 → 折叠计数）
}

// diffSplitBlocks 按空行切内容块。TrimSpace 判空行使"纯空白行"也作块边界；
// 语言中立（md/正文/代码一致），标题/列表被空行包围时自然成块。
func diffSplitBlocks(lines []string, ign diffIgnore) []diffBlock {
	var blocks []diffBlock
	start, ord := 0, 0 // start：当前未处理行的 0-based 下标
	for i, ln := range lines {
		if strings.TrimSpace(ln) == "" {
			if start < i { // 收块：start..i-1 连续非空行（1-based：start+1..i）
				ord++
				blocks = append(blocks, diffBlock{ord: ord, start: start + 1, end: i})
			}
			start = i + 1
		}
	}
	if start < len(lines) {
		ord++
		blocks = append(blocks, diffBlock{ord: ord, start: start + 1, end: len(lines)})
	}
	for k := range blocks {
		blocks[k].text = strings.Join(lines[blocks[k].start-1:blocks[k].end], "\n")
		blocks[k].ntext = diffNormalize(blocks[k].text, ign)
		blocks[k].rawDiff = blocks[k].text != blocks[k].ntext
	}
	return blocks
}

// diffOp 序列对齐操作：'=' 相同、'-' A 独有（删）、'+' B 独有（增）。
// ai/bi 为对应侧 slice 下标；'-' 时 bi=-1，'+' 时 ai=-1。
type diffOp struct {
	kind byte
	ai   int
	bi   int
}

// diffAlign 对齐两条字符串序列（块级 ntext / 块内 token 复用同一实现）。
// 公共前后缀先剔除（相同内容大头不进 LCS），剩余中段在阈值内走 DP(LCS) 精确对齐，
// 超阈值退化贪心（重复内容多/全不同的大段，保证不炸内存且顺序稳定），tool-diff.md §4 复杂度预算。
func diffAlign(a, b []string) []diffOp {
	la, lb := len(a), len(b)
	pre := 0
	for pre < la && pre < lb && a[pre] == b[pre] {
		pre++
	}
	suf := 0
	for suf < la-pre && suf < lb-pre && a[la-1-suf] == b[lb-1-suf] {
		suf++
	}
	ops := make([]diffOp, 0, la+lb)
	for i := 0; i < pre; i++ {
		ops = append(ops, diffOp{'=', i, i})
	}
	mid := diffAlignMid(a[pre:la-suf], b[pre:lb-suf])
	for _, op := range mid {
		switch op.kind {
		case '=':
			ops = append(ops, diffOp{'=', op.ai + pre, op.bi + pre})
		case '-':
			ops = append(ops, diffOp{'-', op.ai + pre, -1})
		default:
			ops = append(ops, diffOp{'+', -1, op.bi + pre})
		}
	}
	for i := 0; i < suf; i++ {
		ops = append(ops, diffOp{'=', la - suf + i, lb - suf + i})
	}
	return ops
}

// diffAlignDPCells DP 单元格上限：超出走贪心（防 O(N·M) 内存/时间爆炸）。
const diffAlignDPCells = 1 << 20

// diffAlignMid 中段对齐（下标相对输入序列）。
func diffAlignMid(a, b []string) []diffOp {
	if len(a) == 0 {
		ops := make([]diffOp, len(b))
		for j := range b {
			ops[j] = diffOp{'+', -1, j}
		}
		return ops
	}
	if len(b) == 0 {
		ops := make([]diffOp, len(a))
		for i := range a {
			ops[i] = diffOp{'-', i, -1}
		}
		return ops
	}
	if len(a)*len(b) <= diffAlignDPCells {
		return diffAlignDP(a, b)
	}
	return diffAlignGreedy(a, b)
}

// diffAlignDP LCS DP + 回溯（最小编辑脚本，确定性）。平局先删后增，稳定可测。
func diffAlignDP(a, b []string) []diffOp {
	n, m := len(a), len(b)
	w := m + 1
	dp := make([]int32, (n+1)*(m+1))
	for i := 1; i <= n; i++ {
		for j := 1; j <= m; j++ {
			if a[i-1] == b[j-1] {
				dp[i*w+j] = dp[(i-1)*w+(j-1)] + 1
			} else if dp[(i-1)*w+j] >= dp[i*w+(j-1)] {
				dp[i*w+j] = dp[(i-1)*w+j]
			} else {
				dp[i*w+j] = dp[i*w+(j-1)]
			}
		}
	}
	rev := make([]diffOp, 0, n+m)
	i, j := n, m
	for i > 0 && j > 0 {
		if a[i-1] == b[j-1] {
			rev = append(rev, diffOp{'=', i - 1, j - 1})
			i--
			j--
		} else if dp[(i-1)*w+j] >= dp[i*w+(j-1)] {
			rev = append(rev, diffOp{'-', i - 1, -1})
			i--
		} else {
			rev = append(rev, diffOp{'+', -1, j - 1})
			j--
		}
	}
	for ; i > 0; i-- {
		rev = append(rev, diffOp{'-', i - 1, -1})
	}
	for ; j > 0; j-- {
		rev = append(rev, diffOp{'+', -1, j - 1})
	}
	ops := make([]diffOp, len(rev))
	for k := range rev {
		ops[len(rev)-1-k] = rev[k]
	}
	return ops
}

// diffAlignGreedy 大段退化对齐：按 b 的位置表贪心取"当前位置起最近同值"，O(N log N)。
// 重复块多或两侧全不同时不保证最小编辑（可接受的次优：只影响噪音大小，不影响正确判同）。
func diffAlignGreedy(a, b []string) []diffOp {
	pos := make(map[string][]int, len(b))
	for j, s := range b {
		pos[s] = append(pos[s], j)
	}
	ops := make([]diffOp, 0, len(a)+len(b))
	bi := 0
	for ai, s := range a {
		if bi < len(b) && b[bi] == s { // 快路径：与当前 b 顺延相等
			ops = append(ops, diffOp{'=', ai, bi})
			bi++
			continue
		}
		ps := pos[s]
		p := sort.Search(len(ps), func(k int) bool { return ps[k] >= bi })
		if p < len(ps) {
			idx := ps[p]
			for ; bi < idx; bi++ {
				ops = append(ops, diffOp{'+', -1, bi})
			}
			ops = append(ops, diffOp{'=', ai, bi})
			bi++
		} else {
			ops = append(ops, diffOp{'-', ai, -1})
		}
	}
	for ; bi < len(b); bi++ {
		ops = append(ops, diffOp{'+', -1, bi})
	}
	return ops
}

// diffTok 细标 token：字/词粒度基本单元（tool-diff.md §4 步骤 4）。
type diffTok struct {
	text string
	s, e int // 在展示文本 rune 切片中的区间（含 s 不含 e）
}

// diffTokens 把块展示文本切成 token：CJK 逐字、拉丁/数字/下划线连写、其余单字符
// （标点/空白/符号在未忽略时也作 token 参与比对）。被 ignore 的字符直接剔除不参与比对。
// 展示文本 = 原文换行折叠为空格（行内展示，跨行噪音不刷屏；判同仍走原文 ntext）。
func diffTokens(text string, ign diffIgnore) ([]rune, []diffTok) {
	ra := []rune(text)
	var toks []diffTok
	for i := 0; i < len(ra); {
		if diffIgnoredRune(ra[i], ign) {
			i++
			continue
		}
		start := i
		if unicode.Is(unicode.Han, ra[i]) {
			i++ // CJK 逐字成 token（字级细标）
		} else if diffWordRune(ra[i]) {
			i++
			for i < len(ra) && diffWordRune(ra[i]) {
				i++ // 字母/数字/下划线连写成词 token
			}
		} else {
			i++ // 其余单字符 token（标点/空白/符号未忽略时也参与比对）
		}
		toks = append(toks, diffTok{text: string(ra[start:i]), s: start, e: i})
	}
	return ra, toks
}

// diffWordRune 文字类 rune（L* + Nd + '_'，不含 CJK——CJK 单独逐字处理）。
func diffWordRune(r rune) bool {
	return (unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_') && !unicode.Is(unicode.Han, r)
}

// diffInlineText 细标/回显用的块文本：换行折叠为空格（行内展示）。
func diffInlineText(text string) string { return strings.ReplaceAll(text, "\n", " ") }

// 细标展示的保守参数（tool-diff.md §4 步骤 4：防 O(N·M) 爆炸与超长刷屏）。
const (
	diffDetailMaxRunes = 4096 // 改动块对任一侧超此 rune 数：细标降级为段落级提示
	diffClusterCap     = 6    // 单块对内最多细标的散点簇数，超出给"另有 k 处"计数
	diffClusterCtx     = 6    // 差异片段两侧各带多少 rune 上下文
	diffClusterLen     = 200  // 单条簇展示最长 rune 数
	diffContentCap     = 300  // 整块展示（删/增块原文）最长 rune 数
	diffSampleCap      = 5    // 折叠（格式/排版）样例最多行数
)

// diffDetail 一次改动块对的细标：返回行内簇展示行、已展示簇数与总簇数、是否可细标。
// 超长块降级 ok=false（段落级提示引导 file_read 细读）。任一侧被忽略类剔光时，
// 整侧归为一个簇展示（替换语义仍成立）。
func diffDetail(aText, bText string, ign diffIgnore) (lines []string, emitted, total int, ok bool) {
	aDisp, bDisp := diffInlineText(aText), diffInlineText(bText)
	if utf8.RuneCountInString(aDisp) > diffDetailMaxRunes || utf8.RuneCountInString(bDisp) > diffDetailMaxRunes {
		return nil, 0, 0, false
	}
	aR, aToks := diffTokens(aDisp, ign)
	bR, bToks := diffTokens(bDisp, ign)
	switch {
	case len(aToks) == 0 && len(bToks) == 0:
		return nil, 0, 0, false // 理论不可达（ntext 不等），防御
	case len(aToks) == 0:
		// A 侧仅被忽略字符（全敏感下为空白行替换）：B 整段算一个新增簇。
		return []string{"B: " + diffClusterLine(bR, 0, len(bR))}, 1, 1, true
	case len(bToks) == 0:
		return []string{"A: " + diffClusterLine(aR, 0, len(aR))}, 1, 1, true
	}
	at := make([]string, len(aToks))
	for i, t := range aToks {
		at[i] = t.text
	}
	bt := make([]string, len(bToks))
	for i, t := range bToks {
		bt[i] = t.text
	}
	ops := diffAlign(at, bt)
	// 按连续非 '=' 段分组：每簇给 A 删 token 区间与 B 增 token 区间（簇内无 '=' 夹心，区间各自连续）。
	type clus struct{ aLo, aHi, bLo, bHi int }
	var cls []clus
	for i := 0; i < len(ops); {
		if ops[i].kind == '=' {
			i++
			continue
		}
		c := clus{aLo: 1 << 30, bLo: 1 << 30, aHi: -1, bHi: -1}
		for ; i < len(ops) && ops[i].kind != '='; i++ {
			if ops[i].kind == '-' {
				if ops[i].ai < c.aLo {
					c.aLo = ops[i].ai
				}
				if ops[i].ai > c.aHi {
					c.aHi = ops[i].ai
				}
			} else {
				if ops[i].bi < c.bLo {
					c.bLo = ops[i].bi
				}
				if ops[i].bi > c.bHi {
					c.bHi = ops[i].bi
				}
			}
		}
		cls = append(cls, c)
	}
	for _, c := range cls {
		if emitted >= diffClusterCap {
			break
		}
		if c.aLo <= c.aHi {
			lines = append(lines, "A: "+diffClusterLine(aR, aToks[c.aLo].s, aToks[c.aHi].e))
		}
		if c.bLo <= c.bHi {
			lines = append(lines, "B: "+diffClusterLine(bR, bToks[c.bLo].s, bToks[c.bHi].e))
		}
		emitted++
	}
	return lines, emitted, len(cls), true
}

// diffClusterLine 单侧散点簇的行内展示：『』标出差异片段，前后各带少量原文上下文。
func diffClusterLine(ra []rune, s, e int) string {
	ctxS := s - diffClusterCtx
	if ctxS < 0 {
		ctxS = 0
	}
	ctxE := e + diffClusterCtx
	if ctxE > len(ra) {
		ctxE = len(ra)
	}
	var b strings.Builder
	if ctxS > 0 {
		b.WriteString("…") // 变化点之前还有原文未展示
	}
	b.WriteString(string(ra[ctxS:s]))
	b.WriteString("『")
	b.WriteString(string(ra[s:e]))
	b.WriteString("』")
	b.WriteString(string(ra[e:ctxE]))
	if ctxE < len(ra) {
		b.WriteString("…") // 变化点之后还有原文未展示
	}
	return truncateRunes(b.String(), diffClusterLen)
}

// diffEntry 一条实质差异明细（'d' 删 / 'i' 增 / 'm' 改）。
type diffEntry struct {
	kind byte
	a, b *diffBlock // 'm' 两侧成对；'d'/'i' 只给单侧
}

// diffFoldSample 一条折叠样例（格式差异 / 切分排版）：只给位置与类别，不展开内容。
type diffFoldSample struct {
	label                      string
	aStart, aEnd, bStart, bEnd int
}

// runFileDiff 文件比对执行逻辑（外置具名函数，可独立单测）。
func runFileDiff(cfg *FileConfig, args json.RawMessage) (tool.Result, error) {
	var in struct {
		FileA  string   `json:"file_a"`
		FileB  string   `json:"file_b"`
		Ignore []string `json:"ignore"`
		Limit  *int     `json:"limit"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return tool.Result{Data: "参数错误: " + err.Error(), IsError: true}, nil
	}
	a, b := strings.TrimSpace(in.FileA), strings.TrimSpace(in.FileB)
	if a == "" || b == "" {
		return tool.Result{Data: "参数错误: file_a 与 file_b 均为必填", IsError: true}, nil
	}
	var ign diffIgnore
	for _, name := range in.Ignore {
		switch strings.TrimSpace(name) {
		case "space":
			ign |= ignSpace
		case "punct":
			ign |= ignPunct
		case "symbol":
			ign |= ignSymbol
		default:
			return tool.Result{Data: "参数错误: ignore 只支持 space/punct/symbol，got " + name, IsError: true}, nil
		}
	}
	limit := 10
	if in.Limit != nil && *in.Limit > 0 {
		limit = *in.Limit
	}

	linesA, errA := diffReadFile(cfg, a)
	if errA != "" {
		return tool.Result{Data: "读取 A 失败: " + errA, IsError: true}, nil
	}
	linesB, errB := diffReadFile(cfg, b)
	if errB != "" {
		return tool.Result{Data: "读取 B 失败: " + errB, IsError: true}, nil
	}
	head := diffHead(a, b, linesA, linesB)
	if diffLinesEqual(linesA, linesB) {
		return tool.Result{Data: head + "两文件内容相同，无差异", IsError: false}, nil
	}
	blocksA := diffSplitBlocks(linesA, ign)
	blocksB := diffSplitBlocks(linesB, ign)

	// 块级对齐（tool-diff.md §4 步骤 3）。
	na := make([]string, len(blocksA))
	for i := range blocksA {
		na[i] = blocksA[i].ntext
	}
	nb := make([]string, len(blocksB))
	for i := range blocksB {
		nb[i] = blocksB[i].ntext
	}
	ops := diffAlign(na, nb)

	var entries []diffEntry
	var fmtSamples, reorderSamples []diffFoldSample
	fmtEq, reorderN := 0, 0

	flushRegion := func(da, ib []int) {
		// da/ib：改动区内 A 删块与 B 增块的块下标（保持各自出现顺序）。
		if len(da) == 0 && len(ib) == 0 {
			return
		}
		if len(da) > 0 && len(ib) > 0 {
			// 切分/重排判定：两侧归一化拼接相等 → 内容没动，只是块边界/顺序不同
			// （v1 不做移动追踪，tool-diff.md §2 设计原则）。
			var ca, cb strings.Builder
			for _, k := range da {
				ca.WriteString(blocksA[k].ntext)
			}
			for _, k := range ib {
				cb.WriteString(blocksB[k].ntext)
			}
			if ca.String() == cb.String() {
				reorderN++
				if len(reorderSamples) < diffSampleCap {
					reorderSamples = append(reorderSamples, diffFoldSample{
						label:  "排版",
						aStart: blocksA[da[0]].start, aEnd: blocksA[da[len(da)-1]].end,
						bStart: blocksB[ib[0]].start, bEnd: blocksB[ib[len(ib)-1]].end,
					})
				}
				return
			}
		}
		// 实质差异：按顺序把 A 删块与 B 增块两两配对成"改"，多余单侧为删/增。
		k := len(da)
		if len(ib) < k {
			k = len(ib)
		}
		for p := 0; p < k; p++ {
			entries = append(entries, diffEntry{kind: 'm', a: &blocksA[da[p]], b: &blocksB[ib[p]]})
		}
		for _, idx := range da[k:] {
			entries = append(entries, diffEntry{kind: 'd', a: &blocksA[idx]})
		}
		for _, idx := range ib[k:] {
			entries = append(entries, diffEntry{kind: 'i', b: &blocksB[idx]})
		}
	}
	var da, ib []int
	for _, op := range ops {
		switch op.kind {
		case '=':
			// 已匹配但 raw 有差异（差异全在被忽略类）→ 格式折叠计数。
			if blocksA[op.ai].rawDiff {
				fmtEq++
				if len(fmtSamples) < diffSampleCap {
					fmtSamples = append(fmtSamples, diffFoldSample{
						label:  "格式",
						aStart: blocksA[op.ai].start, aEnd: blocksA[op.ai].end,
						bStart: blocksB[op.bi].start, bEnd: blocksB[op.bi].end,
					})
				}
			}
			flushRegion(da, ib)
			da, ib = nil, nil
		case '-':
			da = append(da, op.ai)
		default:
			ib = append(ib, op.bi)
		}
	}
	flushRegion(da, ib)

	return tool.Result{
		Data: diffAssemble(head, fmtEq, reorderN, fmtSamples, reorderSamples, entries, ign,
			limit, in.Limit != nil && *in.Limit <= 0),
		IsError: false,
	}, nil
}

// diffHead 报告头部（A/B 路径 + 行数）。
func diffHead(a, b string, linesA, linesB []string) string {
	return fmt.Sprintf("file_diff: A=%s（%d 行）B=%s（%d 行）\n", a, len(linesA), b, len(linesB))
}

// diffAssemble 组装最终报告：头 + 统计 + 图例 + 明细 + 未展开提示 + 折叠样例（tool-diff.md §5）。
func diffAssemble(head string, fmtEq, reorderN int, fmtSamples, reorderSamples []diffFoldSample,
	entries []diffEntry, ign diffIgnore, limit int, unlimited bool) string {
	var out strings.Builder
	out.WriteString(head)

	nc, nd, ni := 0, 0, 0
	for _, e := range entries {
		switch e.kind {
		case 'm':
			nc++
		case 'd':
			nd++
		default:
			ni++
		}
	}
	if len(entries) == 0 {
		out.WriteString(fmt.Sprintf("无实质内容差异；格式/排版差异 %d 处（空白/标点/符号/切分，已折叠）\n", fmtEq+reorderN))
	} else {
		out.WriteString(fmt.Sprintf("实质差异 %d 处：改 %d、删 %d、增 %d；格式/排版差异 %d 处（已折叠）\n",
			nc+nd+ni, nc, nd, ni, fmtEq+reorderN))
	}

	shown := 0
	skipN := 0
	legendDone := false
	for i := range entries {
		e := &entries[i]
		if !unlimited && shown >= limit { // 明细条数上限（统计已含全部，这里只截展开）
			skipN++
			continue
		}
		switch e.kind {
		case 'm':
			if !legendDone {
				out.WriteString("（『』内为差异片段：A 侧为删除内容、B 侧为新增内容；… 为省略）\n")
				legendDone = true
			}
			aB, bB := e.a, e.b
			out.WriteString(fmt.Sprintf("[改] A:第 %d-%d 行 ↔ B:第 %d-%d 行（A 第 %d 段）\n", aB.start, aB.end, bB.start, bB.end, aB.ord))
			cls, emitted, total, ok := diffDetail(aB.text, bB.text, ign)
			if !ok {
				out.WriteString("改动面大，建议 file_read 窗口细读\n")
			} else {
				for _, ln := range cls {
					out.WriteString(ln + "\n")
				}
				if total > emitted {
					out.WriteString(fmt.Sprintf("…该段内另有 %d 处散点差异\n", total-emitted))
				}
			}
			shown++
		case 'd':
			out.WriteString(fmt.Sprintf("[删] A:第 %d-%d 行（A 第 %d 段）\n", e.a.start, e.a.end, e.a.ord))
			out.WriteString("A: " + truncateRunes(e.a.text, diffContentCap) + "\n")
			shown++
		default:
			out.WriteString(fmt.Sprintf("[增] B:第 %d-%d 行（B 第 %d 段）\n", e.b.start, e.b.end, e.b.ord))
			out.WriteString("B: " + truncateRunes(e.b.text, diffContentCap) + "\n")
			shown++
		}
	}
	if skipN > 0 {
		out.WriteString(fmt.Sprintf("另有 %d 处实质差异未展开（可传 limit 加大）\n", skipN))
	}
	// 折叠样例（尾部，至多各 5 条一行）：只给位置与类别，不展开内容。
	for _, s := range fmtSamples {
		out.WriteString(fmt.Sprintf("[%s] A:第 %d-%d 行 ↔ B:第 %d-%d 行（仅忽略类差异）\n",
			s.label, s.aStart, s.aEnd, s.bStart, s.bEnd))
	}
	for _, s := range reorderSamples {
		out.WriteString(fmt.Sprintf("[%s] A:第 %d-%d 行 ↔ B:第 %d-%d 行（切分/重排，内容一致）\n",
			s.label, s.aStart, s.aEnd, s.bStart, s.bEnd))
	}
	return strings.TrimSuffix(out.String(), "\n")
}

// truncateRunes 按 rune 截断（字节切分可能切坏 UTF-8）。
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…（截断）"
}

// diffLinesEqual 两侧文件内容是否完全相同（行级逐条比较，判同不依赖 ignore）。
func diffLinesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// diffReadFile 读取并预检比对文件（与 file_read 同一路径/大小/.env 语义，tool-fs.md §5）。
// 返回 error 文本（空串 = 成功），把业务失败留在 Result 而非向上抛。
func diffReadFile(cfg *FileConfig, p string) ([]string, string) {
	abs, err := resolveReadPath(cfg.Root, p)
	if err != nil {
		return nil, err.Error()
	}
	if strings.HasPrefix(filepath.Base(abs), ".env") {
		return nil, "[file_diff: 敏感文件不比对（.env 防护，fail-closed）]"
	}
	if fi, err := os.Stat(abs); err != nil {
		return nil, err.Error()
	} else if fi.IsDir() {
		return nil, "是目录，需要文件路径"
	} else if fi.Size() > maxFileReadSize {
		return nil, "文件过大（>10MB），建议 doc_search 定位后窗口化读"
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, err.Error()
	}
	if strings.IndexByte(string(data), 0) >= 0 {
		return nil, "疑似二进制文件，不支持比对"
	}
	raw := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	lines := make([]string, len(raw))
	for i, ln := range raw {
		lines[i] = strings.TrimSuffix(ln, "\r") // \r\n 行尾归一（判空与展示双受益）
	}
	return lines, ""
}
