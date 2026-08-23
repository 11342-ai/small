package agent

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"
)

// ---- 估算 ----

func TestEstimateTokens_Monotonic(t *testing.T) {
	short, long := estimateTokens("abc"), estimateTokens(strings.Repeat("x", 300))
	if short <= 0 || long <= short {
		t.Errorf("estimate monotonic broken: short=%d long=%d", short, long)
	}
}

// ---- 触发与截断（不启用持久化） ----

// TestCompact_DisabledByDefault budget<=0 时完全不截断（历史原样保留）。
func TestCompact_DisabledByDefault(t *testing.T) {
	script := &scriptCompleter{results: []Result{{Reply: "r"}}}
	a := New(script, nil, nil,
		WithHistory([]Turn{
			{Role: "user", Content: strings.Repeat("x", 200)},
			{Role: "assistant", Content: strings.Repeat("y", 200)},
		}),
	)
	if _, err := a.Run(context.Background(), "more"); err != nil {
		t.Fatalf("run: %v", err)
	}
	// 注入 2 条 + 本轮 user + assistant 回复 = 4；未启用预算时不截断。
	if len(a.History()) != 4 {
		t.Errorf("history = %d, want 4 (no truncation without budget)", len(a.History()))
	}
}

// TestCompact_NoTruncationWithinBudget 预算充足时不截断。
func TestCompact_NoTruncationWithinBudget(t *testing.T) {
	script := &scriptCompleter{results: []Result{{Reply: "r"}}}
	a := New(script, nil, nil,
		WithHistory([]Turn{{Role: "user", Content: "hi"}}),
		WithTokenBudget(10_000),
	)
	if _, err := a.Run(context.Background(), "hello"); err != nil {
		t.Fatalf("run: %v", err)
	}
	// 注入 1 条 + user + assistant = 3。
	if len(a.History()) != 3 {
		t.Errorf("history = %d, want 3", len(a.History()))
	}
}

// TestCompact_TruncatesOldKeepsRecentAndSystem 超预算：最早被丢、最近一轮保留、
// 请求侧 system 永远在首条。
func TestCompact_TruncatesOldKeepsRecentAndSystem(t *testing.T) {
	script := &scriptCompleter{results: []Result{{Reply: "r"}}}
	a := New(script, nil, nil,
		WithSystemPrompt("sys"),
		WithHistory([]Turn{
			{Role: "user", Content: strings.Repeat("a", 200)},
			{Role: "assistant", Content: strings.Repeat("b", 200)},
			{Role: "user", Content: "u2"},
			{Role: "assistant", Content: "r2"},
		}),
		WithTokenBudget(30),
	)
	if _, err := a.Run(context.Background(), "u3"); err != nil {
		t.Fatalf("run: %v", err)
	}

	got := a.History()
	// 最早的超长轮被丢，最近的 u2/r2/u3 保留。
	if len(got) == 0 || got[0].Content != "u2" {
		t.Errorf("history = %+v, want starts with u2", got)
	}
	// 请求侧首条仍是 system。
	captured := script.captured[0]
	if len(captured) == 0 || captured[0].Role != "system" || captured[0].Content != "sys" {
		t.Errorf("request turns = %+v, want system first", captured)
	}
}

// TestCompact_KeepsLastUserEvenIfOverBudget 极端小预算下仍保留最近一轮（正确性优先）。
func TestCompact_KeepsLastUserEvenIfOverBudget(t *testing.T) {
	script := &scriptCompleter{results: []Result{{Reply: "r"}}}
	a := New(script, nil, nil,
		WithHistory([]Turn{
			{Role: "user", Content: strings.Repeat("a", 300)},
			{Role: "assistant", Content: strings.Repeat("b", 300)},
		}),
		WithTokenBudget(5),
	)
	if _, err := a.Run(context.Background(), "u2"); err != nil {
		t.Fatalf("run: %v", err)
	}
	got := a.History()
	// 极端预算下仍保留最近一轮（u2 + 回复），超长的最早轮被丢。
	if len(got) == 0 || got[0].Content != "u2" {
		t.Errorf("history = %+v, want [u2, ...]", got)
	}
}

// TestCompact_ToolRoundTruncatedAsGroup 工具轮次成组丢弃：assistant(ToolCalls)
// 被丢时连带丢后续 tool 结果，保留区不出现悬空工具结果。
func TestCompact_ToolRoundTruncatedAsGroup(t *testing.T) {
	script := &scriptCompleter{results: []Result{{Reply: "r"}}}
	history := []Turn{
		{Role: "user", Content: strings.Repeat("x", 120)}, // 超长，必然先被丢
		{Role: "assistant", ToolCalls: []ToolCall{toolCall("c1", "echo", `{"v":"y"}`)}},
		{Role: "tool", ToolCallID: "c1", Content: "y"},
		{Role: "user", Content: "u2"},
		{Role: "assistant", Content: "r2"},
	}
	a := New(script, nil, nil, WithHistory(history), WithTokenBudget(20))
	if _, err := a.Run(context.Background(), "u3"); err != nil {
		t.Fatalf("run: %v", err)
	}

	got := a.History()
	for i, m := range got {
		if m.Role == "tool" {
			// tool 消息前面必须紧跟携带匹配 ToolCallID 的 assistant(ToolCalls)。
			if i == 0 || got[i-1].Role != "assistant" || !toolCallMatches(got[i-1], m.ToolCallID) {
				t.Errorf("悬空 tool 结果 at %d: %+v (history %+v)", i, m, got)
			}
		}
	}
}

