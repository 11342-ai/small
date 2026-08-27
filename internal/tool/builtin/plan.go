package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"small/internal/tool"
)

// PlanStep 一步计划（纯内存，Run 内有效，见 Zoo/model/plan.md）。
type PlanStep struct {
	ID     int    `json:"id"`
	Text   string `json:"text"`
	Status string `json:"status"` // pending / in_progress / done
}

// planStatuses 合法状态集合（防模型乱填）。
func validPlanStatus(s string) bool {
	switch s {
	case "pending", "in_progress", "done":
		return true
	}
	return false
}

// planMark 状态 → 展示标记。
func planMark(status string) string {
	switch status {
	case "in_progress":
		return "◐"
	case "done":
		return "☑"
	default:
		return "☐"
	}
}

// PlanStore Run 级计划清单（状态生命周期与 Run 绑定，见 Zoo/model/plan.md §3）。
// 组合根每轮 Run 新建并放进 ctx（WithPlan），工具从 ctx 取（planFromCtx）；
// Run 结束即弃，恢复会话不复活，无需清理逻辑。
type PlanStore struct {
	steps  []PlanStep
	nextID int
}

// NewPlanStore 构造空清单（ID 从 1 起，用户可见）。
func NewPlanStore() *PlanStore {
	return &PlanStore{nextID: 1}
}

// Add 追加一步（状态 pending），返回步骤 ID。
func (s *PlanStore) Add(text string) int {
	id := s.nextID
	s.nextID++
	s.steps = append(s.steps, PlanStep{ID: id, Text: text, Status: "pending"})
	return id
}

// Update 更新指定步骤状态；ID 不存在或状态非法返回 error（业务失败回灌给模型）。
func (s *PlanStore) Update(id int, status string) error {
	if !validPlanStatus(status) {
		return fmt.Errorf("非法状态 %q（可选 pending/in_progress/done）", status)
	}
	for i := range s.steps {
		if s.steps[i].ID == id {
			s.steps[i].Status = status
			return nil
		}
	}
	return fmt.Errorf("步骤 %d 不存在", id)
}

// ListText 渲染当前清单（含状态标记），模型与展示层（observer）同时消费。
func (s *PlanStore) ListText() string {
	if len(s.steps) == 0 {
		return "（当前无计划步骤）"
	}
	var b strings.Builder
	for i, st := range s.steps {
		fmt.Fprintf(&b, "%d. %s %s\n", i+1, planMark(st.Status), st.Text)
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// planCtxKey ctx 键：包级私有类型防外部碰撞（对齐 plan.md §3）。
type planCtxKey struct{}

// WithPlan 把 Run 级计划清单放入 ctx（组合根每轮 Run 调用，agent 零改动）。
func WithPlan(ctx context.Context, s *PlanStore) context.Context {
	return context.WithValue(ctx, planCtxKey{}, s)
}

// planFromCtx 从 ctx 取计划清单；未注入返回 nil（fail-closed：无清单不记录）。
func planFromCtx(ctx context.Context) *PlanStore {
	s, _ := ctx.Value(planCtxKey{}).(*PlanStore)
	return s
}

// Plan 构造计划清单工具（plan）。
// 模型自维护 Run 内分步清单，展示给用户看进度（Claude Code todoWrite 同款）。
// 触发约束：Description 声明"仅多步任务时维护，单步任务不建"（对齐 memory_save 先例）。
// 状态经 ctx 注入而非构造注入：绕开 RegisterBuiltins 依赖注入面（tool-extend.md 边界①）。
func Plan() tool.Tool {
	return tool.NewFunc(
		tool.Spec{
			Name:        "plan",
			Description: "维护当前任务的分步执行清单（仅多步任务时使用，单步任务不要建清单）。add 追加步骤，update 更新状态（pending/in_progress/done），list 查看当前清单。每次操作返回最新清单，让用户看到任务推进。",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"action": {"type": "string", "enum": ["add", "update", "list"], "description": "操作类型"},
					"text": {"type": "string", "description": "add 时的步骤描述"},
					"id": {"type": "integer", "description": "update 时目标步骤的 id"},
					"status": {"type": "string", "enum": ["pending", "in_progress", "done"], "description": "update 时的新状态"}
				},
				"required": ["action"]
			}`),
		},
		func(ctx context.Context, args json.RawMessage) (tool.Result, error) {
			return runPlan(ctx, args)
		},
	)
}

// runPlan plan 执行逻辑（外置具名函数，可脱离工具壳独立单测）。
func runPlan(ctx context.Context, args json.RawMessage) (tool.Result, error) {
	store := planFromCtx(ctx)
	if store == nil {
		// fail-closed：组合根未注入清单（无 Run 上下文）→ 不记录，业务失败回灌。
		return tool.Result{Data: "plan 不可用：当前上下文未启用计划清单", IsError: true}, nil
	}
	var in struct {
		Action string `json:"action"`
		Text   string `json:"text"`
		ID     int    `json:"id"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return tool.Result{Data: "参数错误: " + err.Error(), IsError: true}, nil
	}
	switch in.Action {
	case "add":
		if strings.TrimSpace(in.Text) == "" {
			return tool.Result{Data: "参数错误: text 为空", IsError: true}, nil
		}
		store.Add(strings.TrimSpace(in.Text))
	case "update":
		if err := store.Update(in.ID, in.Status); err != nil {
			return tool.Result{Data: "更新失败: " + err.Error(), IsError: true}, nil
		}
	case "list":
		// 只读，无副作用。
	case "":
		return tool.Result{Data: "参数错误: action 缺失", IsError: true}, nil
	default:
		return tool.Result{Data: "参数错误: 未知 action " + in.Action, IsError: true}, nil
	}
	// 每次操作返回最新清单：模型可跟踪进度，组合根 observer 顺带展示（plan.md §5 展示边界）。
	return tool.Result{Data: store.ListText()}, nil
}
