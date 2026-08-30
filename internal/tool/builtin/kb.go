package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"small/internal/kb"
	"small/internal/tool"
)

// 知识库工具与 MemorySearch 同款构造风格：一工具一构造函数（tool.Tool），
// 有状态依赖（知识库索引）显式入参，由 RegisterBuiltins 装配（见 Zoo/model/kb.md §7）。
// 执行逻辑外置为具名函数（runKbXxx）：可脱离工具壳直接单测。
// 三个工具均只读（查询/巡检，无副作用），权限表登记 Pass。
// 失败语义：参数错误/节点不存在/知识库为空统一按业务失败（IsError）回灌，
// 由模型自行换参重试或直接作答（同 memory 工具边界）。

// KbTree 构造知识库结构树查询工具（kb_tree）：缩进树形展示结构父子关系，
// 支持从指定节点下钻与按域过滤（大方块自动聚合视图）。
func KbTree(store *kb.Store) tool.Tool {
	return tool.NewFunc(
		tool.Spec{
			Name:        "kb_tree",
			Description: "查询知识库结构树：从指定节点（Ref）或顶层开始，缩进展示父子层级（含认知深度 L1-L5 与域）。涉及知识库整体结构、某个知识点的归属与子知识点时调用；可传 domain 只看某个域（如 redis）的知识点。",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"ref": {"type": "string", "description": "起点节点 Ref，如 数据/一致性.md 或 数据/一致性.md#失效；缺省从顶层开始"},
					"domain": {"type": "string", "description": "按域过滤，只列出属于该域的知识点文件"}
				}
			}`),
		},
		func(_ context.Context, args json.RawMessage) (tool.Result, error) {
			return runKbTree(store, args)
		},
	)
}

// runKbTree 结构树查询的执行逻辑：解码参数 → 从起点/域过滤后 DFS 打印子树。
func runKbTree(store *kb.Store, args json.RawMessage) (tool.Result, error) {
	var in struct {
		Ref    string `json:"ref"`
		Domain string `json:"domain"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return tool.Result{Data: "参数错误: " + err.Error(), IsError: true}, nil
	}
	nodes := store.Nodes()
	if len(nodes) == 0 {
		return tool.Result{Data: "知识库为空（目录不存在或无 md 文件）", IsError: true}, nil
	}
	byRef := make(map[string]kb.Node, len(nodes))
	for _, n := range nodes {
		byRef[n.Ref] = n
	}
	var b strings.Builder
	if in.Ref != "" {
		if _, ok := byRef[in.Ref]; !ok {
			return tool.Result{Data: "节点不存在: " + in.Ref, IsError: true}, nil
		}
		printKbTree(&b, byRef, store, in.Ref, 0)
	} else {
		// 顶层 = 全部文件根节点（Heading 为空）；domain 过滤作用于文件根。
		for _, n := range nodes {
			if n.Heading != "" {
				continue
			}
			if in.Domain != "" && !hasString(n.Domains, in.Domain) {
				continue
			}
			printKbTree(&b, byRef, store, n.Ref, 0)
		}
	}
	return tool.Result{Data: b.String()}, nil
}

// printKbTree 递归打印节点及其结构子树（children 已是字典序，输出确定）。
func printKbTree(b *strings.Builder, byRef map[string]kb.Node, store *kb.Store, ref string, depth int) {
	n := byRef[ref]
	fmt.Fprintf(b, "%s%s (L%d)%s\n", strings.Repeat("  ", depth), n.Ref, n.Level, domainsSuffix(n.Domains))
	for _, c := range store.Children(ref) {
		printKbTree(b, byRef, store, c, depth+1)
	}
}

// KbRefs 构造反链报告工具（kb_refs）：删除检查的第一性入口——先看谁引用它、
// 有哪些子节点，再与用户协作决定删除方案（保留/改指/级联/断链，见 kb.md §6）。
func KbRefs(store *kb.Store) tool.Tool {
	return tool.NewFunc(
		tool.Spec{
			Name:        "kb_refs",
			Description: "报告知识库中某节点（Ref）的引用关系：被谁引用（反链）、结构子节点、父节点。删除/修改一个知识点前必须调用，确认删除的影响面后再与用户协作处理。",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"ref": {"type": "string", "description": "要检查的节点 Ref，如 数据/一致性.md#延迟双删"}
				},
				"required": ["ref"]
			}`),
		},
		func(_ context.Context, args json.RawMessage) (tool.Result, error) {
			return runKbRefs(store, args)
		},
	)
}

// runKbRefs 反链报告的执行逻辑：节点信息 + 反链 + 结构子节点。
func runKbRefs(store *kb.Store, args json.RawMessage) (tool.Result, error) {
	var in struct {
		Ref string `json:"ref"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return tool.Result{Data: "参数错误: " + err.Error(), IsError: true}, nil
	}
	if in.Ref == "" {
		return tool.Result{Data: "参数错误: ref 必填", IsError: true}, nil
	}
	n, ok := store.Node(in.Ref)
	if !ok {
		return tool.Result{Data: "节点不存在: " + in.Ref, IsError: true}, nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "节点: %s (L%d)%s\n", n.Ref, n.Level, domainsSuffix(n.Domains))
	fmt.Fprintf(&b, "父: %s\n", orDash(n.Parent))
	fmt.Fprintf(&b, "被引用（%d 处）:\n", len(store.Backlinks(in.Ref)))
	for _, s := range store.Backlinks(in.Ref) {
		fmt.Fprintf(&b, "  <- %s\n", s)
	}
	fmt.Fprintf(&b, "子节点（%d 个）:\n", len(store.Children(in.Ref)))
	for _, c := range store.Children(in.Ref) {
		fmt.Fprintf(&b, "  -> %s\n", c)
	}
	return tool.Result{Data: b.String()}, nil
}

