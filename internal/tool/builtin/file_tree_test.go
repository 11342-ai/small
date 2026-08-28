package builtin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunFileTree_Structure(t *testing.T) {
	root := newFileWorkspace(t)
	cfg := &FileConfig{Root: root}

	res, err := runFileTree(cfg, json.RawMessage(`{}`))
	if err != nil || res.IsError {
		t.Fatalf("file_tree 失败：res=%+v err=%v", res, err)
	}
	// 目录 + 文件 + 头部摘要。
	if !strings.Contains(res.Data, "internal/") {
		t.Fatalf("应含目录 internal/，got %q", res.Data)
	}
	if !strings.Contains(res.Data, "README.md") || !strings.Contains(res.Data, "# 项目") {
		t.Fatalf("应含 README.md 及摘要行，got %q", res.Data)
	}
	if !strings.Contains(res.Data, "package agent") {
		t.Fatalf("应含 agent.go 摘要，got %q", res.Data)
	}
	// 隐藏目录（.git）跳过。
	if strings.Contains(res.Data, ".git") {
		t.Fatalf("隐藏目录不应出现，got %q", res.Data)
	}
	// 无超预算时不应出现省略提示。
	if strings.Contains(res.Data, "省略") {
		t.Fatalf("默认预算不应省略，got %q", res.Data)
	}
}

func TestRunFileTree_BudgetTruncate(t *testing.T) {
	root := newFileWorkspace(t)
	cfg := &FileConfig{Root: root}

	// 极小预算：触发省略提示。
	res, err := runFileTree(cfg, json.RawMessage(`{"budget":50}`))
	if err != nil || res.IsError {
		t.Fatalf("file_tree 失败：res=%+v err=%v", res, err)
	}
	if !strings.Contains(res.Data, "省略") && !strings.Contains(res.Data, "可调大 budget") {
		t.Fatalf("小预算应提示省略，got %q", res.Data)
	}
}

func TestRunFileTree_Subtree(t *testing.T) {
	root := newFileWorkspace(t)
	cfg := &FileConfig{Root: root}

	res, err := runFileTree(cfg, json.RawMessage(`{"path":"internal"}`))
	if err != nil || res.IsError {
		t.Fatalf("file_tree 失败：res=%+v err=%v", res, err)
	}
	// 子树模式：起点（internal）自身不输出，其下 agent/ 与 tool/ 出现。
	if !strings.Contains(res.Data, "agent/") || !strings.Contains(res.Data, "tool/") {
		t.Fatalf("子树应含 agent/ 与 tool/，got %q", res.Data)
	}
	if strings.Contains(res.Data, "README.md") {
		t.Fatalf("子树模式不应含子树外文件，got %q", res.Data)
	}
}

func TestRunFileTree_RespectsGitIgnore(t *testing.T) {
	root := newFileWorkspace(t)
	// .gitignore 忽略 *.tmp；file_tree 也应遵循。
	writeGitIgnore(t, root, "*.tmp\n")
	if err := os.WriteFile(filepath.Join(root, "gen.tmp"), []byte("generated"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &FileConfig{Root: root}

	res, err := runFileTree(cfg, json.RawMessage(`{}`))
	if err != nil || res.IsError {
		t.Fatalf("file_tree 失败：res=%+v err=%v", res, err)
	}
	if strings.Contains(res.Data, "gen.tmp") {
		t.Fatalf("gitignore 文件不应出现在树中，got %q", res.Data)
	}
}
