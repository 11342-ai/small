package builtin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"small/internal/tool"
)

// 文档解析工具（doc_parse/doc_read/doc_clean），见 Zoo/model/tool-lit.md。
// 形态：封装本地 CLI `lit`（LlamaIndex LiteParse）的常用命令为固定参数模板，
// 产物落系统缓存目录（config.cache_dir，缺省 ~/.small/cache），同源同内容重复
// 解析自动命中复用（parse once 的工具化落地，降低重复劳动）。
// 与 Echo() 同款构造风格：一工具一构造函数，执行逻辑外置具名函数可独立单测。

// LitConfig 文档解析工具的缓存根配置。
// Root 为缓存目录：doc_parse 产物落盘、doc_clean 清洗、doc_read 读取都限定在该
// 目录内（对齐 memory 部件"目录内读"模式；写受控缓存目录 = Pass 权限，tool-lit.md §5）。
// Root 为空时工具不注册（退化，同 mem/exec 惯例）。
type LitConfig struct {
	Root string
}

// defaultLitTimeout doc_parse 单次解析超时：5 分钟。born-digital 秒级，扫描件慢
// （tool-lit.md §9 决策 4）。
const defaultLitTimeout = 5 * time.Minute

// litInstallHint lit 缺失时的安装指引（工具只检查不自动安装，无网络安装动作）。
const litInstallHint = "未找到 lit 命令，请先安装: npm i -g @llamaindex/liteparse（Office 文档需 LibreOffice，图片需 ImageMagick）"

// docPageBreakRe 分页符行：整行仅由 3 个以上 '-'（含可选空白）构成（PDF 分页残留）。
var docPageBreakRe = regexp.MustCompile(`^-{3,}$`)

// docPageNumRe 页码行：整行为"第 N 页"（解析分页残留，tool-lit.md §2）。
var docPageNumRe = regexp.MustCompile(`^第\s*\d+\s*页$`)

// docDashedPageRe 分页页码残留（pdf-workflow.md §3 规则 7）：整行为横线包数字
// 的页眉页脚（"－70－"、"- 70 -"、"——12——" 等，全角/半角横线，含空白）。
var docDashedPageRe = regexp.MustCompile(`^[－\-—]+\s*\d+\s*[－\-—]+$`)

// cjkSpaceRe CJK 字间空格压缩（pdf-workflow.md §3 规则 6）：删除 CJK 字符
// （含全角标点）之间的空格，修复逐字拆分（"第 一 百 四 十 一 条" → 连续中文）。
// 反复替换直到稳定：单轮替换会因"跳过已处理位置"漏掉部分空格（见 docFoldCJKSpace）。
var cjkSpaceRe = regexp.MustCompile(`([\p{Han}\x{FF00}-\x{FFEF}\x{3000}-\x{303F}])\s+([\p{Han}\x{FF00}-\x{FFEF}\x{3000}-\x{303F}])`)

// docCacheExt 按 format 映射产物扩展名：text → .md（用户习惯 .md 命名），json → .json。
func docCacheExt(format string) string {
	if format == "json" {
		return ".json"
	}
	return ".md"
}

// docCacheName 产物命名：<basename>.<sha256(绝对路径)前12位>.<ext>（tool-lit.md §4）。
// 对路径做 hash（而非内容）：同路径重复解析必命中；内容覆盖属用户变更，重新解析
// 时产物名不变但会覆盖旧产物（缓存只存最近一次解析结果，够用不膨胀）。
func docCacheName(src, format string) string {
	h := sha256.Sum256([]byte(filepath.Clean(src)))
	base := strings.TrimSuffix(filepath.Base(src), filepath.Ext(src))
	return base + "." + hex.EncodeToString(h[:6]) + docCacheExt(format)
}

// resolveInCache 把参数路径解析为缓存目录内的绝对路径（前缀约束，防读/写任意文件）。
// 相对路径按 Root 解析；绝对路径须落在 Root 内（对齐 file 工具的 resolveInRoot 语义）。
func resolveInCache(root, p string) (string, error) {
	if root == "" {
		return "", fmt.Errorf("缓存目录未配置")
	}
	var abs string
	if filepath.IsAbs(p) {
		abs = filepath.Clean(p)
	} else {
		abs = filepath.Clean(filepath.Join(root, p))
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("路径越界：%q 不在缓存目录（%s）内", p, root)
	}
	return abs, nil
}

