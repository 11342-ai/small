// Package trace 记录工具调用的观测元数据（耗时/轮次/失败标记/审计），独立成文件，
// 供调试与审计。它不参与对话循环的任何行为：模型看到的输入、回灌内容、会话历史
// 均不受影响。
//
// 核心边界：session 管"回灌模型的对话历史"，trace 管"工具调用的观测元数据"，
// 两者永不混写（对照市面 Harness trajectory / Codex rollout 与对话消息分离存储）。
//
// 本包是叶子包，只依赖标准库，不 import 任何内部包；写盘动作由组合根包装成
// agent.ToolObserver 注入，agent 完全不感知本包的存在（依赖方向见 Zoo/model/trace.md）。
package trace

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// Entry 一条工具调用轨迹记录。字段对齐 agent.ToolCallEvent 并补充元数据：
// Round 定位多轮顺序、DurationMs 记录执行耗时、Session 供跨会话聚合 grep。
type Entry struct {
	// TS 执行完成时刻（RFC3339，人读可查）。
	TS time.Time `json:"ts"`
	// Session 会话 id，跨会话聚合审计用。
	Session string `json:"session"`
	// Round 工具循环第几轮（0 起）。
	Round int `json:"round"`
	// Name 工具名。
	Name string `json:"name"`
	// Args 模型传入的原始参数 JSON。
	Args string `json:"args"`
	// Data 执行结果文本（含业务失败内容）。
	Data string `json:"data"`
	// IsError 业务失败标记。
	IsError bool `json:"isError"`
	// DurationMs 执行耗时毫秒（转 ms 而非 time.Duration 纳秒，保人读可查）。
	DurationMs int64 `json:"durationMs"`
	// Type 事件类型：缺省 "tool"（工具调用）；plan 变更记 "plan"（见 Zoo/model/plan.md §6）。
	// omitempty + 旧文件无此字段（""）按 "tool" 处理，向后兼容。
	Type string `json:"type,omitempty"`
}

// Store trace 写入器：<path> 的追加日志。
// 文件在首次 Append 时惰性打开——不调用 Append 则零副作用（纯对话/无会话时不产生文件）。
// 单会话单写者，不加并发锁（对齐 session"多进程写同一会话不支持"的约定）。
type Store struct {
	path string
	f    *os.File // 首次 Append 惰性打开；进程生命周期内不关闭
}

// New 构造 trace 写入器。仅记录目标路径、不碰磁盘；真正打开推迟到首次 Append。
func New(path string) *Store {
	return &Store{path: path}
}

// Append 追加一条轨迹。每次写入一行（单行 O_APPEND 追加在 POSIX 下原子），
// 已写入的行不因后续失败丢失。打开/写失败均返回 error，由调用方（组合根包装层）
// 决定处置——按约定观测失败属次要失败，不打断对话（见 Zoo/model/trace.md §2.2）。
func (s *Store) Append(e Entry) error {
	if s.f == nil {
		f, err := os.OpenFile(s.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return fmt.Errorf("trace: open %q: %w", s.path, err)
		}
		s.f = f
	}
	data, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("trace: marshal: %w", err)
	}
	if _, err := s.f.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("trace: append %q: %w", s.path, err)
	}
	return nil
}
