package builtin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeGitIgnore 写 .gitignore 并加载。
func writeGitIgnore(t *testing.T, root, content string) *gitIgnore {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return loadGitIgnore(root)
}

func TestLoadGitIgnore_Parse(t *testing.T) {
	gi := writeGitIgnore(t, t.TempDir(), "# 注释\n\n*.log\n/dist/\n!keep.log\nbuild\nbad\\escape\n")
	// 有效规则：*.log、/dist/、!keep.log、build（注释/空行/转义跳过）。
	if len(gi.patterns) != 4 {
		t.Fatalf("patterns = %d, want 4（%+v）", len(gi.patterns), gi.patterns)
	}
	// 取反与目录后缀标记。
	var negate, dirOnly bool
	for _, p := range gi.patterns {
		if p.glob == "keep.log" {
			negate = p.negate
		}
		if p.glob == "dist" {
			dirOnly = p.dirOnly
		}
	}
	if !negate {
		t.Error("!keep.log 应标记 negate")
	}
	if !dirOnly {
		t.Error("/dist/ 应标记 dirOnly")
	}
}

func TestGitIgnore_Ignored(t *testing.T) {
	gi := writeGitIgnore(t, t.TempDir(), "*.log\n/dist/\n!keep.log\nbuild\n")
	cases := []struct {
		rel   string
		isDir bool
		want  bool
	}{
		{"a.log", false, true},        // *.log basename
		{"sub/b.log", false, true},    // *.log 任意层级
		{"keep.log", false, false},    // ! 取反（最后匹配）
		{"dist", true, true},          // /dist/ 目录
		{"dist/x.js", false, true},    // /dist/ 下内容（目录规则前缀）
		{"build", true, true},         // build 无斜杠 → basename
		{"src/main.go", false, false}, // 无关
	}
	for _, c := range cases {
		if got := gi.Ignored(c.rel, c.isDir); got != c.want {
			t.Errorf("Ignored(%q, dir=%v) = %v, want %v", c.rel, c.isDir, got, c.want)
		}
	}
}

func TestRunFileList_GitIgnore(t *testing.T) {
	root := newFileWorkspace(t)
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("*.tmp\nignored_dir/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.tmp"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "ignored_dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "ignored_dir/x.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &FileConfig{Root: root}

	// glob **/*.go：ignored_dir 被 gitignore → 不出现（工作区自身 .go 文件照常出现）。
	res, err := runFileList(cfg, json.RawMessage(`{"path":"**/*.go"}`))
	if err != nil || res.IsError || strings.Contains(res.Data, "ignored_dir") {
		t.Fatalf("gitignore 目录应被跳过，got res=%+v err=%v", res, err)
	}
	// 列目录：*.tmp 与 ignored_dir 均不出现。
	res, err = runFileList(cfg, json.RawMessage(`{"path":"."}`))
	if err != nil || res.IsError {
		t.Fatalf("列目录失败：res=%+v err=%v", res, err)
	}
	if strings.Contains(res.Data, "a.tmp") || strings.Contains(res.Data, "ignored_dir") {
		t.Fatalf("gitignore 文件/目录应被过滤，got %q", res.Data)
	}
}

func TestRunDocSearch_GitIgnore(t *testing.T) {
	root := newFileWorkspace(t)
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("*.tmp\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "secret.tmp"), []byte("NEEDLE_XYZ"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &FileConfig{Root: root}

	res, err := runDocSearch(cfg, json.RawMessage(`{"query":"NEEDLE_XYZ"}`))
	if err != nil || !res.IsError {
		t.Fatalf("gitignore 文件命中应被跳过（无结果），got res=%+v err=%v", res, err)
	}
}