// LitParse 构造文档解析工具（doc_parse）：调 lit 把 PDF/DOCX 等解析为文本落缓存。
func LitParse(cfg *LitConfig) tool.Tool {
	return tool.NewFunc(
		tool.Spec{
			Name:        "doc_parse",
			Description: "用 lit 本地解析文档（PDF/DOCX/PPTX/XLSX/图片，libreoffice 自动转换）为文本，产物落系统缓存目录（~/.small/cache）。source 为文档绝对路径（任意位置）；pages 可限定页范围（如 1-5,10）；ocr 默认 false（born-digital 快且同质，扫描件/图片需 true）；format 默认 text（json 仅当需要版式坐标时用）。同源同内容重复解析自动命中缓存。返回产物路径、行数/字数与摘要；产物可能含水印/分页符噪声，可调 doc_clean 清洗。",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"source": {"type": "string", "description": "文档绝对路径（任意位置）"},
					"pages": {"type": "string", "description": "限定解析页范围，如 1-5,10；缺省全文档"},
					"ocr": {"type": "boolean", "description": "是否启用 OCR（扫描件/图片为 true；born-digital 缺省 false 更快）"},
					"format": {"type": "string", "description": "输出格式：text（缺省）或 json"}
				},
				"required": ["source"]
			}`),
		},
		func(ctx context.Context, args json.RawMessage) (tool.Result, error) {
			return runLitParse(ctx, cfg, args)
		},
	)
}

// runLitParse doc_parse 执行逻辑（外置具名函数，可脱离工具壳独立单测）。
func runLitParse(ctx context.Context, cfg *LitConfig, args json.RawMessage) (tool.Result, error) {
	var in struct {
		Source string `json:"source"`
		Pages  string `json:"pages"`
		Ocr    bool   `json:"ocr"`
		Format string `json:"format"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return tool.Result{Data: "参数错误: " + err.Error(), IsError: true}, nil
	}
	src := strings.TrimSpace(in.Source)
	if src == "" {
		return tool.Result{Data: "参数错误: source 为空", IsError: true}, nil
	}
	format := strings.TrimSpace(in.Format)
	if format == "" {
		format = "text"
	}
	if format != "text" && format != "json" {
		return tool.Result{Data: "参数错误: format 仅支持 text/json", IsError: true}, nil
	}
	fi, err := os.Stat(src)
	if err != nil {
		return tool.Result{Data: "源文件不存在: " + err.Error(), IsError: true}, nil
	}
	if fi.IsDir() {
		return tool.Result{Data: "参数错误: source 是目录，应为文档文件", IsError: true}, nil
	}
	// 先算产物路径并检查缓存命中：命中时无需 lit 也无需建目录（产物在则目录必在）。
	out := filepath.Join(cfg.Root, docCacheName(src, format))
	if _, err := os.Stat(out); err == nil {
		return docParseResult(out, true)
	}
	// lit 依赖检查（fail-closed）：缺失给安装指引，不自动安装（tool-lit.md §5）。
	if _, err := exec.LookPath("lit"); err != nil {
		return tool.Result{Data: litInstallHint, IsError: true}, nil
	}
	if err := os.MkdirAll(cfg.Root, 0o755); err != nil {
		return tool.Result{Data: "缓存目录不可用: " + err.Error(), IsError: true}, nil
	}
	// 未命中：组装 lit parse 命令（参数模板固定，模型只提供选项；刻意不用 shell）。
	argv := []string{"parse", src, "--format", format, "-o", out}
	if !in.Ocr {
		argv = append(argv, "--no-ocr")
	}
	if p := strings.TrimSpace(in.Pages); p != "" {
		argv = append(argv, "--target-pages", p)
	}
	runCtx, cancel := context.WithTimeout(ctx, defaultLitTimeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, "lit", argv...)
	cmdOut, err := cmd.CombinedOutput()
	if err != nil {
		if runCtx.Err() == context.DeadlineExceeded {
			return tool.Result{Data: "doc_parse 超时（超过 5 分钟，大扫描件可只解析部分页 pages）", IsError: true}, nil
		}
		return tool.Result{Data: "doc_parse 失败: " + err.Error() + "\n" + truncateOutput(string(cmdOut)), IsError: true}, nil
	}
	return docParseResult(out, false)
}

