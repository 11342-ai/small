package builtin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"small/internal/tool"
)

// ---------- 纯函数单测 ----------

func TestDiffNormalize(t *testing.T) {
	all := ignSpace | ignPunct | ignSymbol
	cases := []struct {
		name  string
		in    string
		ign   diffIgnore
		equal string
	}{
		{"全敏感原样返回", "a b，c。d①e🔥", 0, "a b，c。d①e🔥"},
		{"忽略空白标点符号", "a b，c。d①e🔥\t\n", all, "abcde"},
		{"只忽略标点", "a，b。c", ignPunct, "abc"},
		{"只忽略符号（圈码）", "d①e②f", ignSymbol, "def"},
		{"空白类含全角空格与换行", "x\u3000y\nz", ignSpace, "xyz"},
		{"CJK 与全角数字不被忽略", "第一百四十一条１２３", all, "第一百四十一条１２３"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := diffNormalize(c.in, c.ign); got != c.equal {
				t.Errorf("diffNormalize(%q, %d) = %q, want %q", c.in, c.ign, got, c.equal)
			}
		})
	}
}

func TestDiffSplitBlocks(t *testing.T) {
	// 空行（含纯空白行）为块边界，行号与块内容正确。
	lines := []string{"甲", "乙", "   ", "丙", "", "丁"}
	blocks := diffSplitBlocks(lines, 0)
	if len(blocks) != 3 {
		t.Fatalf("got %d blocks, want 3", len(blocks))
	}
	want := []struct {
		start, end int
		text       string
	}{{1, 2, "甲\n乙"}, {4, 4, "丙"}, {6, 6, "丁"}}
	for i, w := range want {
		b := blocks[i]
		if b.start != w.start || b.end != w.end || b.text != w.text {
			t.Errorf("block %d = {start:%d end:%d text:%q}, want {%d %d %q}",
				i, b.start, b.end, b.text, w.start, w.end, w.text)
		}
		if b.ord != i+1 {
			t.Errorf("block %d ord = %d, want %d", i, b.ord, i+1)
		}
	}
	// 纯空文件/纯空白文件：零块。
	if n := len(diffSplitBlocks([]string{}, 0)); n != 0 {
		t.Errorf("空文件应零块，got %d", n)
	}
	if n := len(diffSplitBlocks([]string{"", "   "}, 0)); n != 0 {
		t.Errorf("纯空白文件应零块，got %d", n)
	}
	// rawDiff：仅被忽略类有差异时置位（判格式折叠）。
	bs := diffSplitBlocks([]string{"a b"}, ignSpace)
	if len(bs) != 1 || !bs[0].rawDiff || bs[0].ntext != "ab" {
		t.Errorf("rawDiff 判定错误: %+v", bs)
	}
}

func opKinds(ops []diffOp) string {
	var b strings.Builder
	for _, op := range ops {
		b.WriteByte(op.kind)
	}
	return b.String()
}

func TestDiffAlignDP(t *testing.T) {
	cases := []struct {
		name string
		a, b []string
		want string // kind 序列
	}{
		{"全相同", []string{"x", "y"}, []string{"x", "y"}, "=="},
		{"中插", []string{"x", "y"}, []string{"x", "z", "y"}, "=+="},
		{"纯增", []string{}, []string{"a", "b"}, "++"},
		{"纯删", []string{"a", "b"}, []string{}, "--"},
		{"替换", []string{"x", "y"}, []string{"x", "z"}, "=+-"}, // 同锚点先增后删（最小脚本，位置等价）
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := opKinds(diffAlign(c.a, c.b)); got != c.want {
				t.Fatalf("diffAlign(%v, %v) = %q, want %q", c.a, c.b, got, c.want)
			}
		})
	}
}

func TestDiffAlignTrimCommon(t *testing.T) {
	// 公共前后缀直接对齐（不进 DP），中段 a 换 c、b 保留：= + - = = 同锚点先增后删。
	if got := opKinds(diffAlign([]string{"p", "a", "b", "s"}, []string{"p", "c", "b", "s"})); got != "=+-==" {
		t.Fatalf("got %q, want %q", got, "=+-==")
	}
}

