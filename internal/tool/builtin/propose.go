package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"small/internal/tool"
)

// propose 工具族：提议式写入（propose_file_write / propose_file_edit）。
// 语义（tool-fs.md §4.5）：改动暂存到 ProposedStore（Run 级 ctx 注入，形态对齐 PlanStore），
// **不碰磁盘**；组合根在每轮 Run 结束后检查待确认提议——展示摘要、读用户输入，
// 确认后调 ApplyProposed 落地（复用 writeFileAtomic）。agent 循环零改动。
// 落地即"权限横切"的最小形态：propose 把"写"的决策权完全交给组合根（用户侧），
// 与 exec/file_write 的每步 Confirm 同一安全取向（fail-closed）。

// ProposedEdit 一条待确认的改动（Run 内有效）。
type ProposedEdit struct {
	ID      int
	Path    string // 相对工作区根
	Kind    string // "write" / "edit"
	Content string // 落地时的完整新内容（edit 已在提议时算好，落地无需重新匹配）
}

// ProposedStore Run 级提议暂存（生命周期与 Run 绑定：组合根每轮新建并注入 ctx，
// Run 结束即处理/丢弃，恢复会话不复活——同 PlanStore 语义）。
type ProposedStore struct {
	nextID int
	items  []ProposedEdit
}

// NewProposedStore 构造空暂存（ID 从 1 起，用户可见）。
func NewProposedStore() *ProposedStore {
	return &ProposedStore{nextID: 1}
}

// Add 追加一条提议，返回其 ID。
func (s *ProposedStore) Add(e ProposedEdit) int {
	e.ID = s.nextID
	s.nextID++
	s.items = append(s.items, e)
	return e.ID
}

// Pending 返回全部待确认提议。
func (s *ProposedStore) Pending() []ProposedEdit { return s.items }

// Clear 清空（组合根处理完一轮后调用，防重复提示）。
func (s *ProposedStore) Clear() {
	s.items = nil
	s.nextID = 1
}

// proposalsCtxKey ctx 键：包级私有类型防外部碰撞（对齐 plan.md §3）。
type proposalsCtxKey struct{}

// WithProposals 把 Run 级提议暂存放入 ctx（组合根每轮 Run 调用）。
func WithProposals(ctx context.Context, s *ProposedStore) context.Context {
	return context.WithValue(ctx, proposalsCtxKey{}, s)
}

// proposalsFromCtx 从 ctx 取提议暂存；未注入返回 nil（fail-closed：无暂存不提议）。
func proposalsFromCtx(ctx context.Context) *ProposedStore {
	s, _ := ctx.Value(proposalsCtxKey{}).(*ProposedStore)
	return s
}

