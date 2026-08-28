package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func execArgs(command string, args ...string) json.RawMessage {
	in, _ := json.Marshal(map[string]any{"command": command, "args": args})
	return in
}

func TestRunExec_BadArgs(t *testing.T) {
	if res, err := runExec(context.Background(), ExecConfig{Allow: []string{"echo"}}, json.RawMessage(`{`)); err != nil || !res.IsError {
		t.Fatalf("非法 JSON 应返回业务失败，got res=%+v err=%v", res, err)
	}
	if res, _ := runExec(context.Background(), ExecConfig{Allow: []string{"echo"}}, execArgs("")); !res.IsError || !strings.Contains(res.Data, "command 为空") {
		t.Fatalf("空命令应报错，got %q", res.Data)
	}
}

func TestRunExec_NotAllowed(t *testing.T) {
	// 白名单外命令：fail-closed（白名单优先于一切）。
	res, err := runExec(context.Background(), ExecConfig{Allow: []string{"echo"}}, execArgs("rm", "-rf", "/"))
	if err != nil || !res.IsError || !strings.Contains(res.Data, "不在白名单内") {
		t.Fatalf("白名单外命令应拒绝，got res=%+v err=%v", res, err)
	}
}

func TestRunExec_Success(t *testing.T) {
	res, err := runExec(context.Background(), ExecConfig{Allow: []string{"echo"}}, execArgs("echo", "hello"))
	if err != nil {
		t.Fatalf("runExec: %v", err)
	}
	if res.IsError {
		t.Fatalf("不应失败，got %q", res.Data)
	}
	if !strings.Contains(res.Data, "hello") {
		t.Fatalf("输出应包含 hello，got %q", res.Data)
	}
}

func TestRunExec_Timeout(t *testing.T) {
	res, err := runExec(context.Background(), ExecConfig{
		Allow:   []string{"sleep"},
		Timeout: 50 * time.Millisecond,
	}, execArgs("sleep", "5"))
	if err != nil {
		t.Fatalf("runExec: %v", err)
	}
	if !res.IsError || !strings.Contains(res.Data, "超时") {
		t.Fatalf("超时命令应报超时，got %q", res.Data)
	}
}

func TestTruncateOutput(t *testing.T) {
	short := "hi"
	if got := truncateOutput(short); got != short {
		t.Fatalf("短输出不应截断，got %q", got)
	}
	long := strings.Repeat("界", outputLimit+100) // 中文多字节，验证 rune 安全切分
	got := truncateOutput(long)
	if len([]rune(got)) != outputLimit+7 { // 截断标记 "\n…（已截断）" 为 7 rune
		t.Fatalf("截断后 rune 数 = %d, want %d", len([]rune(got)), outputLimit+7)
	}
	if !strings.HasSuffix(got, "（已截断）") {
		t.Fatalf("截断应带标记，got %q", got[len(got)-20:])
	}
}
