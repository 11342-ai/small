package builtin

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newFileWorkspace 建一个含若干文件/目录的临时工作区。
func newFileWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	mustWrite := func(rel, content string) {
		t.Helper()
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	mustWrite("README.md", "# 项目\n\n部署决策：用 JSONL 存会话。")
	mustWrite("internal/agent/agent.go", "package agent\n\n// Run 推进一轮对话。\nfunc Run() {}\n")
	mustWrite("internal/tool/tool.go", "package tool\n\n// Spec 工具声明。\n")
	mustWrite(".git/config", "[core]")
	mustWrite("internal/agent/big.go", strings.Repeat("x", 600<<10)) // 超 512KB，doc_search 应跳过
	return root
}

func TestResolveInRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "ws")
	_ = os.MkdirAll(root, 0o755)

	if abs, err := resolveInRoot(root, "a/b.go"); err != nil {
		t.Fatalf("相对路径应放行，got err=%v", err)
	} else if abs != filepath.Join(root, "a/b.go") {
		t.Fatalf("abs = %s", abs)
	}
	if _, err := resolveInRoot(root, "../evil.txt"); err == nil || !strings.Contains(err.Error(), "越界") {
		t.Fatalf(".. 逃逸应拒绝，got %v", err)
	}
	if _, err := resolveInRoot(root, filepath.Join(t.TempDir(), "outside.txt")); err == nil {
		t.Fatal("Root 外绝对路径应拒绝")
	}
	if _, err := resolveInRoot(root, "a/../../evil.txt"); err == nil {
		t.Fatal("含 .. 的相对路径应拒绝")
	}
}

// TestResolveReadPath 只读放宽语义（tool-fs.md §5，2026-09-01）：
// 工作区内放行；工作区外非敏感路径放行（用户文档目录）；敏感系统目录拒绝。
func TestResolveReadPath(t *testing.T) {
	root := filepath.Join(t.TempDir(), "ws")
	_ = os.MkdirAll(root, 0o755)

	// 工作区内（相对/绝对）放行。
	if _, err := resolveReadPath(root, "a/b.go"); err != nil {
		t.Errorf("工作区内相对路径应放行: %v", err)
	}
	if _, err := resolveReadPath(root, filepath.Join(root, "x.md")); err != nil {
		t.Errorf("工作区内绝对路径应放行: %v", err)
	}
	// 工作区外非敏感路径放行（用户文档目录场景，如 ~/Pdf）。
	outside := filepath.Join(t.TempDir(), "Pdf")
	_ = os.MkdirAll(outside, 0o755)
	if abs, err := resolveReadPath(root, outside); err != nil || abs != outside {
		t.Errorf("工作区外非敏感路径应放行: %v %v", abs, err)
	}
	// 相对 .. 逃逸到非敏感路径也放行（同一黑名单语义）。
	if _, err := resolveReadPath(root, "../x.txt"); err != nil {
		t.Errorf("逃逸到非敏感路径应放行: %v", err)
	}
	// 敏感系统目录拒绝（绝对与相对逃逸都拦）。
	for _, p := range []string{"/etc/passwd", "/proc/self/status", "/usr/bin/env", "/var/log/syslog", "/root/.bashrc", "/boot/grub.cfg"} {
		if _, err := resolveReadPath(root, p); err == nil {
			t.Errorf("敏感路径 %q 应拒绝", p)
		}
	}
	// 相对逃逸到敏感路径应拒绝：动态上溯到根再进入 /etc（不依赖 TempDir 深度）。
	up := ""
	for d := root; filepath.Dir(d) != d; d = filepath.Dir(d) {
		up = filepath.Join(up, "..")
	}
	if _, err := resolveReadPath(root, filepath.Join(up, "etc", "passwd")); err == nil {
		t.Error("逃逸到敏感路径应拒绝")
	}
}

