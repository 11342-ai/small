// Package session 是 agent 的存储部件：把对话历史落盘（JSONL，每会话一个文件），
// 支持列出/恢复/追加/删除，使对话可跨进程续聊。
//
// 边界约定：
//   - 本包不感知 agent 的 Turn，也不 import agent（依赖单向，避免 agent → session → agent
//     循环）；自己持有与 Turn 字段镜像的 Message 模型，翻译由 agent 侧负责。
//   - 存储形态是 append-only 日志：崩溃最多留下半行坏数据，恢复时跳过坏行，
//     已写完的完整行不丢。多进程写同一会话明确不支持（CLI 单进程不存在该场景）。
package session

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Message 一条对话消息的存储形态，字段与 agent.Turn 镜像。
// 落盘数据的唯一用途是"恢复后回灌模型"，故不含思考过程（市面模型均不回传 reasoning）。
type Message struct {
	// Role 取值 user / assistant / system / tool。
	Role string `json:"role"`
	// Content 消息正文。
	Content string `json:"content,omitempty"`
	// ToolCallID 仅 Role == "tool" 时有效：指向被执行的调用。
	ToolCallID string `json:"tool_call_id,omitempty"`
	// ToolCalls 仅 Role == "assistant" 时有效：模型请求的工具调用列表。
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
}

// ToolCall 一次工具调用的存储形态，与 agent.ToolCall 镜像。
type ToolCall struct {
	// ID 调用的唯一标识，结果回灌时必须原样带回。
	ID string `json:"id"`
	// Name 工具名。
	Name string `json:"name"`
	// Args 参数 JSON 文本（原始形态）。
	Args string `json:"args"`
}

// Meta 会话的列表元数据，来自文件 stat 与行数。
type Meta struct {
	// ID 会话标识（不含路径分隔符，即文件名去后缀）。
	ID string
	// TurnCount 消息条数（文件行数）。
	TurnCount int
	// UpdatedAt 文件最近修改时间。
	UpdatedAt time.Time
}

// fileExt 会话文件的扩展名，List 按它扫描目录。
const fileExt = ".jsonl"

// Store 会话仓库：<dir>/<id>.jsonl 的追加日志集合。
// 单进程内用互斥锁保证追加/读取一致（-race 干净）；多进程写同一会话不支持。
type Store struct {
	dir string
	mu  sync.Mutex
}

// New 构造会话仓库，目录不存在则自动创建。
func New(dir string) (*Store, error) {
	if dir == "" {
		return nil, errors.New("session: empty dir")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("session: create dir %q: %w", dir, err)
	}
	return &Store{dir: dir}, nil
}

// List 列出全部会话，按 id 升序保证顺序稳定（重复调用结果确定，便于测试与 CLI 展示）。
func (s *Store) List() ([]Meta, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("session: read dir %q: %w", s.dir, err)
	}
	metas := make([]Meta, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), fileExt) {
			continue
		}
		id := strings.TrimSuffix(e.Name(), fileExt)
		info, err := e.Info()
		if err != nil {
			return nil, fmt.Errorf("session: stat %q: %w", e.Name(), err)
		}
		metas = append(metas, Meta{
			ID:        id,
			UpdatedAt: info.ModTime(),
			TurnCount: countLines(filepath.Join(s.dir, e.Name())),
		})
	}
	sort.Slice(metas, func(i, j int) bool { return metas[i].ID < metas[j].ID })
	return metas, nil
}

// Load 恢复会话的全部消息。文件不存在视为新会话（返回空历史）；坏行跳过不报错
// ——坏行是崩溃留下的半行痕迹，属正常容错而非异常。
func (s *Store) Load(id string) ([]Message, error) {
	if err := validateID(id); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	f, err := os.Open(s.path(id))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // 新会话：空历史不是错误
		}
		return nil, fmt.Errorf("session: open %q: %w", id, err)
	}
	defer f.Close()

	var msgs []Message
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var m Message
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			continue // 坏行跳过：崩溃容错，见包注释
		}
		msgs = append(msgs, m)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("session: read %q: %w", id, err)
	}
	return msgs, nil
}

// Append 把消息追加进会话文件。每次写入一行（单行 O_APPEND 追加在 POSIX 下原子），
// 已完成的轮次天然不丢；追加时不覆盖既有内容。
func (s *Store) Append(id string, msgs []Message) error {
	if err := validateID(id); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	f, err := os.OpenFile(s.path(id), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("session: open %q: %w", id, err)
	}
	defer f.Close()
	for _, m := range msgs {
		data, err := json.Marshal(m)
		if err != nil {
			return fmt.Errorf("session: marshal message: %w", err)
		}
		if _, err := f.Write(append(data, '\n')); err != nil {
			return fmt.Errorf("session: append %q: %w", id, err)
		}
	}
	return nil
}

// Delete 删除会话文件；文件不存在视为删除成功（幂等）。
func (s *Store) Delete(id string) error {
	if err := validateID(id); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := os.Remove(s.path(id)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("session: delete %q: %w", id, err)
	}
	return nil
}

// path 返回会话文件的完整路径。调用方必须先 validateID。
func (s *Store) path(id string) string {
	return filepath.Join(s.dir, id+fileExt)
}

// validateID 校验会话 id：非空、不含路径分隔符，防止把任意路径当文件名（路径注入）。
func validateID(id string) error {
	if id == "" {
		return errors.New("session: empty id")
	}
	if id == "." || id == ".." || strings.ContainsAny(id, `/\`) {
		return fmt.Errorf("session: invalid id %q", id)
	}
	return nil
}

// countLines 统计文件行数；读失败按 0 处理（List 尽力而为，不因单个文件阻塞）。
func countLines(path string) int {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	n := 0
	for sc.Scan() {
		n++
	}
	return n
}
