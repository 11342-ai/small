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
	// 白名单外：即使 Confirm 放行也必须拒绝（fail-closed，白名单优先）。
	res, err := runExec(context.Background(), ExecConfig{
		Allow:   []string{"echo"},
		Confirm: func(string, []string) bool { return true },
	}, execArgs("rm", "-rf", "/"))
	if err != nil || !res.IsError || !strings.Contains(res.Data, "不在白名单内") {
		t.Fatalf("白名单外命令应拒绝，got res=%+v err=%v", res, err)
	}
}

func TestRunExec_ConfirmNilFailClosed(t *testing.T) {
	// Confirm 为 nil：即使白名单内也拒绝（fail-closed）。
	res, err := runExec(context.Background(), ExecConfig{Allow: []string{"echo"}}, execArgs("echo", "hi"))
	if err != nil || !res.IsError || !strings.Contains(res.Data, "未获确认") {
		t.Fatalf("Confirm 为 nil 应拒绝，got res=%+v err=%v", res, err)
	}
}

func TestRunExec_ConfirmDenied(t *testing.T) {
	res, err := runExec(context.Background(), ExecConfig{
		Allow:   []string{"echo"},
		Confirm: func(string, []string) bool { return false },
	}, execArgs("echo", "hi"))
	if err != nil || !res.IsError || !strings.Contains(res.Data, "未获确认") {
		t.Fatalf("用户拒绝应拒绝，got res=%+v err=%v", res, err)
	}
}

func TestRunExec_Success(t *testing.T) {
	gotCmd, gotArgs := "", []string(nil)
	res, err := runExec(context.Background(), ExecConfig{
		Allow: []string{"echo"},
		Confirm: func(cmd string, args []string) bool {
			gotCmd, gotArgs = cmd, args
			return true
		},
	}, execArgs("echo", "hello"))
	if err != nil {
		t.Fatalf("runExec: %v", err)
	}
	if res.IsError {
		t.Fatalf("不应失败，got %q", res.Data)
	}
	if gotCmd != "echo" || len(gotArgs) != 1 || gotArgs[0] != "hello" {
		t.Fatalf("Confirm 收到的命令不匹配：cmd=%q args=%v", gotCmd, gotArgs)
	}
	if !strings.Contains(res.Data, "hello") {
		t.Fatalf("输出应包含 hello，got %q", res.Data)
	}
}

func TestRunExec_Timeout(t *testing.T) {
	res, err := runExec(context.Background(), ExecConfig{
		Allow:   []string{"sleep"},
		Timeout: 50 * time.Millisecond,
		Confirm: func(string, []string) bool { return true },
	}, execArgs("sleep", "5"))
	if err != nil {
		t.Fatalf("runExec: %v", err)
	}
	if !res.IsError || !strings.Contains(res.Data, "超时") {
		t.Fatalf("超时命令应报超时，got %q", res.Data)
	}
}

func TestTruncateExecOutput(t *testing.T) {
	short := "hi"
	if got := truncateExecOutput(short); got != short {
		t.Fatalf("短输出不应截断，got %q", got)
	}
	long := strings.Repeat("界", execOutputLimit+100) // 中文多字节，验证 rune 安全切分
	got := truncateExecOutput(long)
	if len([]rune(got)) != execOutputLimit+7 { // 截断标记 "\n…（已截断）" 为 7 rune
		t.Fatalf("截断后 rune 数 = %d, want %d", len([]rune(got)), execOutputLimit+7)
	}
	if !strings.HasSuffix(got, "（已截断）") {
		t.Fatalf("截断应带标记，got %q", got[len(got)-20:])
	}
}
