package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestLoadBootstrapMemory 覆盖启动注入读取的四个分支：文件缺失、纯空白、正常内容、超限截断。
func TestLoadBootstrapMemory(t *testing.T) {
	dir := t.TempDir()
	write := func(content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "MEMORY.md"), []byte(content), 0o644); err != nil {
			t.Fatalf("write MEMORY.md: %v", err)
		}
	}

	// 文件缺失：无常驻记忆，不阻塞启动。
	if got := loadBootstrapMemory(dir); got != "" {
		t.Errorf("missing file: got %q, want empty", got)
	}
	// 纯空白：视为无内容。
	write("  \n\t\n")
	if got := loadBootstrapMemory(dir); got != "" {
		t.Errorf("blank file: got %q, want empty", got)
	}
	// 正常内容：原样返回（TrimSpace 后）。
	write("## 偏好\n中文交流")
	if got := loadBootstrapMemory(dir); got != "## 偏好\n中文交流" {
		t.Errorf("normal: got %q", got)
	}
	// 超限：按字符截断并注明（只截注入副本，文件本体不受影响），且不得切坏 UTF-8。
	write(strings.Repeat("长", bootstrapLimit+10))
	got := loadBootstrapMemory(dir)
	note := "\n（已截断，完整内容可用 memory_search 检索）"
	if utf8.RuneCountInString(got) > bootstrapLimit+utf8.RuneCountInString(note) {
		t.Errorf("truncated rune count = %d, exceeds limit", utf8.RuneCountInString(got))
	}
	if !utf8.ValidString(got) {
		t.Error("truncated result is not valid UTF-8")
	}
	if !strings.Contains(got, "已截断") {
		t.Errorf("truncated note missing: %q", got)
	}
}