// TestIsSensitivePath 黑名单边界匹配：/etc 命中 /etc/passwd，不误伤 /etcetera、/home。
func TestIsSensitivePath(t *testing.T) {
	for _, p := range []string{"/etc", "/etc/passwd", "/proc/1", "/sys", "/usr/local", "/bin/sh", "/sbin/init", "/boot", "/dev/null", "/root", "/var/log"} {
		if !isSensitivePath(p) {
			t.Errorf("%q 应命中敏感黑名单", p)
		}
	}
	for _, p := range []string{"/etcetera", "/home/cxr", "/home/cxr/Pdf", "/tmp", "/usr-local"} {
		if isSensitivePath(p) {
			t.Errorf("%q 不应命中敏感黑名单", p)
		}
	}
}

// TestRunFileRead_OutsideWorkspace file_read 读工作区外非敏感文件成功、敏感路径拒绝。
func TestRunFileRead_OutsideWorkspace(t *testing.T) {
	root := newFileWorkspace(t)
	outside := t.TempDir()
	outFile := filepath.Join(outside, "note.md")
	if err := os.WriteFile(outFile, []byte("工作区外内容\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg := &FileConfig{Root: root}

	res, err := runFileRead(cfg, json.RawMessage(`{"path":"`+outFile+`"}`))
	if err != nil || res.IsError || !strings.Contains(res.Data, "工作区外内容") {
		t.Errorf("读工作区外非敏感文件应成功: %v %q", err, res.Data)
	}
	res, err = runFileRead(cfg, json.RawMessage(`{"path":"/etc/passwd"}`))
	if err != nil || !res.IsError || !strings.Contains(res.Data, "敏感") {
		t.Errorf("读敏感路径应拒绝: %v %q", err, res.Data)
	}
	// 写工具仍严格：file_write 工作区外路径拒绝（回归，tool-fs.md §5 路径约束写）。
	if res, err := runFileWrite(cfg, json.RawMessage(`{"path":"`+outFile+`","content":"x"}`)); err != nil || !res.IsError || !strings.Contains(res.Data, "越界") {
		t.Errorf("写工作区外路径应拒绝: %v %q", err, res.Data)
	}
}

func TestRunFileRead(t *testing.T) {
	root := newFileWorkspace(t)
	cfg := &FileConfig{Root: root}

	res, err := runFileRead(cfg, json.RawMessage(`{"path":"README.md"}`))
	if err != nil || res.IsError {
		t.Fatalf("读取失败：res=%+v err=%v", res, err)
	}
	if !strings.Contains(res.Data, "部署决策") || !strings.Contains(res.Data, "1│") {
		t.Fatalf("应含内容与行号，got %q", res.Data)
	}

	res, err = runFileRead(cfg, json.RawMessage(`{"path":"internal/agent/agent.go"}`))
	if err != nil || res.IsError || !strings.Contains(res.Data, "Run") {
		t.Fatalf("相对子路径读取失败：res=%+v err=%v", res, err)
	}

	// 敏感系统目录拒绝（放宽后仍 fail-closed，tool-fs.md §5）。
	res, err = runFileRead(cfg, json.RawMessage(`{"path":"/etc/passwd"}`))
	if err != nil || !res.IsError || !strings.Contains(res.Data, "敏感") {
		t.Fatalf("敏感路径应拒绝，got res=%+v err=%v", res, err)
	}

	res, err = runFileRead(cfg, json.RawMessage(`{"path":"no-such.md"}`))
	if err != nil || !res.IsError {
		t.Fatalf("不存在文件应失败，got res=%+v err=%v", res, err)
	}
}

func TestRunFileList(t *testing.T) {
	root := newFileWorkspace(t)
	cfg := &FileConfig{Root: root}

	// 目录列举：根目录应含 README.md 与 internal/（隐藏 .git 不列出？ReadDir 会列出 .git——验证含内）。
	res, err := runFileList(cfg, json.RawMessage(`{"path":"."}`))
	if err != nil || res.IsError {
		t.Fatalf("列目录失败：res=%+v err=%v", res, err)
	}
	if !strings.Contains(res.Data, "README.md") || !strings.Contains(res.Data, "internal/") {
		t.Fatalf("应含文件与目录，got %q", res.Data)
	}

	// glob：**/*.go 应命中 agent.go 与 tool.go（** 跨目录），且不含 README.md。
	res, err = runFileList(cfg, json.RawMessage(`{"path":"**/*.go"}`))
	if err != nil || res.IsError {
		t.Fatalf("glob 失败：res=%+v err=%v", res, err)
	}
	if !strings.Contains(res.Data, "internal/agent/agent.go") || !strings.Contains(res.Data, "internal/tool/tool.go") {
		t.Fatalf("glob 应命中 .go 文件，got %q", res.Data)
	}
	if strings.Contains(res.Data, "README.md") {
		t.Fatalf("glob *.go 不应含 README.md，got %q", res.Data)
	}

	res, err = runFileList(cfg, json.RawMessage(`{"path":"**/*.nope"}`))
	if err != nil || res.IsError || !strings.Contains(res.Data, "未找到") {
		t.Fatalf("无匹配应提示，got res=%+v err=%v", res, err)
	}
}

func TestRunDocSearch(t *testing.T) {
	root := newFileWorkspace(t)
	cfg := &FileConfig{Root: root}

	res, err := runDocSearch(cfg, json.RawMessage(`{"query":"JSONL"}`))
	if err != nil || res.IsError {
		t.Fatalf("搜索失败：res=%+v err=%v", res, err)
	}
	if !strings.Contains(res.Data, "README.md:3") {
		t.Fatalf("应命中 README.md 第 3 行，got %q", res.Data)
	}
	// 大小写不敏感 + 跨文件。
	res, err = runDocSearch(cfg, json.RawMessage(`{"query":"spec"}`))
	if err != nil || res.IsError || !strings.Contains(res.Data, "tool.go") {
		t.Fatalf("应命中 tool.go 的 Spec，got %q", res.Data)
	}

	// 无匹配：业务失败。
	res, err = runDocSearch(cfg, json.RawMessage(`{"query":"不存在的词xyz"}`))
	if err != nil || !res.IsError {
		t.Fatalf("无匹配应失败，got res=%+v err=%v", res, err)
	}

	// limit 生效。
	res, err = runDocSearch(cfg, json.RawMessage(`{"query":"package","limit":1}`))
	if err != nil || res.IsError {
		t.Fatalf("limit 搜索失败：res=%+v err=%v", res, err)
	}
	if strings.Count(res.Data, ":") < 1 {
		t.Fatalf("limit=1 应只返回 1 条，got %q", res.Data)
	}
}

func TestRunDocSearch_SkipsBigAndHidden(t *testing.T) {
	root := newFileWorkspace(t)
	cfg := &FileConfig{Root: root}
	// big.go 内容为 x 串（无 package 关键词），即使命中也不该因为大文件拖慢；.git 被跳过。
	res, err := runDocSearch(cfg, json.RawMessage(`{"query":"core"}`))
	if err != nil || !res.IsError {
		t.Fatalf(".git 应被跳过（无命中），got res=%+v err=%v", res, err)
	}
}

// writeLines 建一个 line1..lineN 的测试文件。
func writeLines(t *testing.T, root, rel string, n int) string {
	t.Helper()
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "line%d\n", i)
	}
	p := filepath.Join(root, rel)
	if err := os.WriteFile(p, []byte(b.String()), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
	return p
}

func TestRunFileRead_Window(t *testing.T) {
	root := newFileWorkspace(t)
	writeLines(t, root, "multi.txt", 10)
	cfg := &FileConfig{Root: root}

	// offset=3 limit=3 → 行 3..5，附续读建议 offset=6。
	res, err := runFileRead(cfg, json.RawMessage(`{"path":"multi.txt","offset":3,"limit":3}`))
	if err != nil || res.IsError {
		t.Fatalf("窗口读失败：res=%+v err=%v", res, err)
	}
	for _, want := range []string{"3│ line3", "4│ line4", "5│ line5"} {
		if !strings.Contains(res.Data, want) {
			t.Fatalf("应含 %q，got %q", want, res.Data)
		}
	}
	if strings.Contains(res.Data, "1│ line1") || strings.Contains(res.Data, "6│ line6") {
		t.Fatalf("窗口外行不应出现，got %q", res.Data)
	}
	if !strings.Contains(res.Data, "offset=6") {
		t.Fatalf("应给续读建议 offset=6，got %q", res.Data)
	}
}

func TestRunFileRead_WindowToEnd(t *testing.T) {
	root := newFileWorkspace(t)
	writeLines(t, root, "multi.txt", 10)
	cfg := &FileConfig{Root: root}

	// offset=9 limit=5：limit 超界截到文件尾，此时无续读建议。
	res, err := runFileRead(cfg, json.RawMessage(`{"path":"multi.txt","offset":9,"limit":5}`))
	if err != nil || res.IsError {
		t.Fatalf("窗口读失败：res=%+v err=%v", res, err)
	}
	if !strings.Contains(res.Data, "9│ line9") || !strings.Contains(res.Data, "10│ line10") {
		t.Fatalf("应含末尾两行，got %q", res.Data)
	}
	if strings.Contains(res.Data, "继续可") {
		t.Fatalf("读到文件尾不应给续读建议，got %q", res.Data)
	}
}

func TestRunFileRead_OffsetBeyond(t *testing.T) {
	root := newFileWorkspace(t)
	writeLines(t, root, "multi.txt", 3)
	cfg := &FileConfig{Root: root}

	res, err := runFileRead(cfg, json.RawMessage(`{"path":"multi.txt","offset":99}`))
	if err != nil || !res.IsError || !strings.Contains(res.Data, "offset=99 超出") {
		t.Fatalf("超界应提示，got res=%+v err=%v", res, err)
	}
}

func TestRunFileRead_SensitiveEnv(t *testing.T) {
	root := newFileWorkspace(t)
	cfg := &FileConfig{Root: root}
	for _, f := range []string{".env", ".env.example"} {
		if err := os.WriteFile(filepath.Join(root, f), []byte("SECRET=xx"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{".env", ".env.example"} {
		res, err := runFileRead(cfg, json.RawMessage(`{"path":"`+f+`"}`))
		if err != nil || !res.IsError || !strings.Contains(res.Data, "IGNORED") {
			t.Fatalf("%s 应拒绝读取，got res=%+v err=%v", f, res, err)
		}
	}
	// 普通文件不受影响（回归）。
	res, err := runFileRead(cfg, json.RawMessage(`{"path":"README.md"}`))
	if err != nil || res.IsError {
		t.Fatalf("普通文件应照常读，got res=%+v err=%v", res, err)
	}
}

func TestRunFileRead_TooLarge(t *testing.T) {
	root := newFileWorkspace(t)
	p := filepath.Join(root, "huge.txt")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(maxFileReadSize + 1); err != nil { // 稀疏文件，不占磁盘
		t.Fatal(err)
	}
	f.Close()
	cfg := &FileConfig{Root: root}

	res, err := runFileRead(cfg, json.RawMessage(`{"path":"huge.txt"}`))
	if err != nil || !res.IsError || !strings.Contains(res.Data, "过大") {
		t.Fatalf("超大文件应提示跳过，got res=%+v err=%v", res, err)
	}
}

func TestRunFileList_IgnoreDirsAndFiles(t *testing.T) {
	root := newFileWorkspace(t)
	must := func(rel, content string) {
		t.Helper()
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	must("node_modules/pkg/a.js", "x")
	must("dist/bundle.js", "x")
	must("vendor/example.com/x.go", "package x")
	must("src/app.js", "x")
	must("src/lib.min.js", "x")
	must("src/lib.js.map", "x")
	cfg := &FileConfig{Root: root}

	// glob **/*.go：vendor/ 下应被跳过（结果里不应有 vendor 路径）。
	res, err := runFileList(cfg, json.RawMessage(`{"path":"**/*.go"}`))
	if err != nil || res.IsError || strings.Contains(res.Data, "vendor") {
		t.Fatalf("vendor 应被跳过，got res=%+v err=%v", res, err)
	}
	// glob **/*.js：普通 js 命中，node_modules/dist/*.min.js/*.map 全部跳过。
	res, err = runFileList(cfg, json.RawMessage(`{"path":"**/*.js"}`))
	if err != nil || res.IsError {
		t.Fatalf("glob 失败：res=%+v err=%v", res, err)
	}
	if !strings.Contains(res.Data, "src/app.js") {
		t.Fatalf("普通 js 应命中，got %q", res.Data)
	}
	for _, bad := range []string{"node_modules", "dist/", ".min.js", ".map"} {
		if strings.Contains(res.Data, bad) {
			t.Fatalf("忽略项不应出现（%s），got %q", bad, res.Data)
		}
	}
}

func TestRunFileList_DirListingFiltersIgnored(t *testing.T) {
	root := newFileWorkspace(t)
	if err := os.MkdirAll(filepath.Join(root, "node_modules/pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "node_modules/pkg/x.js"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &FileConfig{Root: root}

	res, err := runFileList(cfg, json.RawMessage(`{"path":"."}`))
	if err != nil || res.IsError {
		t.Fatalf("列目录失败：res=%+v err=%v", res, err)
	}
	if strings.Contains(res.Data, "node_modules") || strings.Contains(res.Data, ".git") {
		t.Fatalf("列目录不应含忽略/隐藏目录，got %q", res.Data)
	}
}

func TestRunDocSearch_IgnoreDirs(t *testing.T) {
	root := newFileWorkspace(t)
	if err := os.MkdirAll(filepath.Join(root, "node_modules/pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "node_modules/pkg/a.js"), []byte("SECRET_TOKEN_XYZ"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &FileConfig{Root: root}

	res, err := runDocSearch(cfg, json.RawMessage(`{"query":"SECRET_TOKEN_XYZ"}`))
	if err != nil || !res.IsError {
		t.Fatalf("node_modules 内命中应被跳过（无结果），got res=%+v err=%v", res, err)
	}
}

func TestRunDocSearch_PathFile(t *testing.T) {
	root := newFileWorkspace(t)
	cfg := &FileConfig{Root: root}

	// path 限定单文件：命中只来自 README.md（JSONL 只出现在 README）。
	res, err := runDocSearch(cfg, json.RawMessage(`{"query":"JSONL","path":"README.md"}`))
	if err != nil || res.IsError {
		t.Fatalf("限定文件搜索失败：res=%+v err=%v", res, err)
	}
	if !strings.Contains(res.Data, "README.md:") {
		t.Fatalf("应命中 README.md，got %q", res.Data)
	}
	// 限定文件内没有的词 → 未找到（不扩散到其他文件）。
	res, err = runDocSearch(cfg, json.RawMessage(`{"query":"package","path":"README.md"}`))
	if err != nil || !res.IsError {
		t.Fatalf("限定文件内无匹配应失败，got res=%+v err=%v", res, err)
	}
}

func TestRunDocSearch_PathDir(t *testing.T) {
	root := newFileWorkspace(t)
	cfg := &FileConfig{Root: root}

	// path 限定子目录：只搜 internal 下，不搜根 README。
	res, err := runDocSearch(cfg, json.RawMessage(`{"query":"Spec","path":"internal"}`))
	if err != nil || res.IsError {
		t.Fatalf("限定目录搜索失败：res=%+v err=%v", res, err)
	}
	if !strings.Contains(res.Data, "tool/tool.go") {
		t.Fatalf("应命中 internal/tool/tool.go，got %q", res.Data)
	}
	if strings.Contains(res.Data, "README.md") {
		t.Fatalf("限定目录不应命中外部文件，got %q", res.Data)
	}
}

func TestRunDocSearch_PathMissing(t *testing.T) {
	root := newFileWorkspace(t)
	cfg := &FileConfig{Root: root}

	res, err := runDocSearch(cfg, json.RawMessage(`{"query":"x","path":"no-such-dir"}`))
	if err != nil || !res.IsError || !strings.Contains(res.Data, "路径不存在") {
		t.Fatalf("路径不存在应报错，got res=%+v err=%v", res, err)
	}
}

func TestRunDocSearch_PathExplicitIgnoresHidden(t *testing.T) {
	root := newFileWorkspace(t)
	cfg := &FileConfig{Root: root}

	// 显式点名 .git（隐藏目录）：照常搜索（对齐 file_read 语义），命中 .git/config。
	res, err := runDocSearch(cfg, json.RawMessage(`{"query":"core","path":".git"}`))
	if err != nil || res.IsError || !strings.Contains(res.Data, ".git/config") {
		t.Fatalf("显式点名隐藏目录应可搜，got res=%+v err=%v", res, err)
	}
}