// KbCheck 构造知识库巡检工具（kb_check）：无环、断链、重复标题、level 一致性。
// 定期整理仪式（kb.md §4 第 5 条）与用户手工改文件后的校验兜底。
func KbCheck(store *kb.Store) tool.Tool {
	return tool.NewFunc(
		tool.Spec{
			Name:        "kb_check",
			Description: "全库巡检知识库健康度：结构环、断链引用、重复标题、认知深度（level）与段落不一致。整理知识库、批量修改文件后调用，发现问题逐项修复。",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {}
			}`),
		},
		func(_ context.Context, args json.RawMessage) (tool.Result, error) {
			return runKbCheck(store, args)
		},
	)
}

// runKbCheck 巡检的执行逻辑：拉取 Check 结果并格式化。
func runKbCheck(store *kb.Store, args json.RawMessage) (tool.Result, error) {
	issues := store.Check()
	if len(issues) == 0 {
		return tool.Result{Data: "巡检通过：无结构环、无断链、无重复标题、level 一致。"}, nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "发现 %d 个问题，需处理:\n", len(issues))
	for _, is := range issues {
		fmt.Fprintf(&b, "[%s] %s: %s\n", is.Type, is.Ref, is.Detail)
	}
	return tool.Result{Data: b.String()}, nil
}

// KbWrite 构造知识库写入工具（kb_write）：把一段知识点落盘为带 frontmatter 的
// md 文件（domains/level 自动生成，level 由段落词表派生），供 kb_tree/kb_refs 立即索引。
// 触发约束：用户明确要求把知识点记入知识库时调用；写入前先向用户确认 domains
// （新域需用户批准，审批式词表增长）。写工具 → 权限表 Ask，执行前由 agent 统一确认。
func KbWrite(store *kb.Store) tool.Tool {
	return tool.NewFunc(
		tool.Spec{
			Name:        "kb_write",
			Description: "把一段知识点写入知识库（~/.small/kb 下的 md 文件）：自动生成 frontmatter（domains/level）并立即可被 kb_tree/kb_refs 检索。用户要求把知识点记入知识库时调用；写入前先向用户确认该知识点属于哪些域（新域需用户批准）。",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"topic": {"type": "string", "description": "知识点标题（作为文件名与 # 标题）"},
					"content": {"type": "string", "description": "知识点正文（建议分 ## 段落，含 定义/原理/失效 等深度段落便于自动定级）"},
					"domains": {"type": "array", "items": {"type": "string"}, "description": "所属域列表（须为已与用户确认的受控词表）"},
					"file": {"type": "string", "description": "目标相对路径（可含子目录，如 Java/延迟双删；缺省用 topic 作文件名）"},
					"level": {"type": "integer", "description": "认知深度 1-5 覆盖值（缺省由段落词表自动派生）"}
				},
				"required": ["topic", "content"]
			}`),
		},
		func(_ context.Context, args json.RawMessage) (tool.Result, error) {
			return runKbWrite(store, args)
		},
	)
}

// runKbWrite 知识库写入的执行逻辑：解码参数 → 受控写入（路径校验/防覆盖在 Store 层）。
func runKbWrite(store *kb.Store, args json.RawMessage) (tool.Result, error) {
	var in struct {
		Topic   string   `json:"topic"`
		Content string   `json:"content"`
		Domains []string `json:"domains"`
		File    string   `json:"file"`
		Level   int      `json:"level"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return tool.Result{Data: "参数错误: " + err.Error(), IsError: true}, nil
	}
	file := in.File
	if file == "" {
		file = in.Topic
	}
	ref, err := store.Write(file, in.Topic, in.Content, in.Domains, in.Level)
	if err != nil {
		return tool.Result{Data: "写入失败: " + err.Error(), IsError: true}, nil
	}
	return tool.Result{Data: "已写入知识库: " + ref}, nil
}

// hasString 判断列表是否含目标串。
func hasString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// domainsSuffix 把域列表格式化为行尾标注（无域返回空串）。
func domainsSuffix(ds []string) string {
	if len(ds) == 0 {
		return ""
	}
	return " [" + strings.Join(ds, ", ") + "]"
}

// orDash 空串显示为 "-"（父/值占位）。
func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
