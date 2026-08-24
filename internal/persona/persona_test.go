package persona

import (
	"reflect"
	"sort"
	"strings"
	"testing"
	"testing/fstest"
)

func TestParsePersona(t *testing.T) {
	p, err := parsePersona("catton.md", []byte("---\nname: catton\ndescription: 二次元\nfirst_message: 喵~你好\n"+
		"examples:\n  - user: 你是谁？\n    assistant: 我是猫瞳喵~\n---\n\n你是猫瞳。\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := Persona{
		Name:         "catton",
		Description:  "二次元",
		SystemPrompt: "你是猫瞳。",
		FirstMessage: "喵~你好",
		Examples:     []Example{{User: "你是谁？", Assistant: "我是猫瞳喵~"}},
	}
	if !reflect.DeepEqual(p, want) {
		t.Errorf("parsed = %+v, want %+v", p, want)
	}
}

func TestParsePersonaNameDefaultsToFileStem(t *testing.T) {
	p, err := parsePersona("scientist.md", []byte("---\ndescription: 严谨\n---\n\n你是科学家。"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if p.Name != "scientist" {
		t.Errorf("name = %q, want file stem", p.Name)
	}
}

func TestParsePersonaNoFrontmatter(t *testing.T) {
	p, err := parsePersona("onee.md", []byte("你是御姐。"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if p.Name != "onee" || p.SystemPrompt != "你是御姐。" {
		t.Errorf("parsed = %+v", p)
	}
}

func TestParsePersonaErrors(t *testing.T) {
	cases := []struct {
		name string
		data string
	}{
		{"missing closing ---", "---\nname: x\n正文"},
		{"empty body", "---\nname: x\n---\n\n  \n"},
	}
	for _, c := range cases {
		if _, err := parsePersona("x.md", []byte(c.data)); err == nil {
			t.Errorf("%s: want error", c.name)
		}
	}
}

func TestLoadRejectsDuplicateName(t *testing.T) {
	fsys := fstest.MapFS{
		"personas/default.md": &fstest.MapFile{Data: []byte("---\n---\n\ndefault body")},
		"personas/a.md":       &fstest.MapFile{Data: []byte("---\nname: dup\n---\n\na body")},
		"personas/b.md":       &fstest.MapFile{Data: []byte("---\nname: dup\n---\n\nb body")},
	}
	if _, err := load(fsys); err == nil {
		t.Fatal("duplicate name: want error")
	}
}

func TestLoadRequiresDefault(t *testing.T) {
	fsys := fstest.MapFS{
		"personas/a.md": &fstest.MapFile{Data: []byte("---\n---\n\na body")},
	}
	if _, err := load(fsys); err == nil {
		t.Fatal("missing default: want error")
	}
}

func TestLoadEmbeddedAndQuery(t *testing.T) {
	m, err := Load()
	if err != nil {
		t.Fatalf("Load embedded: %v", err)
	}
	// 默认人格必在且正文非空（Load 已校验，这里锁行为）。
	if d := m.Default(); d.Name != "default" || strings.TrimSpace(d.SystemPrompt) == "" {
		t.Errorf("default = %+v", d)
	}
	// 全部人格正文不超长度守门（budget 守门，见设计文档 §4）。
	for _, p := range m.List() {
		if runes := len([]rune(p.SystemPrompt)); runes > maxPromptRunes {
			t.Errorf("%s: body %d runes > %d", p.Name, runes, maxPromptRunes)
		}
	}
	// List 按 name 稳定排序。
	names := make([]string, 0, len(m.order))
	for _, p := range m.List() {
		names = append(names, p.Name)
	}
	if !sort.StringsAreSorted(names) {
		t.Errorf("List not sorted: %v", names)
	}
	if !reflect.DeepEqual(names, m.order) {
		t.Errorf("List order %v != internal order %v", names, m.order)
	}
	// Get 命中 / 未命中（未命中附可用列表提示）。
	if _, err := m.Get("catton"); err != nil {
		t.Errorf("Get catton: %v", err)
	}
	if _, err := m.Get("nope"); err == nil {
		t.Error("Get nope: want error")
	} else if !strings.Contains(err.Error(), "catton") {
		t.Errorf("Get error should list available personas: %v", err)
	}
}

func TestCompose(t *testing.T) {
	base := "契约层：工具说明"
	p := Persona{
		SystemPrompt: "你是猫瞳。",
		FirstMessage: "喵~你好呀！",
		Examples:     []Example{{User: "你是谁？", Assistant: "我是猫瞳喵~"}, {User: "帮我干活", Assistant: "好嘞~交给猫瞳！"}},
	}
	got := Compose(base, p, "")
	// 三段顺序固定：契约 → 人格正文 → 开场白 → few-shot → （无记忆时不输出记忆块）。
	want := "契约层：工具说明\n\n你是猫瞳。\n\n喵~你好呀！\n\n用户：你是谁？\n你：我是猫瞳喵~\n\n用户：帮我干活\n你：好嘞~交给猫瞳！"
	if got != want {
		t.Errorf("Compose = %q, want %q", got, want)
	}
}

func TestComposeZeroRegression(t *testing.T) {
	// 零回归锁：无新字段 + 记忆注入时，输出与改版前 main.go 的手拼逐字节一致。
	base := "契约层"
	p := Persona{SystemPrompt: "人格正文"}
	mem := "<memory>\n常驻记忆\n</memory>"
	old := base + "\n\n" + p.SystemPrompt + "\n\n" + mem
	if got := Compose(base, p, mem); got != old {
		t.Errorf("Compose with fields empty = %q, want legacy %q", got, old)
	}
}

func TestComposeSkipsEmptySegments(t *testing.T) {
	// 空 base / 空 memory 不产生多余空行或残留标签。
	if got := Compose("", Persona{SystemPrompt: "正文"}, ""); got != "正文" {
		t.Errorf("Compose minimal = %q, want 正文", got)
	}
	if got := Compose("契约", Persona{SystemPrompt: "正文"}, ""); got != "契约\n\n正文" {
		t.Errorf("Compose no memory = %q", got)
	}
}
