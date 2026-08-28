package agent

import (
	"context"
	"strings"
	"testing"

	"small/internal/policy"
	"small/internal/tool"
)

// permAgent 构造带权限表/确认回调的测试 Agent：首轮模型请求 echo，次轮给最终回复。
// confirm 传 nil 表示不注入确认回调（用 WithToolConfirm 与否控制）。
func permAgent(perms map[string]policy.Permission, confirm func(string) bool) (*Agent, *scriptCompleter) {
	reg := tool.New()
	if err := reg.Register(scriptTool()); err != nil {
		panic(err)
	}
	script := &scriptCompleter{results: []Result{
		{Reply: "calling", ToolCalls: []ToolCall{toolCall("c1", "echo", `{"v":"hi"}`)}},
		{Reply: "done"},
	}}
	opts := []Option{WithToolPermissions(perms)}
	if confirm != nil {
		opts = append(opts, WithToolConfirm(confirm))
	}
	return New(script, reg, nil, opts...), script
}

// historyHas 历史是否含指定 Role 且 Content 匹配子串。
func historyHas(a *Agent, role, substr string) bool {
	for _, tr := range a.History() {
		if tr.Role == role && strings.Contains(tr.Content, substr) {
			return true
		}
	}
	return false
}

func TestAgent_PermissionPass(t *testing.T) {
	// Pass 工具直接放行，确认回调不应被触发。
	a, _ := permAgent(map[string]policy.Permission{"echo": policy.Pass},
		func(string) bool { t.Error("Pass 工具不应触发确认回调"); return false })
	if _, err := a.Run(context.Background(), "hi"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !historyHas(a, "tool", "hi") {
		t.Fatal("Pass 工具应执行并回灌结果")
	}
}

func TestAgent_PermissionAskConfirmed(t *testing.T) {
	called := false
	a, _ := permAgent(map[string]policy.Permission{"echo": policy.Ask},
		func(name string) bool { called = true; return name == "echo" && true })
	if _, err := a.Run(context.Background(), "hi"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !called {
		t.Fatal("Ask 工具应触发确认回调")
	}
	if !historyHas(a, "tool", "hi") {
		t.Fatal("确认放行后工具应执行")
	}
}

func TestAgent_PermissionAskDenied(t *testing.T) {
	// 确认拒绝：工具不执行、observer 不触发、拒绝原因回灌。
	observed := 0
	a, _ := permAgent(map[string]policy.Permission{"echo": policy.Ask},
		func(string) bool { return false })
	a.observe = func(ToolCallEvent) { observed++ }

	if _, err := a.Run(context.Background(), "hi"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if observed != 0 {
		t.Fatalf("被拒工具不应触发 observer，got %d", observed)
	}
	if historyHas(a, "tool", "hi") {
		t.Fatal("被拒工具不应执行")
	}
	if !historyHas(a, "tool", "被拒绝") {
		t.Fatal("拒绝原因应回灌历史")
	}
}

func TestAgent_PermissionAskNoConfirmFailClosed(t *testing.T) {
	// Ask 工具无确认回调：fail-closed 拒绝（不注入 WithToolConfirm）。
	a, _ := permAgent(map[string]policy.Permission{"echo": policy.Ask}, nil)
	if _, err := a.Run(context.Background(), "hi"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if historyHas(a, "tool", "hi") {
		t.Fatal("Ask 无确认回调不应执行")
	}
	if !historyHas(a, "tool", "被拒绝") {
		t.Fatal("fail-closed 拒绝原因应回灌")
	}
}

func TestAgent_PermissionTableNilDefaultsPass(t *testing.T) {
	// 不注入权限表：行为与旧版一致（全 Pass）。
	a, _ := permAgent(nil, nil)
	if _, err := a.Run(context.Background(), "hi"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !historyHas(a, "tool", "hi") {
		t.Fatal("无权限表应全 Pass（工具执行）")
	}
}

func TestAgent_PermissionUnknownToolDefaultsPass(t *testing.T) {
	// 权限表外工具（echo 不在表内）：缺省 Pass。
	a, _ := permAgent(map[string]policy.Permission{"other": policy.Ask}, nil)
	if _, err := a.Run(context.Background(), "hi"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !historyHas(a, "tool", "hi") {
		t.Fatal("表外工具应缺省 Pass（执行）")
	}
}
