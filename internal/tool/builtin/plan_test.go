package builtin

import (
	"context"
	"encoding/json"
	"small/internal/tool"
	"strings"
	"testing"
)

func planArgs(action, text, status string, id int) json.RawMessage {
	in, _ := json.Marshal(map[string]any{
		"action": action, "text": text, "status": status, "id": id,
	})
	return in
}

func TestPlanStore_AddUpdateList(t *testing.T) {
	s := NewPlanStore()
	if got := s.Add("查文件"); got != 1 {
		t.Fatalf("Add id = %d, want 1", got)
	}
	if got := s.Add("改代码"); got != 2 {
		t.Fatalf("Add id = %d, want 2", got)
	}
	if err := s.Update(1, "done"); err != nil {
		t.Fatalf("Update: %v", err)
	}
	out := s.ListText()
	for _, want := range []string{"☑ 查文件", "☐ 改代码"} {
		if !strings.Contains(out, want) {
			t.Fatalf("ListText 应含 %q，got %q", want, out)
		}
	}
}

func TestPlanStore_UpdateErrors(t *testing.T) {
	s := NewPlanStore()
	s.Add("x")
	if err := s.Update(99, "done"); err == nil || !strings.Contains(err.Error(), "不存在") {
		t.Fatalf("更新不存在步骤应报错，got %v", err)
	}
	if err := s.Update(1, "weird"); err == nil || !strings.Contains(err.Error(), "非法状态") {
		t.Fatalf("非法状态应报错，got %v", err)
	}
}

func TestRunPlan_NoStoreFailClosed(t *testing.T) {
	// ctx 未注入清单（无 Run 上下文）：fail-closed，业务失败回灌。
	res, err := runPlan(context.Background(), planArgs("add", "x", "", 0))
	if err != nil || !res.IsError {
		t.Fatalf("无清单应失败回灌，got res=%+v err=%v", res, err)
	}
}

func TestRunPlan_AddList(t *testing.T) {
	store := NewPlanStore()
	ctx := WithPlan(context.Background(), store)
	if res, err := runPlan(ctx, planArgs("add", " 查文件 ", "", 0)); err != nil || res.IsError {
		t.Fatalf("add 失败：res=%+v err=%v", res, err)
	}
	res, err := runPlan(ctx, planArgs("list", "", "", 0))
	if err != nil || res.IsError || !strings.Contains(res.Data, "查文件") {
		t.Fatalf("list 应含步骤，got %q err=%v", res.Data, err)
	}
	// add 返回最新清单（展示边界：状态变化即展示）。
	res, err = runPlan(ctx, planArgs("add", "改代码", "", 0))
	if err != nil || !strings.Contains(res.Data, "☐ 改代码") {
		t.Fatalf("add 后应返回含新步骤的清单，got %q err=%v", res.Data, err)
	}
}

func TestRunPlan_Update(t *testing.T) {
	store := NewPlanStore()
	ctx := WithPlan(context.Background(), store)
	_, _ = runPlan(ctx, planArgs("add", "查文件", "", 0))
	res, err := runPlan(ctx, planArgs("update", "", "done", 1))
	if err != nil || res.IsError || !strings.Contains(res.Data, "☑ 查文件") {
		t.Fatalf("update 应标记完成，got %q err=%v", res.Data, err)
	}
}

func TestRunPlan_BadArgs(t *testing.T) {
	ctx := WithPlan(context.Background(), NewPlanStore())
	cases := []struct {
		name string
		args json.RawMessage
	}{
		{"非法 JSON", json.RawMessage(`{`)},
		{"空 text", planArgs("add", "  ", "", 0)},
		{"缺 action", planArgs("", "", "", 0)},
		{"未知 action", planArgs("nuke", "", "", 0)},
	}
	for _, c := range cases {
		res, err := runPlan(ctx, c.args)
		if err != nil || !res.IsError {
			t.Fatalf("%s: 应业务失败，got res=%+v err=%v", c.name, res, err)
		}
	}
}

func TestRunPlan_RunIsolation(t *testing.T) {
	// 生命周期边界：新 Run（新 store）清单为空，旧 Run 不复活（plan.md §5）。
	ctx1 := WithPlan(context.Background(), NewPlanStore())
	_, _ = runPlan(ctx1, planArgs("add", "第一步", "", 0))
	ctx2 := WithPlan(context.Background(), NewPlanStore())
	res, err := runPlan(ctx2, planArgs("list", "", "", 0))
	if err != nil || res.IsError || !strings.Contains(res.Data, "无计划步骤") {
		t.Fatalf("新 Run 清单应为空，got %q err=%v", res.Data, err)
	}
}

func TestRegister_PlanAlwaysRegistered(t *testing.T) {
	// plan 无构造依赖（状态走 ctx），应无条件注册。
	reg := tool.New()
	if err := RegisterBuiltins(reg, Deps{}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, ok := reg.Get("plan"); !ok {
		t.Fatal("plan should be registered unconditionally")
	}
}
