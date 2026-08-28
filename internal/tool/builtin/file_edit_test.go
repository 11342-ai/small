package builtin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// editArgs 构造 file_edit 参数（replacements 数组 + allow_multiple 开关）。
func editArgs(path string, allowMultiple bool, reps ...editReplacement) json.RawMessage {
	in, _ := json.Marshal(map[string]any{
		"path":           path,
		"allow_multiple": allowMultiple,
		"replacements":   reps,
	})
	return in
}

// readWorkspace 读工作区文件内容（测试断言用）。
func readWorkspace(t *testing.T, root, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(data)
}

func TestRunFileEdit_SingleReplace(t *testing.T) {
	root := newFileWorkspace(t)
	cfg := &FileConfig{Root: root}

	res, err := runFileEdit(cfg, editArgs("README.md", false,
		editReplacement{Old: "JSONL", New: "JSON Lines"}))
	if err != nil || res.IsError {
		t.Fatalf("替换失败：res=%+v err=%v", res, err)
	}
	got := readWorkspace(t, root, "README.md")
	if !strings.Contains(got, "JSON Lines") || strings.Contains(got, "JSONL") {
		t.Fatalf("应替换 JSONL→JSON Lines，got %q", got)
	}
}

func TestRunFileEdit_NotUniqueRejectedThenAllowMultiple(t *testing.T) {
	root := newFileWorkspace(t)
	p := filepath.Join(root, "dup.txt")
	if err := os.WriteFile(p, []byte("package a\npackage b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &FileConfig{Root: root}

	// 默认唯一匹配：多命中拒绝，文件不变。
	res, err := runFileEdit(cfg, editArgs("dup.txt", false,
		editReplacement{Old: "package", New: "pkg"}))
	if err != nil || !res.IsError || !strings.Contains(res.Data, "出现 2 处") {
		t.Fatalf("多命中应拒绝，got res=%+v err=%v", res, err)
	}
	if got := readWorkspace(t, root, "dup.txt"); !strings.Contains(got, "package a") {
		t.Fatalf("拒绝后文件不应变，got %q", got)
	}

	// allow_multiple=true：全部替换。
	res, err = runFileEdit(cfg, editArgs("dup.txt", true,
		editReplacement{Old: "package", New: "pkg"}))
	if err != nil || res.IsError {
		t.Fatalf("allow_multiple 应成功，got res=%+v err=%v", res, err)
	}
	if got := readWorkspace(t, root, "dup.txt"); !strings.Contains(got, "pkg a\npkg b") {
		t.Fatalf("应全部替换，got %q", got)
	}
}

func TestRunFileEdit_AllFailedNoChange(t *testing.T) {
	root := newFileWorkspace(t)
	cfg := &FileConfig{Root: root}

	res, err := runFileEdit(cfg, editArgs("README.md", false,
		editReplacement{Old: "不存在的词", New: "x"}))
	if err != nil || !res.IsError || !strings.Contains(res.Data, "文件无变更") {
		t.Fatalf("全失败应报文件无变更，got res=%+v err=%v", res, err)
	}
	if got := readWorkspace(t, root, "README.md"); !strings.Contains(got, "JSONL") {
		t.Fatalf("全失败后文件不应变，got %q", got)
	}
}

func TestRunFileEdit_PartialSuccess(t *testing.T) {
	root := newFileWorkspace(t)
	cfg := &FileConfig{Root: root}

	// 第 1 组成功、第 2 组未找到：返回成功 + 跳过原因。
	res, err := runFileEdit(cfg, editArgs("README.md", false,
		editReplacement{Old: "JSONL", New: "JSON Lines"},
		editReplacement{Old: "没有的词", New: "x"}))
	if err != nil || res.IsError {
		t.Fatalf("部分成功不应判失败，got res=%+v err=%v", res, err)
	}
	if !strings.Contains(res.Data, "替换 1 组") || !strings.Contains(res.Data, "没有的词") {
		t.Fatalf("应含成功数与跳过原因，got %q", res.Data)
	}
	if got := readWorkspace(t, root, "README.md"); !strings.Contains(got, "JSON Lines") {
		t.Fatalf("成功组应落地，got %q", got)
	}
}

func TestRunFileEdit_CRLFPreserved(t *testing.T) {
	root := newFileWorkspace(t)
	p := filepath.Join(root, "crlf.txt")
	if err := os.WriteFile(p, []byte("line a\r\nline b\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &FileConfig{Root: root}

	// old_string 用 \n 写（模型视角），也能命中 CRLF 文件；落盘后仍保持 CRLF。
	res, err := runFileEdit(cfg, editArgs("crlf.txt", false,
		editReplacement{Old: "line a\nline b", New: "line A\nline B"}))
	if err != nil || res.IsError {
		t.Fatalf("CRLF 替换失败：res=%+v err=%v", res, err)
	}
	if got := readWorkspace(t, root, "crlf.txt"); got != "line A\r\nline B\r\n" {
		t.Fatalf("应保持 CRLF 且替换成功，got %q", got)
	}
}

func TestRunFileEdit_EscapeRejected(t *testing.T) {
	root := newFileWorkspace(t)
	cfg := &FileConfig{Root: root}

	res, err := runFileEdit(cfg, editArgs("../evil.txt", false,
		editReplacement{Old: "a", New: "b"}))
	if err != nil || !res.IsError || !strings.Contains(res.Data, "越界") {
		t.Fatalf("越界应拒绝，got res=%+v err=%v", res, err)
	}
}

func TestRunFileEdit_BadArgs(t *testing.T) {
	cfg := &FileConfig{Root: newFileWorkspace(t)}
	cases := []struct {
		name string
		args json.RawMessage
	}{
		{"非法 JSON", json.RawMessage(`{`)},
		{"空 path", json.RawMessage(`{"path":" ","replacements":[{"old_string":"a","new_string":"b"}]}`)},
		{"空 replacements", json.RawMessage(`{"path":"README.md"}`)},
	}
	for _, c := range cases {
		res, err := runFileEdit(cfg, c.args)
		if err != nil || !res.IsError {
			t.Fatalf("%s: 应业务失败，got res=%+v err=%v", c.name, res, err)
		}
	}
}

func TestWriteFileAtomic(t *testing.T) {
	root := t.TempDir()

	// 新建：内容完整、无残留临时文件、权限 0644。
	p := filepath.Join(root, "new.txt")
	if err := writeFileAtomic(p, []byte("hello")); err != nil {
		t.Fatalf("新建失败: %v", err)
	}
	if got := readWorkspace(t, root, "new.txt"); got != "hello" {
		t.Fatalf("内容不符，got %q", got)
	}
	leftover, _ := filepath.Glob(filepath.Join(root, ".small-write-*"))
	if len(leftover) != 0 {
		t.Fatalf("不应残留临时文件，got %v", leftover)
	}

	// 覆盖：保留原文件权限（0640 → 仍 0640）。
	if err := os.Chmod(p, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(p, []byte("world")); err != nil {
		t.Fatalf("覆盖失败: %v", err)
	}
	if got := readWorkspace(t, root, "new.txt"); got != "world" {
		t.Fatalf("覆盖内容不符，got %q", got)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o640 {
		t.Fatalf("覆盖应保留原权限，got %v", fi.Mode().Perm())
	}
}
