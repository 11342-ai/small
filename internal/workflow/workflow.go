// Package workflow 提供任务工作流分支（skill/workflow）的编译期内置定义。
//
// 对齐 persona 模式（internal/persona）：go:embed workflows/*.md，frontmatter 定义
// 元数据（名称/触发/输入输出/停止条件），正文定义步骤；提示词数据层——组合根把
// 分支清单渲染进 base 提示词，模型按触发条件自动进入对应分支执行（Zoo/model/workflow.md）。
//
// 设计要点：
//   - 模块化：每 workflow 一个 md 文件（embeded 代码资产，新增分支只加文件）；
//   - 触发双通道：提示词自动判断（模型命中 Trigger）+ 命令显式进入（/pdf，见 main）；
//   - 本包是叶子：不 import 任何内部包；agent 不感知 workflow（提示词是字符串）。
package workflow

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

//go:embed workflows/*.md
var files embed.FS

// 能力名：workflow 资产 frontmatter 的 requires 值，与组合根传给 RenderBranchFor 的
// "可用能力集"键对齐。能力名是资产词表，归资产所在包持有（见 k8s-diagnosis.md §16.5）。
const (
	// CapK8s k8s 只读诊断能力（采集器就绪）：k8s-diag 分支依赖它。
	CapK8s = "k8s"
)

// Workflow 一个任务工作流分支：元数据 + 步骤正文。
type Workflow struct {
	// Name 唯一名（如 pdf），触发判断与命令的标识。
	Name string
	// Description 一句话说明（分支清单展示）。
	Description string
	// Trigger 触发条件描述（模型判断依据：任务何时命中该分支）。
	Trigger string
	// Input 输入说明（用户需提供什么）。
	Input string
	// Output 输出说明（产物形态）。
	Output string
	// Stop 停止条件（正常结束 + 异常兜底，模型须遵守）。
	Stop string
	// Requires 该分支依赖的启动期能力名（空 = 无条件可用）。能力不满足时分支整段不注入提示词——
	// 避免"提示词里有分支、对应工具却没注册"（见 Zoo/model/k8s-diagnosis.md §16.5）。
	Requires string
	// Steps 步骤正文（markdown，渲染进分支清单）。
	Steps string
}

// Manager 工作流集合：Load 后只读、无状态，无需锁（-race 干净）。
type Manager struct {
	workflows map[string]Workflow
	order     []string // 稳定顺序（按 name 排序），List/RenderBranch 用
}

// Load 从编译期内嵌目录加载全部工作流并解析 frontmatter。
// 格式错误（坏 frontmatter/空正文/name 冲突）= 开发错误，返回 error，由组合根
// fail fast——workflow 是编译期资产，问题应在开发期暴露（对齐 persona.Load）。
func Load() (*Manager, error) {
	return load(files)
}

// load 解析给定 fs.FS 下的 workflows/*.md，独立出来便于测试注入虚拟文件系统。
func load(fsys fs.FS) (*Manager, error) {
	entries, err := fs.ReadDir(fsys, "workflows")
	if err != nil {
		return nil, fmt.Errorf("workflow: read dir: %w", err)
	}
	m := &Manager{workflows: make(map[string]Workflow, len(entries))}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		data, err := fs.ReadFile(fsys, "workflows/"+e.Name())
		if err != nil {
			return nil, fmt.Errorf("workflow: read %q: %w", e.Name(), err)
		}
		w, err := parseWorkflow(e.Name(), data)
		if err != nil {
			return nil, fmt.Errorf("workflow: %q: %w", e.Name(), err)
		}
		if _, dup := m.workflows[w.Name]; dup {
			return nil, fmt.Errorf("workflow: duplicate name %q", w.Name)
		}
		m.workflows[w.Name] = w
		m.order = append(m.order, w.Name)
	}
	sort.Strings(m.order)
	return m, nil
}