// docParseResult 汇总解析产物：路径 + 行数/字符数 + 摘要（前 40 行）+ 噪声提示。
func docParseResult(out string, hit bool) (tool.Result, error) {
	data, err := os.ReadFile(out)
	if err != nil {
		return tool.Result{Data: "读取产物失败: " + err.Error(), IsError: true}, nil
	}
	lines := strings.Split(string(data), "\n")
	nonEmpty := 0
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			nonEmpty++
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "doc_parse: 解析完成（缓存命中: %v）\n", hit)
	fmt.Fprintf(&b, "缓存文件: %s\n", out)
	fmt.Fprintf(&b, "行数: %d（非空 %d）| 字符数: %d\n", len(lines), nonEmpty, len(string(data)))
	b.WriteString("摘要（前 40 行）：\n")
	preview := strings.TrimSpace(string(data))
	if preview == "" {
		preview = "（产物为空）"
	}
	// 摘要在报告内独立截断，避免大产物撑爆整体输出。
	if pl := strings.Split(preview, "\n"); len(pl) > 40 {
		preview = strings.Join(pl[:40], "\n") + "\n…（后续可 doc_read 窗口化阅读）"
	}
	fmt.Fprintf(&b, "%s\n", preview)
	b.WriteString("提示: 产物可能含水印/分页符/页码残留噪声，可调 doc_clean 清洗后阅读。")
	return tool.Result{Data: truncateOutput(b.String())}, nil
}

// LitRead 构造缓存产物读取工具（doc_read）：file_read 受工作区约束读不了缓存目录，
// 读口必须是本工具（对齐 memory_get 读 memory 目录的部件内读模式，tool-lit.md §4）。
func LitRead(cfg *LitConfig) tool.Tool {
	return tool.NewFunc(
		tool.Spec{
			Name:        "doc_read",
			Description: "读取 doc_parse 解析产物（缓存目录内文件），带行号，支持 offset/limit 窗口化（offset 起始行 1-based，limit 行数；超长截断并提示续读）。path 为 doc_parse 返回的缓存文件路径（相对或绝对皆可，仅限缓存目录内）。用于细读解析出的文档内容。",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"path": {"type": "string", "description": "缓存产物路径（doc_parse 返回的绝对路径或相对缓存根）"},
					"offset": {"type": "integer", "description": "起始行号（1-based，缺省 1）"},
					"limit": {"type": "integer", "description": "读取行数（缺省读到文件尾）"}
				},
				"required": ["path"]
			}`),
		},
		func(_ context.Context, args json.RawMessage) (tool.Result, error) {
			return runLitRead(cfg, args)
		},
	)
}

// runLitRead doc_read 执行逻辑：窗口化读取，对齐 file_read 语义（带行号 + 续读提示）。
func runLitRead(cfg *LitConfig, args json.RawMessage) (tool.Result, error) {
	var in struct {
		Path   string `json:"path"`
		Offset *int   `json:"offset"`
		Limit  *int   `json:"limit"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return tool.Result{Data: "参数错误: " + err.Error(), IsError: true}, nil
	}
	abs, err := resolveInCache(cfg.Root, strings.TrimSpace(in.Path))
	if err != nil {
		return tool.Result{Data: err.Error(), IsError: true}, nil
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return tool.Result{Data: "读取失败: " + err.Error(), IsError: true}, nil
	}
	lines := strings.Split(string(data), "\n")
	total := len(lines)
	start := 1
	if in.Offset != nil && *in.Offset > 1 {
		start = *in.Offset
	}
	if start > total {
		return tool.Result{Data: fmt.Sprintf("[doc_read: 共 %d 行，offset=%d 超出]", total, start), IsError: true}, nil
	}
	end := total
	if in.Limit != nil && *in.Limit > 0 {
		end = start + *in.Limit - 1
		if end > total {
			end = total
		}
	}
	var b strings.Builder
	for i := start - 1; i < end; i++ {
		fmt.Fprintf(&b, "%5d│ %s\n", i+1, lines[i])
	}
	out := strings.TrimSuffix(b.String(), "\n")
	if end < total {
		out += fmt.Sprintf("\n（继续可 offset=%d）", end+1)
	}
	return tool.Result{Data: truncateOutput(out)}, nil
}