func TestDiffAlignGreedy(t *testing.T) {
	// 重复符号：贪心按位置表顺序匹配（确定性）。
	a := []string{"a", "b", "a"}
	b := []string{"a", "a", "b"}
	if got := opKinds(diffAlignGreedy(a, b)); got != "=+=-" {
		t.Fatalf("got %q, want %q", got, "=+=-")
	}
}

func TestDiffTokens(t *testing.T) {
	// CJK 逐字、拉丁/数字连写、标点单字。
	ra, toks := diffTokens("a2bc王。", 0)
	if string(ra) != "a2bc王。" {
		t.Fatalf("rune 序列异常: %q", string(ra))
	}
	var texts []string
	for _, tk := range toks {
		texts = append(texts, tk.text)
	}
	if got := strings.Join(texts, "|"); got != "a2bc|王|。" {
		t.Fatalf("token 序列 = %q, want %q", got, "a2bc|王|。")
	}
	// 被忽略类别剔除：不产生 token。
	_, toks2 := diffTokens("a，b ①c", ignPunct|ignSpace|ignSymbol)
	texts = texts[:0]
	for _, tk := range toks2 {
		texts = append(texts, tk.text)
	}
	if got := strings.Join(texts, "|"); got != "a|b|c" {
		t.Fatalf("忽略后 token 序列 = %q, want %q", got, "a|b|c")
	}
}

func TestDiffClusterLine(t *testing.T) {
	// 中段差异带两侧上下文；首/尾差异不误加省略号。
	ra := []rune("一二三四五六七八九十")
	got := diffClusterLine(ra, 4, 6) // 五 六 被『』标出
	if !strings.Contains(got, "『五六』") || strings.HasPrefix(got, "…") || strings.HasSuffix(got, "…") {
		t.Fatalf("中段簇展示异常: %q", got)
	}
	got2 := diffClusterLine(ra, 8, 9) // 九 位于末尾附近
	if !strings.Contains(got2, "『九』") || strings.HasSuffix(got2, "…") {
		t.Fatalf("尾部簇展示异常: %q", got2)
	}
}

func TestDiffDetail(t *testing.T) {
	t.Run("中插词", func(t *testing.T) {
		lines, emitted, total, ok := diffDetail(
			"处三年以下有期徒刑拘役并处罚金",
			"处三年以下有期徒刑或者拘役并处罚金", 0)
		if !ok || total != 1 || emitted != 1 {
			t.Fatalf("ok=%v emitted=%d total=%d", ok, emitted, total)
		}
		join := strings.Join(lines, "\n")
		if !strings.Contains(join, "『或者』") {
			t.Fatalf("应标出新增「或者」: %q", join)
		}
	})
	t.Run("删除词只出 A 行", func(t *testing.T) {
		lines, _, total, ok := diffDetail(
			"处三年以下有期徒刑拘役并处罚金",
			"处三年以下有期徒刑并处罚金", 0)
		if !ok || total != 1 {
			t.Fatalf("ok=%v total=%d", ok, total)
		}
		join := strings.Join(lines, "\n")
		if !strings.Contains(join, "『拘役』") || !strings.HasPrefix(lines[0], "A: ") {
			t.Fatalf("应标出删除「拘役」且只出 A 行: %q", join)
		}
	})
	t.Run("整段替换为一簇", func(t *testing.T) {
		lines, emitted, total, ok := diffDetail("旧版全文", "新版全文", 0)
		if !ok || emitted != 1 || total != 1 || len(lines) < 2 {
			t.Fatalf("ok=%v emitted=%d total=%d lines=%v", ok, emitted, total, lines)
		}
	})
	t.Run("超长降级", func(t *testing.T) {
		_, _, _, ok := diffDetail(strings.Repeat("甲", diffDetailMaxRunes+1), "乙", 0)
		if ok {
			t.Fatal("超长块应降级（ok=false）")
		}
	})
}

