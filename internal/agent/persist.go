package agent

import "small/internal/session"

// 本文件是 agent 与 session 之间的翻译隔离点：Turn ↔ session.Message 的转换
// 只发生在这里。session 是存储部件（不 import agent，避免循环依赖），
// 因此必须维护一份与 Turn 镜像的数据模型，翻译成本是字段级拷贝。

// toSessionMsgs 把 turn 历史翻译成落盘消息（持久化用）。
func toSessionMsgs(turns []Turn) []session.Message {
	msgs := make([]session.Message, 0, len(turns))
	for _, t := range turns {
		m := session.Message{Role: t.Role, Content: t.Content, ToolCallID: t.ToolCallID}
		for _, c := range t.ToolCalls {
			m.ToolCalls = append(m.ToolCalls, session.ToolCall{ID: c.ID, Name: c.Name, Args: c.Args})
		}
		msgs = append(msgs, m)
	}
	return msgs
}

// FromSession 把落盘消息翻译回领域历史（组合根恢复会话后注入用）。
func FromSession(msgs []session.Message) []Turn {
	turns := make([]Turn, 0, len(msgs))
	for _, m := range msgs {
		t := Turn{Role: m.Role, Content: m.Content, ToolCallID: m.ToolCallID}
		for _, c := range m.ToolCalls {
			t.ToolCalls = append(t.ToolCalls, ToolCall{ID: c.ID, Name: c.Name, Args: c.Args})
		}
		turns = append(turns, t)
	}
	return turns
}

// persistRun 把本轮新增的历史（history[persisted:]）追加落盘，成功后推进水位。
// 未启用持久化（store 为 nil 或 sid 为空）时为空操作。
// 只在 Run 成功路径调用：失败轮次由调用方重试整个对话（与"流中失败不回退"一致），
// 避免盘上出现半轮状态。
func (a *Agent) persistRun() error {
	if a.store == nil || a.sid == "" {
		return nil
	}
	if a.persisted >= len(a.history) {
		return nil
	}
	if err := a.store.Append(a.sid, toSessionMsgs(a.history[a.persisted:])); err != nil {
		return err
	}
	a.persisted = len(a.history)
	return nil
}