// ProposeFileWrite 构造提议写文件工具（propose_file_write）。
// 只暂存不落盘；确认由组合根在 Run 结束后完成（ApplyProposed）。
func ProposeFileWrite(cfg *FileConfig) tool.Tool {
	return tool.NewFunc(
		tool.Spec{
			Name:        "propose_file_write",
			Description: "提议写入文件（不立即写盘）：改动暂存，用户确认后才落地。参数同 file_write（path/content，整体覆盖，不自动建目录）。当改动需要用户先审阅再落定时使用；确认由用户在对话外完成，工具返回提议编号。",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"path": {"type": "string", "description": "文件路径（相对工作区根）"},
					"content": {"type": "string", "description": "写入的完整内容（可为空串）"}
				},
				"required": ["path", "content"]
			}`),
		},
		func(ctx context.Context, args json.RawMessage) (tool.Result, error) {
			return runProposeFileWrite(ctx, cfg, args)
		},
	)
}

// runProposeFileWrite 提议写文件执行逻辑。
func runProposeFileWrite(ctx context.Context, cfg *FileConfig, args json.RawMessage) (tool.Result, error) {
	store := proposalsFromCtx(ctx)
	if store == nil {
		// fail-closed：组合根未注入暂存（无 Run 上下文）→ 拒绝提议。
		return tool.Result{Data: "propose_file_write 不可用：当前上下文未启用提议暂存", IsError: true}, nil
	}
	var in struct {
		Path    string  `json:"path"`
		Content *string `json:"content"` // 指针区分"未传"与"空串"
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return tool.Result{Data: "参数错误: " + err.Error(), IsError: true}, nil
	}
	if strings.TrimSpace(in.Path) == "" {
		return tool.Result{Data: "参数错误: path 为空", IsError: true}, nil
	}
	if in.Content == nil {
		return tool.Result{Data: "参数错误: content 缺失", IsError: true}, nil
	}
	abs, err := resolveInRoot(cfg.Root, strings.TrimSpace(in.Path))
	if err != nil {
		return tool.Result{Data: err.Error(), IsError: true}, nil
	}
	// 提前校验父目录：避免用户确认一个注定落不了地的提议（与 file_write 同约束）。
	if fi, err := os.Stat(filepath.Dir(abs)); err != nil || !fi.IsDir() {
		return tool.Result{Data: fmt.Sprintf("父目录不存在：%s（propose_file_write 不自动建目录）", filepath.Dir(in.Path)), IsError: true}, nil
	}
	id := store.Add(ProposedEdit{Path: in.Path, Kind: "write", Content: *in.Content})
	return tool.Result{Data: fmt.Sprintf("已提议 #%d：写入 %s（%d 字节），等待用户确认后落地", id, in.Path, len(*in.Content))}, nil
}

// ProposeFileEdit 构造提议编辑工具（propose_file_edit）。
// 暂存时即校验替换并计算落地内容（复用 applyReplacements），不碰盘。
func ProposeFileEdit(cfg *FileConfig) tool.Tool {
	return tool.NewFunc(
		tool.Spec{
			Name:        "propose_file_edit",
			Description: "提议编辑文件（不立即写盘）：改动暂存，用户确认后才落地。参数同 file_edit（path/replacements/allow_multiple）。当改动需要用户先审阅再落定时使用；确认由用户在对话外完成，工具返回提议编号。",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"path": {"type": "string", "description": "文件路径（相对工作区根）"},
					"replacements": {"type": "array", "items": {"type": "object", "properties": {"old_string": {"type": "string"}, "new_string": {"type": "string"}}, "required": ["old_string", "new_string"]}, "description": "替换组（≥1 组，顺序执行）"},
					"allow_multiple": {"type": "boolean", "description": "允许 old_string 多处命中并全部替换（默认 false）"}
				},
				"required": ["path", "replacements"]
			}`),
		},
		func(ctx context.Context, args json.RawMessage) (tool.Result, error) {
			return runProposeFileEdit(ctx, cfg, args)
		},
	)
}

// runProposeFileEdit 提议编辑执行逻辑。
func runProposeFileEdit(ctx context.Context, cfg *FileConfig, args json.RawMessage) (tool.Result, error) {
	store := proposalsFromCtx(ctx)
	if store == nil {
		return tool.Result{Data: "propose_file_edit 不可用：当前上下文未启用提议暂存", IsError: true}, nil
	}
	var in struct {
		Path          string            `json:"path"`
		Replacements  []editReplacement `json:"replacements"`
		AllowMultiple bool              `json:"allow_multiple"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return tool.Result{Data: "参数错误: " + err.Error(), IsError: true}, nil
	}
	if strings.TrimSpace(in.Path) == "" {
		return tool.Result{Data: "参数错误: path 为空", IsError: true}, nil
	}
	if len(in.Replacements) == 0 {
		return tool.Result{Data: "参数错误: replacements 为空", IsError: true}, nil
	}
	abs, err := resolveInRoot(cfg.Root, strings.TrimSpace(in.Path))
	if err != nil {
		return tool.Result{Data: err.Error(), IsError: true}, nil
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return tool.Result{Data: "读取失败: " + err.Error(), IsError: true}, nil
	}
	// 提议时即校验替换合法性并计算落地内容：无效提议当场拒绝，不让用户确认注定失败的改动。
	content, done, failed := applyReplacements(data, in.Replacements, in.AllowMultiple)
	if done == 0 {
		return tool.Result{Data: "提议无效：\n- " + strings.Join(failed, "\n- "), IsError: true}, nil
	}
	id := store.Add(ProposedEdit{Path: in.Path, Kind: "edit", Content: content})
	out := fmt.Sprintf("已提议 #%d：编辑 %s（替换 %d 组），等待用户确认后落地", id, in.Path, done)
	if len(failed) > 0 {
		out += "；跳过：" + strings.Join(failed, "；")
	}
	return tool.Result{Data: out}, nil
}

// ApplyProposed 落地一条已确认的提议（组合根在用户确认后调用）。
// 写入走 writeFileAtomic（原子写）；落地即"确认后执行"，不再二次确认。
func ApplyProposed(cfg *FileConfig, e ProposedEdit) error {
	abs, err := resolveInRoot(cfg.Root, e.Path)
	if err != nil {
		return err
	}
	return writeFileAtomic(abs, []byte(e.Content))
}