// parseWorkflow 解析单个工作流文件：frontmatter（yaml.v3）+ 正文（步骤）。
// frontmatter 以 --- 起始行开头、以第二个 --- 行结束；缺 name 取文件名 stem。
func parseWorkflow(fileName string, data []byte) (Workflow, error) {
	s := strings.TrimSpace(string(data))
	var meta struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
		Trigger     string `yaml:"trigger"`
		Input       string `yaml:"input"`
		Output      string `yaml:"output"`
		Stop        string `yaml:"stop"`
		Requires    string `yaml:"requires"`
	}
	body := s
	if strings.HasPrefix(s, "---") {
		rest := s[3:]
		idx := strings.Index(rest, "\n---")
		if idx < 0 {
			return Workflow{}, errors.New("malformed frontmatter: missing closing ---")
		}
		head := rest[:idx]
		body = strings.TrimSpace(rest[idx+4:])
		if err := yaml.Unmarshal([]byte(head), &meta); err != nil {
			return Workflow{}, fmt.Errorf("malformed frontmatter: %w", err)
		}
	}
	name := meta.Name
	if name == "" {
		name = strings.TrimSuffix(fileName, ".md")
	}
	if name == "" {
		return Workflow{}, errors.New("empty workflow name")
	}
	if strings.TrimSpace(body) == "" {
		return Workflow{}, errors.New("empty workflow body")
	}
	return Workflow{
		Name:        name,
		Description: meta.Description,
		Trigger:     meta.Trigger,
		Input:       meta.Input,
		Output:      meta.Output,
		Stop:        meta.Stop,
		Requires:    meta.Requires,
		Steps:       body,
	}, nil
}

// Get 按名查找；未命中返回业务错误并附可用列表（提示拼写错误）。
func (m *Manager) Get(name string) (Workflow, error) {
	if w, ok := m.workflows[name]; ok {
		return w, nil
	}
	return Workflow{}, fmt.Errorf("workflow %q not found (available: %s)", name, strings.Join(m.order, ", "))
}

// List 返回全部工作流，按 name 稳定排序（重复调用结果确定）。
func (m *Manager) List() []Workflow {
	out := make([]Workflow, 0, len(m.order))
	for _, n := range m.order {
		out = append(out, m.workflows[n])
	}
	return out
}

// branchIntro 分支段引言：说明进入/收尾的声明纪律（模型据此在首句声明进入、产出后汇报）。
const branchIntro = "可用工作流分支（用户任务命中触发条件时，第一句回复以 [进入分支:分支名] 开头声明进入该分支（如 [进入分支:pdf]），严格按步骤执行；产出后向用户汇报并等待确认，汇报须包含 [分支完成:分支名]）："

// RenderBranch 渲染"可用工作流分支"提示词段，等价于"全部能力可用"（零回归，见 RenderBranchFor）。
func (m *Manager) RenderBranch() string { return m.RenderBranchFor(nil) }

// RenderBranchFor 渲染"可用工作流分支"提示词段（注入 base 契约层）：
// 固定格式 = 引言 + 每个分支的 名称/触发/输入/输出/停止/步骤。
// available 为 nil 表示不做能力裁剪；否则 Requires 非空且能力不满足的分支整段不注入——
// workflow 是编译期资产、能力是运行期事实，两者由组合根在这里对齐（能力名见 CapK8s，
// 见 Zoo/model/k8s-diagnosis.md §16.5）。全部被裁掉时返回空串（组合根不追加空段）。
func (m *Manager) RenderBranchFor(available map[string]bool) string {
	var items strings.Builder
	for _, n := range m.order {
		w := m.workflows[n]
		// available 为 nil = 不裁剪（nil map 查找恒为 false，故必须显式判 nil）。
		if w.Requires != "" && available != nil && !available[w.Requires] {
			continue
		}
		fmt.Fprintf(&items, "\n[%s] %s", w.Name, w.Description)
		if w.Trigger != "" {
			fmt.Fprintf(&items, "\n- 触发：%s", w.Trigger)
		}
		if w.Input != "" {
			fmt.Fprintf(&items, "\n- 输入：%s", w.Input)
		}
		if w.Output != "" {
			fmt.Fprintf(&items, "\n- 输出：%s", w.Output)
		}
		if w.Stop != "" {
			fmt.Fprintf(&items, "\n- 停止：%s", w.Stop)
		}
		fmt.Fprintf(&items, "\n- 步骤：\n%s", w.Steps)
	}
	if items.Len() == 0 {
		return ""
	}
	return branchIntro + items.String()
}