// toolCallMatches 判断 assistant 消息是否携带指定 ID 的工具调用。
func toolCallMatches(assistant Turn, id string) bool {
	for _, c := range assistant.ToolCalls {
		if c.ID == id {
			return true
		}
	}
	return false
}

// ---- 与持久化的同步（用户决策：截断同步重写会话文件） ----

// TestCompact_SyncsPersistedFile 截断后盘上文件被重写，重启恢复 == 截断后历史（不复活）。
func TestCompact_SyncsPersistedFile(t *testing.T) {
	store, _ := persistedStore(t)
	script := &scriptCompleter{results: []Result{{Reply: "r"}}}
	long := []Turn{
		{Role: "user", Content: strings.Repeat("a", 200)},
		{Role: "assistant", Content: strings.Repeat("b", 200)},
		{Role: "user", Content: "u2"},
	}
	a := New(script, nil, store,
		WithSession("s1"),
		WithHistory(long),
		WithTokenBudget(30),
	)
	if _, err := a.Run(context.Background(), "u3"); err != nil {
		t.Fatalf("run: %v", err)
	}

	// 触发截断：最早的超长轮（aaa）被丢，首条变为 u2。
	got := a.History()
	if len(got) == 0 || got[0].Content != "u2" {
		t.Fatalf("history = %+v, want starts with u2 (truncated)", got)
	}
	// 模拟重启：从磁盘恢复，应与截断后的内存历史一致（旧数据被 Rewrite 抹掉）。
	msgs, err := store.Load("s1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	restored := FromSession(msgs)
	if !reflect.DeepEqual(restored, a.History()) {
		t.Errorf("restored = %+v, want %+v (must not resurrect truncated data)", restored, a.History())
	}
}

// TestCompact_BaselineRecordedFromUsage Run 成功后记录真实用量基线，供下轮预算判断。
func TestCompact_BaselineRecordedFromUsage(t *testing.T) {
	script := &scriptCompleter{results: []Result{{Reply: "r", PromptTokens: 42}}}
	a := New(script, nil, nil)
	if _, err := a.Run(context.Background(), "hi"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if a.baseline != 42 {
		t.Errorf("baseline = %d, want 42", a.baseline)
	}
	// Complete 返回瞬间历史只有 [user hi]，故 baselineLen 为 1。
	if a.baselineLen != 1 {
		t.Errorf("baselineLen = %d, want 1", a.baselineLen)
	}
}

// TestCurrentTokens_BaselineAndFallback 预算判断的四种状态：
// 无基线兜底 / 基线覆盖全部 / 基线+新增估算 / 截断后基线失效回到兜底。
func TestCurrentTokens_BaselineAndFallback(t *testing.T) {
	a := New(&scriptCompleter{results: []Result{{Reply: "r"}}}, nil, nil, WithSystemPrompt("sys"))
	a.history = []Turn{
		{Role: "user", Content: strings.Repeat("a", 120)},
		{Role: "assistant", Content: strings.Repeat("b", 120)},
	}

	// 1) 无基线（首轮/恢复）：纯估算兜底。
	if a.currentTokens() != a.totalTokens() {
		t.Error("without baseline, currentTokens must equal pure estimate")
	}

	// 2) 基线覆盖全部历史：currentTokens = baseline + 0。
	a.baseline = 10
	a.baselineLen = len(a.history)
	if a.currentTokens() != 10 {
		t.Errorf("currentTokens = %d, want 10 (baseline only)", a.currentTokens())
	}

	// 3) 基线 + 新增估算：追加一条消息（baselineLen 不变，只对新增估算）。
	a.history = append(a.history, Turn{Role: "user", Content: "u2"})
	want := 10 + msgTokens(a.history[len(a.history)-1])
	if a.currentTokens() != want {
		t.Errorf("currentTokens = %d, want %d (baseline + delta)", a.currentTokens(), want)
	}

	// 4) 截断后基线失效（baselineLen > len(history)）：回到纯估算兜底。
	a.baselineLen = len(a.history) + 5
	if a.currentTokens() != a.totalTokens() {
		t.Error("stale baseline must fall back to pure estimate")
	}
}

// TestCompact_TruncationInvalidatesBaseline 截断后基线置 0（历史变了，基线不再对应当前内容）。
func TestCompact_TruncationInvalidatesBaseline(t *testing.T) {
	script := &scriptCompleter{results: []Result{{Reply: "r"}}}
	a := New(script, nil, nil,
		WithHistory([]Turn{
			{Role: "user", Content: strings.Repeat("a", 300)},
			{Role: "assistant", Content: strings.Repeat("b", 300)},
			{Role: "user", Content: "u2"},
		}),
		WithTokenBudget(20),
	)
	a.baseline, a.baselineLen = 999, 3 // 预置基线（对应截断前的历史）
	if _, err := a.Run(context.Background(), "u3"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if a.baseline != 0 || a.baselineLen != 0 {
		t.Errorf("after truncation baseline = (%d, %d), want (0, 0)", a.baseline, a.baselineLen)
	}
}

// TestCompact_NoSyncWhenNoTruncation 未发生截断时不触发 Rewrite（继续走 append，无全量写开销）。
func TestCompact_NoSyncWhenNoTruncation(t *testing.T) {
	store, dir := persistedStore(t)
	script := &scriptCompleter{results: []Result{{Reply: "r"}}}
	a := New(script, nil, store,
		WithSession("s1"),
		WithTokenBudget(10_000),
	)
	if _, err := a.Run(context.Background(), "hi"); err != nil {
		t.Fatalf("run: %v", err)
	}
	// 未截断则不应残留 Rewrite 的临时文件。
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("unexpected temp file left: %s", e.Name())
		}
	}
}
