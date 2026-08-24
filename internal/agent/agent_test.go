package agent

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"small/internal/tool"
)

// scriptCompleter 按脚本返回结果并记录每次收到的历史，用于确定性驱动循环。
type scriptCompleter struct {
	results  []Result
	captured [][]Turn
	calls    int
}

func (s *scriptCompleter) Complete(_ context.Context, turns []Turn) (Result, error) {
	if s.calls >= len(s.results) {
		return Result{}, errors.New("script exhausted")
	}
	s.captured = append(s.captured, append([]Turn(nil), turns...))
	r := s.results[s.calls]
	s.calls++
	return r, nil
}

// scriptTool 回显 args.v 的测试桩工具。
func scriptTool() tool.Tool {
	return tool.NewFunc(
		tool.Spec{Name: "echo", Description: "echo", Parameters: json.RawMessage(`{}`)},
		func(_ context.Context, args json.RawMessage) (tool.Result, error) {
			var in struct{ V string }
			if err := json.Unmarshal(args, &in); err != nil {
				return tool.Result{Data: "bad args", IsError: true}, nil
			}
			return tool.Result{Data: in.V}, nil
		},
	)
}

// failTool 执行必失败的测试桩工具（模拟框架级错误）。
func failTool() tool.Tool {
	return tool.NewFunc(
		tool.Spec{Name: "boom", Description: "boom", Parameters: json.RawMessage(`{}`)},
		func(context.Context, json.RawMessage) (tool.Result, error) {
			return tool.Result{}, errors.New("boom")
		},
	)
}

func toolCall(id, name, args string) ToolCall {
	return ToolCall{ID: id, Name: name, Args: args}
}

// ---- 纯对话（无工具） ----

func TestAgent_RunToolObserver(t *testing.T) {
	reg := tool.New()
	if err := reg.Register(scriptTool()); err != nil {
		t.Fatalf("register: %v", err)
	}
	script := &scriptCompleter{results: []Result{
		{ToolCalls: []ToolCall{toolCall("c1", "echo", `{"v":"hello"}`)}},
		{Reply: "done"},
	}}
	var events []ToolCallEvent
	a := New(script, reg, nil, WithSystemPrompt("sys"), WithToolObserver(func(ev ToolCallEvent) {
		events = append(events, ev)
	}))

	if _, err := a.Run(context.Background(), "hi"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// 只实际执行了 echo 一次：应恰好一个事件，且携带名称/入参/结果。
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1（未注入时无事件）", len(events))
	}
	if events[0].Name != "echo" || events[0].Args != `{"v":"hello"}` {
		t.Errorf("event = %+v, want echo call with args", events[0])
	}
	if events[0].Result.Data != "hello" || events[0].Result.IsError {
		t.Errorf("event result = %+v, want echo output", events[0].Result)
	}
	// 耗时/轮次随事件流出（trace 的输入）：首轮执行耗时非负，轮次从 0 起。
	if events[0].Duration < 0 {
		t.Errorf("duration = %v, want >= 0", events[0].Duration)
	}
	if events[0].Round != 0 {
		t.Errorf("round = %d, want 0（单轮工具循环）", events[0].Round)
	}
}

func TestAgent_RunPlainChat(t *testing.T) {
	script := &scriptCompleter{results: []Result{{Reply: "hi back", Thinking: "secret"}}}
	a := New(script, nil, nil, WithSystemPrompt("sys"))

	result, err := a.Run(context.Background(), "hello")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Reply != "hi back" {
		t.Errorf("reply = %q", result.Reply)
	}
	// 历史 = [user, assistant]；系统提示不持久化，仅在请求时经 allTurns 注入。
	want := []Turn{
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "hi back"},
	}
	if got := a.History(); !reflect.DeepEqual(got, want) {
		t.Errorf("history = %+v, want %+v", got, want)
	}
	// 请求侧历史首条是系统提示；思考过程不入历史（请求里也不该出现）。
	captured := script.captured[0]
	if len(captured) != 2 || captured[0].Role != "system" || captured[0].Content != "sys" {
		t.Errorf("request turns = %+v, want [system, user]", captured)
	}
	for _, tr := range a.History() {
		if tr.Content == "secret" {
			t.Error("thinking must not enter history")
		}
	}
}

// ---- 工具循环 ----