// LitClean 构造清洗工具（doc_clean）：对缓存产物做确定性规则清洗（分页符/页码/
// 行内重复水印/相邻重复/空行折叠），原地改写，返回清洗报告（tool-lit.md §3.3）。
func LitClean(cfg *LitConfig) tool.Tool {
	return tool.NewFunc(
		tool.Spec{
			Name:        "doc_clean",
			Description: "清洗 doc_parse 解析产物中的确定性噪声（原地改写缓存文件，仅限缓存目录内）：删除分页符横线行、页码残留行（第 N 页）、折叠行内重复水印词组（如 人民法院案例库 人民法院案例库）、相邻完全相同行、连续空行。返回各规则删除计数。规则保守，语义类问题（换行粘连/数字空格/双栏重排）需在整理阶段手动修复。",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"path": {"type": "string", "description": "缓存产物路径（doc_parse 返回的绝对路径或相对缓存根）"}
				},
				"required": ["path"]
			}`),
		},
		func(_ context.Context, args json.RawMessage) (tool.Result, error) {
			return runLitClean(cfg, args)
		},
	)
}

// runLitClean doc_clean 执行逻辑：读 → 逐行过规则 → 原地写回 → 返回报告。
func runLitClean(cfg *LitConfig, args json.RawMessage) (tool.Result, error) {
	var in struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return tool.Result{Data: "参数错误: " + err.Error(), IsError: true}, nil
	}
	abs, err := resolveInCache(cfg.Root, strings.TrimSpace(in.Path))
	if err != nil {
		return tool.Result{Data: err.Error(), IsError: true}, nil
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return tool.Result{Data: "读取失败: " + err.Error(), IsError: true}, nil
	}
	cleaned, counts := cleanDocLines(strings.Split(string(data), "\n"))
	if err := os.WriteFile(abs, []byte(strings.Join(cleaned, "\n")), 0o644); err != nil {
		return tool.Result{Data: "写入清洗结果失败: " + err.Error(), IsError: true}, nil
	}
	var b strings.Builder
	b.WriteString("doc_clean: 清洗完成\n")
	fmt.Fprintf(&b, "- 分页符行: %d\n", counts.pageBreaks)
	fmt.Fprintf(&b, "- 页码行: %d\n", counts.pageNums)
	fmt.Fprintf(&b, "- 横线页码残留行: %d\n", counts.dashedPages)
	fmt.Fprintf(&b, "- 行内重复折叠: %d\n", counts.inlineDup)
	fmt.Fprintf(&b, "- 相邻重复行: %d\n", counts.adjacentDup)
	fmt.Fprintf(&b, "- 空行折叠: %d\n", counts.blankFold)
	fmt.Fprintf(&b, "- CJK 字间空格压缩: %d 处\n", counts.cjkSpace)
	if counts.pageBreaks+counts.pageNums+counts.dashedPages+counts.inlineDup+counts.adjacentDup+counts.blankFold+counts.cjkSpace == 0 {
		b.WriteString("未发现可清洗噪声。")
	} else {
		b.WriteString("提示: 语义类问题（段落合并/词间距/双栏重排/数字空格）不在本工具处理范围，需 AI 整理阶段修复（见 SKILL）。")
	}
	return tool.Result{Data: b.String()}, nil
}

// docCleanCounts 各规则删除计数（doc_clean 报告用）。
type docCleanCounts struct {
	pageBreaks  int // 分页符行
	pageNums    int // 页码行
	dashedPages int // 横线页码残留行（－70－）
	inlineDup   int // 行内重复折叠
	adjacentDup int // 相邻重复行
	blankFold   int // 空行折叠
	cjkSpace    int // CJK 字间空格压缩数（删除的空格数）
}

// cleanDocLines 确定性清洗主流程：逐行过规则（tool-lit.md §3.3 + pdf-workflow.md §3）。
// 规则 1 分页符行、规则 2 页码行、规则 7 横线页码残留行删除；规则 3 行内紧邻重复折叠
// （水印）；规则 4 相邻完全相同非空行去重；规则 5 连续空行折叠为 1；规则 6 CJK 字间
// 空格压缩（放行尾处理，避免破坏规则 3 的按空格分词）；末尾空行收掉。
func cleanDocLines(lines []string) ([]string, docCleanCounts) {
	var counts docCleanCounts
	var out []string
	for _, raw := range lines {
		line := strings.TrimRight(raw, "\r")
		trim := strings.TrimSpace(line)
		if docPageBreakRe.MatchString(trim) {
			counts.pageBreaks++
			continue
		}
		if docPageNumRe.MatchString(trim) {
			counts.pageNums++
			continue
		}
		if docDashedPageRe.MatchString(trim) {
			counts.dashedPages++
			continue
		}
		if folded, ok := docFoldInlineRepeat(trim); ok {
			counts.inlineDup++
			line, trim = folded, folded
		}
		if prev := strings.TrimSpace(peekLast(out)); prev != "" && prev == trim {
			counts.adjacentDup++
			continue
		}
		if trim == "" && strings.TrimSpace(peekLast(out)) == "" {
			counts.blankFold++
			continue
		}
		// 规则 6：CJK 字间空格压缩（逐字拆分修复），只改本行不删行。
		if !strings.Contains(line, " ") && !strings.Contains(line, "\t") {
			counts.cjkSpace += 0 // 无空白无需压缩（避免正则空跑）
		} else if comp, removed := docFoldCJKSpace(line); removed > 0 {
			counts.cjkSpace += removed
			line = comp
		}
		out = append(out, line)
	}
	for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
		out = out[:len(out)-1]
	}
	return out, counts
}

// docFoldCJKSpace 删除 CJK 字符/全角标点之间的空格，返回压缩后文本与删除的空格数。
// 反复替换直到稳定：regexp 非重叠匹配会跳过相邻匹配间的字符，单轮会漏删
// （"第 一 百" 首轮只删"第 一"，需第二轮删"一 百"），循环收敛即可。
func docFoldCJKSpace(line string) (string, int) {
	removed := 0
	for {
		next := cjkSpaceRe.ReplaceAllString(line, "${1}${2}")
		if next == line {
			return line, removed
		}
		removed++
		line = next
	}
}

// peekLast 取切片末元素（空切片返回空串，避免越界分支重复）。
func peekLast(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return s[len(s)-1]
}

// docFoldInlineRepeat 折叠行内紧邻重复片段：按空白切 token，从最长段开始找紧邻
// 重复并删除后一段（防 "a b a b" 被误叠成 "a b a"），折叠后重扫当前位置。
// 失败返回原行（含"无重复"与"无折叠收益"两种情况）。
func docFoldInlineRepeat(line string) (string, bool) {
	toks := strings.Fields(line)
	if len(toks) < 2 {
		return line, false
	}
	folded := false
	i := 0
	for i < len(toks) {
		maxK := (len(toks) - i) / 2
		repeated := false
		for k := maxK; k >= 1; k-- {
			if sliceEq(toks[i:i+k], toks[i+k:i+2*k]) {
				toks = append(toks[:i+k], toks[i+2*k:]...)
				folded = true
				repeated = true
				break // 当前位置已折叠，i 不动继续检查（可能还有后续重复）
			}
		}
		if !repeated {
			i++
		}
	}
	if !folded {
		return line, false
	}
	return strings.Join(toks, " "), true
}

// sliceEq 两字符串切片相等。
func sliceEq(a, b []string) bool {
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