// ---------- 工具层测试 ----------

func diffWrite(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return p
}

func diffCall(t *testing.T, root string, args map[string]any) tool.Result {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	res, err := FileDiff(&FileConfig{Root: root}).Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	return res
}

func diffRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	return root
}

func TestFileDiff_Identical(t *testing.T) {
	root := diffRoot(t)
	diffWrite(t, root, "a.txt", "第一段\n第二段\n")
	diffWrite(t, root, "b.txt", "第一段\n第二段")
	res := diffCall(t, root, map[string]any{"file_a": "a.txt", "file_b": "b.txt"})
	if res.IsError || !strings.Contains(res.Data, "无差异") {
		t.Fatalf("identical: IsError=%v Data=%q", res.IsError, res.Data)
	}
}

func TestFileDiff_CharChange(t *testing.T) {
	root := diffRoot(t)
	diffWrite(t, root, "a.txt", "处三年以下有期徒刑拘役并处罚金")
	diffWrite(t, root, "b.txt", "处三年以下有期徒刑或者拘役并处罚金")
	res := diffCall(t, root, map[string]any{"file_a": "a.txt", "file_b": "b.txt"})
	if res.IsError {
		t.Fatalf("unexpected error: %q", res.Data)
	}
	if !strings.Contains(res.Data, "[改]") || !strings.Contains(res.Data, "『或者』") {
		t.Fatalf("应报改并标出新增字: %q", res.Data)
	}
}

func TestFileDiff_AddDeleteBlock(t *testing.T) {
	root := diffRoot(t)
	diffWrite(t, root, "a.txt", "头部段落\n\n被删除的段落内容\n\n尾部保留段落")
	diffWrite(t, root, "b.txt", "头部段落\n\n尾部保留段落")
	res := diffCall(t, root, map[string]any{"file_a": "a.txt", "file_b": "b.txt"})
	if res.IsError || !strings.Contains(res.Data, "[删]") || !strings.Contains(res.Data, "被删除的段落内容") {
		t.Fatalf("整段删除应报 [删] 并附原文: %q", res.Data)
	}
	res2 := diffCall(t, root, map[string]any{"file_a": "b.txt", "file_b": "a.txt"})
	if res2.IsError || !strings.Contains(res2.Data, "[增]") {
		t.Fatalf("反向应为 [增]: %q", res2.Data)
	}
}

func TestFileDiff_IgnoreFold(t *testing.T) {
	root := diffRoot(t)
	// 原版（OCR 逐字空格 + 标点）vs 整理版：忽略空白与标点后实质一致。
	diffWrite(t, root, "raw.txt", "第 一 百 四 十 一 条 生 产 、 销 售 假 药 的")
	diffWrite(t, root, "clean.txt", "第一百四十一条 生产、销售假药的")
	// 全敏感：应报实质差异。
	res := diffCall(t, root, map[string]any{"file_a": "raw.txt", "file_b": "clean.txt"})
	if res.IsError || !strings.Contains(res.Data, "实质差异") {
		t.Fatalf("全敏感应报实质差异: %q", res.Data)
	}
	// 忽略空白 + 标点：折叠为格式差异，无实质。
	res2 := diffCall(t, root, map[string]any{"file_a": "raw.txt", "file_b": "clean.txt", "ignore": []string{"space", "punct"}})
	if res2.IsError {
		t.Fatalf("unexpected error: %q", res2.Data)
	}
	if !strings.Contains(res2.Data, "无实质内容差异") || !strings.Contains(res2.Data, "[格式]") {
		t.Fatalf("忽略后应折叠为格式差异: %q", res2.Data)
	}
	if strings.Contains(res2.Data, "[改]") || strings.Contains(res2.Data, "[删]") || strings.Contains(res2.Data, "[增]") {
		t.Fatalf("忽略后不应有实质明细: %q", res2.Data)
	}
}

