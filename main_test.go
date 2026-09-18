package main

import (
	"context"
	"os"
	"path/filepath"
	"small/internal/k8s"
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

func TestTruncate(t *testing.T) {
	cases := []struct {
		in   string
		n    int
		want string
	}{
		{"短文本", 10, "短文本"},             // 不超限：原样
		{"中文字符串测试截断", 4, "中文字符…（已截断）"}, // 超限：按 rune 截断 + 标注
		{"", 5, ""}, // 空串
	}
	for _, c := range cases {
		if got := truncate(c.in, c.n); got != c.want {
			t.Errorf("truncate(%q, %d) = %q, want %q", c.in, c.n, got, c.want)
		}
	}
}

// TestCmdDiag /diag 的前置校验：未接入集群与参数格式错都在命令层拦住，不注入消息。
// 注意用零值 Collector 只走参数分支——校验失败发生在触达采集器之前，不会打到 nil 客户端。
func TestCmdDiag(t *testing.T) {
	ctx := context.Background()

	// 采集器未就绪：把组合根给的"未接入原因"原样说出去（集群不可达时不该误导用户去查 kubeconfig）。
	reason := "连接 apiserver 失败（connect: connection refused）；确认集群可达（kubectl 能连上）后重启"
	_, err := cmdDiag(nil, reason).Run(ctx, []string{"default/web-0"})
	if err == nil || !strings.Contains(err.Error(), reason) {
		t.Errorf("coll 为 nil 时应报错并带上具体原因，实际: %v", err)
	}

	// 原因缺省（兜底路径）：报错也不能是空话——要给出可执行的下一步。
	_, err = cmdDiag(nil, "").Run(ctx, []string{"default/web-0"})
	if err == nil || !strings.Contains(err.Error(), "kube_config") {
		t.Errorf("原因缺省时应给出兜底提示（检查 kube_config），实际: %v", err)
	}

	// 参数缺失或格式错：全部应在命令层报错（不触达采集器）。
	coll := &k8s.Collector{}
	for _, args := range [][]string{nil, {""}, {"web-0"}, {"/web-0"}, {"default/"}} {
		if _, err := cmdDiag(coll, "").Run(ctx, args); err == nil {
			t.Errorf("args=%v 应报参数错误", args)
		}
	}
}
