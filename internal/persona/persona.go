// Package persona 提供对话人格（风格）的编译期内置来源：把"人格/风格"从硬编码
// 的 system prompt 中拆出，变成可命名的模板，供组合根在创建会话时挑选并注入。
//
// 设计要点（见 Zoo/model/persona.md）：
//   - 一个对话 = 一个人格：persona 在会话创建时定型，存续期内不切换。
//   - 人格是代码资产（开发者维护）→ go:embed 编译期快照；对照 memory 的运行时
//     读用户可编辑 Markdown 文件，两者内容来源性质不同。
//   - 本包是叶子：不 import 任何内部包；agent 不感知 persona（system 是字符串）。
package persona

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

//go:embed personas/*.md
var files embed.FS

// maxPromptRunes 人格正文建议上限（字符数）：system 在预算截断中保留，人格加长
// 会挤占历史预算（对齐 memory 的 2KB 上限思路，见设计文档 §4）。
const maxPromptRunes = 1500

// Persona 一个人格：Name 是 --persona 的取值，SystemPrompt 是注入 system 的正文。
type Persona struct {
	// Name 人格名（frontmatter 缺省取文件名 stem）。
	Name string
	// Description 一句话说明，供错误提示/帮助展示。
	Description string
	// SystemPrompt 人格正文（frontmatter 之后的正文），组合根拼进 system。
	SystemPrompt string
}

// Manager 人格集合：Load 后只读、无状态，无需锁（-race 干净）。
type Manager struct {
	personas map[string]Persona
	order    []string // 稳定顺序（按 name 排序），List/错误提示用
}

// Load 从编译期内嵌目录加载全部人格并解析 frontmatter。
// 格式错误（坏 frontmatter/空正文/name 冲突/缺 default.md）= 开发错误，返回 error，
// 由组合根 fail fast——人格文件是编译期资产，问题应在开发期暴露。
func Load() (*Manager, error) {
	return load(files)
}

// load 解析给定 fs.FS 下的 personas/*.md，独立出来便于测试注入虚拟文件系统
// （fstest.MapFS 覆盖 embed 无法构造的 name 冲突/缺 default 等错误场景）。
func load(fsys fs.FS) (*Manager, error) {
	entries, err := fs.ReadDir(fsys, "personas")
	if err != nil {
		return nil, fmt.Errorf("persona: read dir: %w", err)
	}
	m := &Manager{personas: make(map[string]Persona, len(entries))}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		data, err := fs.ReadFile(fsys, "personas/"+e.Name())
		if err != nil {
			return nil, fmt.Errorf("persona: read %q: %w", e.Name(), err)
		}
		p, err := parsePersona(e.Name(), data)
		if err != nil {
			return nil, fmt.Errorf("persona: %q: %w", e.Name(), err)
		}
		if _, dup := m.personas[p.Name]; dup {
			return nil, fmt.Errorf("persona: duplicate name %q", p.Name)
		}
		m.personas[p.Name] = p
		m.order = append(m.order, p.Name)
	}
	if _, ok := m.personas["default"]; !ok {
		return nil, errors.New("persona: default persona missing (need personas/default.md)")
	}
	sort.Strings(m.order)
	return m, nil
}

// parsePersona 解析单个人格文件：frontmatter（yaml.v3）+ 正文。
// frontmatter 以 --- 起始行开头、以第二个 --- 行结束；缺 name 取文件名 stem。
func parsePersona(fileName string, data []byte) (Persona, error) {
	s := strings.TrimSpace(string(data))
	var meta struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
	}
	body := s
	if strings.HasPrefix(s, "---") {
		rest := s[3:]
		idx := strings.Index(rest, "\n---")
		if idx < 0 {
			return Persona{}, errors.New("malformed frontmatter: missing closing ---")
		}
		head := rest[:idx]
		body = strings.TrimSpace(rest[idx+4:])
		if err := yaml.Unmarshal([]byte(head), &meta); err != nil {
			return Persona{}, fmt.Errorf("malformed frontmatter: %w", err)
		}
	}
	name := meta.Name
	if name == "" {
		name = strings.TrimSuffix(fileName, ".md")
	}
	if name == "" {
		return Persona{}, errors.New("empty persona name")
	}
	if strings.TrimSpace(body) == "" {
		return Persona{}, errors.New("empty system prompt body")
	}
	return Persona{Name: name, Description: meta.Description, SystemPrompt: body}, nil
}

// Get 按名查找；未命中返回业务错误并附可用列表（提示拼写错误）。
func (m *Manager) Get(name string) (Persona, error) {
	if p, ok := m.personas[name]; ok {
		return p, nil
	}
	return Persona{}, fmt.Errorf("persona %q not found (available: %s)", name, strings.Join(m.order, ", "))
}

// List 返回全部人格，按 name 稳定排序（重复调用结果确定）。
func (m *Manager) List() []Persona {
	out := make([]Persona, 0, len(m.order))
	for _, n := range m.order {
		out = append(out, m.personas[n])
	}
	return out
}

// Default 返回默认人格（default.md，Load 已保证存在）。
func (m *Manager) Default() Persona {
	return m.personas["default"]
}
