package agent

// 上下文预算估算与截断：防 context window 溢出（compaction）。
// 第一版只做"预算估算 + 滑动窗口截断"，不做摘要（零新增 LLM 调用），
// 设计决策见 Zoo/compaction.md。

// estimateTokens 字符近似估算一段文本的 token 数：约 1 字符 ≈ 1/3 token，
// 保守取整偏大——宁多估不溢出（DeepSeek 无 Go 官方 tokenizer，近似足够）。
func estimateTokens(s string) int {
	return len(s)/3 + 1
}

// msgTokens 估算一条消息的 token：正文 + role/分隔符等固定开销（+4），
// 工具调用的参数 JSON 也计入（同样按字符近似）。
func msgTokens(t Turn) int {
	total := estimateTokens(t.Content) + 4
	for _, c := range t.ToolCalls {
		total += estimateTokens(c.Args) + 4
	}
	return total
}

// historyTokens 估算当前历史的 token 总量（不含 system）。
func (a *Agent) historyTokens() int {
	total := 0
	for _, t := range a.history {
		total += msgTokens(t)
	}
	return total
}

// totalTokens 估算完整请求的 token 总量（含 system），预算检查以此为基准。
func (a *Agent) totalTokens() int {
	return estimateTokens(a.system) + 4 + a.historyTokens()
}

// currentTokens 估算当前全部输入（含 system）的 token：
// 有真实基线（usage.prompt_tokens）时用"基线 + 新增估算"——基线精确覆盖旧历史
// （含 system 与 tools 声明），只对新增消息估算；无基线时纯估算兜底
// （首轮 / 截断后 / 恢复后）。
func (a *Agent) currentTokens() int {
	if a.baseline > 0 && a.baselineLen <= len(a.history) {
		total := a.baseline
		for _, t := range a.history[a.baselineLen:] {
			total += msgTokens(t)
		}
		return total
	}
	return a.totalTokens()
}

// enforceBudget 在追加本轮用户输入后调用：估算超预算则从头部截断历史，
// 直到不超（system 不在 history 里，天然保留）。
//
// 截断规则保证正确性：
//   - 丢 assistant(ToolCalls) 时连带丢紧随其后的连续 tool 结果——两者是配对，
//     拆开会让模型看到悬空工具结果；
//   - 至少保留最近一条 user，否则本轮对话无法继续（正确性优先于严格预算）；
//   - 截断后真实基线失效（截断改了历史，基线不再对应当前内容）→ 置 0 回到纯估算，
//     下一次 Complete 后重新获得；
//   - 发生截断且启用持久化时，同步重写会话文件（append-only 无法删旧行，
//     不同步盘会导致重启恢复"复活"被截断数据，见 compaction.md 3.3）。
func (a *Agent) enforceBudget() error {
	if a.budget <= 0 || len(a.history) == 0 {
		return nil
	}
	total := a.currentTokens()
	i := 0
	for i < len(a.history) && total > a.budget {
		total -= msgTokens(a.history[i])
		i++
		if a.history[i-1].Role == "assistant" && len(a.history[i-1].ToolCalls) > 0 {
			for i < len(a.history) && a.history[i].Role == "tool" {
				total -= msgTokens(a.history[i])
				i++
			}
		}
	}
	// 兜底：丢弃位置不得越过最后一条 user（至少保留最近一轮）。
	if last := lastUserIndex(a.history); last >= 0 && i > last {
		i = last
	}
	if i == 0 {
		return nil // 未超预算，无截断
	}
	a.history = append([]Turn(nil), a.history[i:]...)
	a.baseline, a.baselineLen = 0, 0 // 基线失效，见上注释
	if a.store != nil && a.sid != "" {
		if err := a.store.Rewrite(a.sid, toSessionMsgs(a.history)); err != nil {
			return err
		}
	}
	a.persisted = len(a.history)
	return nil
}

// lastUserIndex 返回最后一条 user 消息的下标；不存在返回 -1。
func lastUserIndex(turns []Turn) int {
	for j := len(turns) - 1; j >= 0; j-- {
		if turns[j].Role == "user" {
			return j
		}
	}
	return -1
}
