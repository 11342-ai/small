package tool

import (
	"errors"
	"fmt"
	"sort"
)

// Registry 是 tool 模块的"模块对象"：名称 → 工具的映射容器。
// 组合根一次性注册、运行期只读，故不加并发锁——约定注册只发生在启动期；
// 若未来出现动态注册（如运行时接入 MCP 工具），再补锁。
type Registry struct {
	tools map[string]Tool
}

// New 构造空注册表，用法与 provider.New / agent.New 的构造风格一致。
func New() *Registry {
	return &Registry{tools: make(map[string]Tool)}
}

// Register 注册工具。注册是装配动作（组合根调用），重名、空名、nil 均报错。
func (r *Registry) Register(t Tool) error {
	if t == nil {
		return errors.New("tool: register nil tool")
	}
	name := t.Spec().Name
	if name == "" {
		return errors.New("tool: register tool with empty name")
	}
	if _, dup := r.tools[name]; dup {
		return fmt.Errorf("tool: duplicate tool name %q", name)
	}
	r.tools[name] = t
	return nil
}

// Get 按名查找工具，未命中返回 false（comma-ok 惯例）。
func (r *Registry) Get(name string) (Tool, bool) {
	t, ok := r.tools[name]
	return t, ok
}

// List 返回全部工具声明，按名称升序保证顺序稳定（供序列化成 tools 数组，
// 且重复调用结果确定，便于测试与复现）。
func (r *Registry) List() []Spec {
	specs := make([]Spec, 0, len(r.tools))
	for _, t := range r.tools {
		specs = append(specs, t.Spec())
	}
	sort.Slice(specs, func(i, j int) bool { return specs[i].Name < specs[j].Name })
	return specs
}