func TestFileDiff_ReflowFold(t *testing.T) {
	root := diffRoot(t)
	// 同一段文字，一侧整段、一侧被空行切分：忽略空白后判排版差异（内容未变）。
	diffWrite(t, root, "a.txt", "第一段内容\n第二段内容")
	diffWrite(t, root, "b.txt", "第一段内容\n\n第二段内容")
	res := diffCall(t, root, map[string]any{"file_a": "a.txt", "file_b": "b.txt", "ignore": []string{"space"}})
	if res.IsError {
		t.Fatalf("unexpected error: %q", res.Data)
	}
	if !strings.Contains(res.Data, "无实质内容差异") || !strings.Contains(res.Data, "[排版]") {
		t.Fatalf("空行切分应折叠为排版差异: %q", res.Data)
	}
}

func TestFileDiff_Limit(t *testing.T) {
	root := diffRoot(t)
	diffWrite(t, root, "a.txt", "甲段落原始内容\n\n乙段落原始内容\n\n丙段落原始内容")
	diffWrite(t, root, "b.txt", "甲段落改动内容\n\n乙段落改动内容\n\n丙段落改动内容")
	// limit=1：只展开 1 条，其余计数。
	res := diffCall(t, root, map[string]any{"file_a": "a.txt", "file_b": "b.txt", "limit": 1})
	if res.IsError || !strings.Contains(res.Data, "另有 2 处实质差异未展开") {
		t.Fatalf("limit 截断提示缺失: %q", res.Data)
	}
	if n := strings.Count(res.Data, "[改]"); n != 1 {
		t.Fatalf("limit=1 应只展开 1 条 [改]，got %d: %q", n, res.Data)
	}
	// limit=0：不限，3 条全展开。
	res2 := diffCall(t, root, map[string]any{"file_a": "a.txt", "file_b": "b.txt", "limit": 0})
	if n := strings.Count(res2.Data, "[改]"); n != 3 {
		t.Fatalf("limit=0 应展开全部 3 条，got %d", n)
	}
}

func TestFileDiff_ParamsErrors(t *testing.T) {
	root := diffRoot(t)
	diffWrite(t, root, "a.txt", "内容")
	diffWrite(t, root, "b.txt", "内容2")
	// 缺参。
	if res := diffCall(t, root, map[string]any{"file_a": "a.txt"}); !res.IsError {
		t.Fatalf("缺 file_b 应报错: %q", res.Data)
	}
	// 非法 ignore。
	if res := diffCall(t, root, map[string]any{"file_a": "a.txt", "file_b": "b.txt", "ignore": []string{"case"}}); !res.IsError {
		t.Fatalf("非法 ignore 应报错: %q", res.Data)
	}
	// 文件不存在。
	if res := diffCall(t, root, map[string]any{"file_a": "nope.txt", "file_b": "b.txt"}); !res.IsError {
		t.Fatalf("不存在应报错: %q", res.Data)
	}
	// 目录当文件。
	if res := diffCall(t, root, map[string]any{"file_a": "sub", "file_b": "b.txt"}); !res.IsError || !strings.Contains(res.Data, "目录") {
		t.Fatalf("目录应报错: %q", res.Data)
	}
	// .env 拒绝（fail-closed）。
	diffWrite(t, root, ".env", "SECRET=1")
	if res := diffCall(t, root, map[string]any{"file_a": ".env", "file_b": "b.txt"}); !res.IsError || !strings.Contains(res.Data, "敏感") {
		t.Fatalf(".env 应拒绝: %q", res.Data)
	}
	// 二进制拒绝。
	diffWrite(t, root, "bin.txt", "ab\x00cd")
	if res := diffCall(t, root, map[string]any{"file_a": "bin.txt", "file_b": "b.txt"}); !res.IsError || !strings.Contains(res.Data, "二进制") {
		t.Fatalf("二进制应拒绝: %q", res.Data)
	}
}
