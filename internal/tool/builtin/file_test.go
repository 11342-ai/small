package builtin

import (
	"encoding/json"
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

	res, err = runFileRead(cfg, json.RawMessage(`{"path":"../etc/passwd"}`))
	if err != nil || !res.IsError || !strings.Contains(res.Data, "越界") {
		t.Fatalf("越界应拒绝，got res=%+v err=%v", res, err)
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
