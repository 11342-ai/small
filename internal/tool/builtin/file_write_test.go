package builtin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeArgs 构造 file_write 参数。
func writeArgs(path, content string) json.RawMessage {
	in, _ := json.Marshal(map[string]any{"path": path, "content": content})
	return in
}

func TestRunFileWrite_Create(t *testing.T) {
	root := newFileWorkspace(t)
	cfg := &FileConfig{Root: root}

	res, err := runFileWrite(cfg, writeArgs("internal/agent/new.md", "hello\n"))
	if err != nil || res.IsError {
		t.Fatalf("写入失败：res=%+v err=%v", res, err)
	}
	if !strings.Contains(res.Data, "6 字节") { // "hello\n" = 6 字节
		t.Fatalf("应含字节数，got %q", res.Data)
	}
	if got := readWorkspace(t, root, "internal/agent/new.md"); got != "hello\n" {
		t.Fatalf("内容不符，got %q", got)
	}
}

func TestRunFileWrite_Overwrite(t *testing.T) {
	root := newFileWorkspace(t)
	cfg := &FileConfig{Root: root}

	if _, err := runFileWrite(cfg, writeArgs("README.md", "新内容")); err != nil {
		t.Fatalf("覆盖失败: %v", err)
	}
	if got := readWorkspace(t, root, "README.md"); got != "新内容" {
		t.Fatalf("覆盖后内容不符，got %q", got)
	}
}

func TestRunFileWrite_EmptyContent(t *testing.T) {
	root := newFileWorkspace(t)
	cfg := &FileConfig{Root: root}

	// 空串 content = 创建空文件（合法，用于清空/占位）。
	if _, err := runFileWrite(cfg, writeArgs("empty.txt", "")); err != nil {
		t.Fatalf("写空文件失败: %v", err)
	}
	if got := readWorkspace(t, root, "empty.txt"); got != "" {
		t.Fatalf("应为空文件，got %q", got)
	}
}

func TestRunFileWrite_MissingParentDir(t *testing.T) {
	root := newFileWorkspace(t)
	cfg := &FileConfig{Root: root}

	res, err := runFileWrite(cfg, writeArgs("no/such/dir/f.txt", "x"))
	if err != nil || !res.IsError || !strings.Contains(res.Data, "父目录不存在") {
		t.Fatalf("父目录缺失应拒绝，got res=%+v err=%v", res, err)
	}
}

func TestRunFileWrite_EscapeRejected(t *testing.T) {
	root := newFileWorkspace(t)
	cfg := &FileConfig{Root: root}

	res, err := runFileWrite(cfg, writeArgs("../evil.txt", "x"))
	if err != nil || !res.IsError || !strings.Contains(res.Data, "越界") {
		t.Fatalf("越界应拒绝，got res=%+v err=%v", res, err)
	}
}

func TestRunFileWrite_BadArgs(t *testing.T) {
	cfg := &FileConfig{Root: newFileWorkspace(t)}
	cases := []struct {
		name string
		args json.RawMessage
	}{
		{"非法 JSON", json.RawMessage(`{`)},
		{"空 path", writeArgs("  ", "x")},
		{"缺 content", json.RawMessage(`{"path":"a.txt"}`)},
	}
	for _, c := range cases {
		res, err := runFileWrite(cfg, c.args)
		if err != nil || !res.IsError {
			t.Fatalf("%s: 应业务失败，got res=%+v err=%v", c.name, res, err)
		}
	}
}

// 原子写完整性（覆盖场景不留半截/临时文件）由 TestWriteFileAtomic 覆盖；
// 这里补一条：写入后目录无残留临时文件。
func TestRunFileWrite_NoTempLeftover(t *testing.T) {
	root := newFileWorkspace(t)
	cfg := &FileConfig{Root: root}
	if _, err := runFileWrite(cfg, writeArgs("a.txt", "content")); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	leftover, _ := filepath.Glob(filepath.Join(root, ".small-write-*"))
	if len(leftover) != 0 {
		t.Fatalf("不应残留临时文件，got %v", leftover)
	}
	_ = os.Remove(filepath.Join(root, "a.txt"))
}
