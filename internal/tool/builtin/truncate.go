package builtin

// outputLimit 工具单次输出上限（字符）：防长输出刷爆上下文（tool-extend.md 边界④）。
// 各工具共用（exec 输出、file_read 文件内容、web_fetch 网页正文等）。
const outputLimit = 8000

// truncateOutput 输出超限按 rune 截断（字节切分可能切坏 UTF-8）。
func truncateOutput(s string) string {
	runes := []rune(s)
	if len(runes) <= outputLimit {
		return s
	}
	return string(runes[:outputLimit]) + "\n…（已截断）"
}