func TestAgent_RunToolLoop(t *testing.T) {
	reg := tool.New()
	if err := reg.Register(scriptTool()); err != nil {
		t.Fatalf("register: %v", err)
	}
	// 第一轮请求 echo 工具，第二轮给出最终回复。
	script := &scriptCompleter{results: []Result{
		{ToolCalls: []ToolCall{toolCall("c1", "echo", `{"v":"hello"}`)}},
		{Reply: "done"},
	}}
	a := New(script, reg, nil, WithSystemPrompt("sys"))

	result, err := a.Run(context.Background(), "hi")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Reply != "done" {
		t.Errorf("reply = %q, want done", result.Reply)
	}

	// 第二轮完整历史应包含：assistant(带调用) + tool(结果回灌)。
	captured := script.captured[1]
	want := []Turn{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "hi"},
		{Role: "assistant", ToolCalls: []ToolCall{toolCall("c1", "echo", `{"v":"hello"}`)}},
		{Role: "tool", ToolCallID: "c1", Content: "hello"},
	}
	if !reflect.DeepEqual(captured, want) {
		t.Errorf("round 2 turns = %+v, want %+v", captured, want)
	}
	if script.calls != 2 {
		t.Errorf("completer calls = %d, want 2", script.calls)
	}
}

func TestAgent_RunUnknownToolFallsBack(t *testing.T) {
	reg := tool.New()
	if err := reg.Register(scriptTool()); err != nil {
		t.Fatalf("register: %v", err)
	}
	// 模型请求了未注册工具 "ghost"：应回灌错误结果而非中止循环。
	script := &scriptCompleter{results: []Result{
		{ToolCalls: []ToolCall{toolCall("c9", "ghost", "{}")}},
		{Reply: "recovered"},
	}}
	a := New(script, reg, nil)

	result, err := a.Run(context.Background(), "hi")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Reply != "recovered" {
		t.Errorf("reply = %q, want recovered", result.Reply)
	}
	captured := script.captured[1]
	toolTurn := captured[len(captured)-1]
	if toolTurn.Role != "tool" || toolTurn.ToolCallID != "c9" || !strings.Contains(toolTurn.Content, "ghost") {
		t.Errorf("tool turn = %+v, want ghost feedback", toolTurn)
	}
}

func TestAgent_RunToolFrameworkErrorAborts(t *testing.T) {
	reg := tool.New()
	if err := reg.Register(failTool()); err != nil {
		t.Fatalf("register: %v", err)
	}
	script := &scriptCompleter{results: []Result{
		{ToolCalls: []ToolCall{toolCall("c1", "boom", "{}")}},
	}}
	a := New(script, reg, nil)

	_, err := a.Run(context.Background(), "hi")
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("want tool error, got %v", err)
	}
	// 框架级错误应立即中止：不应有第二次 Complete。
	if script.calls != 1 {
		t.Errorf("completer calls = %d, want 1", script.calls)
	}
}

func TestAgent_RunWithoutToolsErrors(t *testing.T) {
	script := &scriptCompleter{results: []Result{
		{ToolCalls: []ToolCall{toolCall("c1", "echo", "{}")}},
	}}
	a := New(script, nil, nil) // 未注册工具，模型请求时应报错

	_, err := a.Run(context.Background(), "hi")
	if err == nil || !strings.Contains(err.Error(), "no tools registered") {
		t.Fatalf("want no-tools error, got %v", err)
	}
}

func TestAgent_RunExceedsMaxRounds(t *testing.T) {
	reg := tool.New()
	if err := reg.Register(scriptTool()); err != nil {
		t.Fatalf("register: %v", err)
	}
	// 模型每轮都请求工具：必须被 maxToolRounds 兜住，否则死循环。
	results := make([]Result, maxToolRounds)
	for i := range results {
		results[i] = Result{ToolCalls: []ToolCall{toolCall("c", "echo", `{"v":"x"}`)}}
	}
	script := &scriptCompleter{results: results}
	a := New(script, reg, nil)

	_, err := a.Run(context.Background(), "hi")
	if err == nil || !strings.Contains(err.Error(), "exceeded 8 tool rounds") {
		t.Fatalf("want max-rounds error, got %v", err)
	}
	if script.calls != maxToolRounds {
		t.Errorf("completer calls = %d, want %d", script.calls, maxToolRounds)
	}
}

func TestAgent_RunHistoryAccumulatesAcrossRuns(t *testing.T) {
	script := &scriptCompleter{results: []Result{{Reply: "one"}, {Reply: "two"}}}
	a := New(script, nil, nil)

	if _, err := a.Run(context.Background(), "first"); err != nil {
		t.Fatalf("run 1: %v", err)
	}
	if _, err := a.Run(context.Background(), "second"); err != nil {
		t.Fatalf("run 2: %v", err)
	}
	// 第二轮携带第一轮的完整历史（用户 + 助手），驱动多轮上下文。
	if len(script.captured[1]) != 3 {
		t.Errorf("round 2 turns = %+v, want 3 (history accumulated)", script.captured[1])
	}
	if first := script.captured[1][0]; !reflect.DeepEqual(first, Turn{Role: "user", Content: "first"}) {
		t.Errorf("round 2 first turn = %+v", first)
	}
}
