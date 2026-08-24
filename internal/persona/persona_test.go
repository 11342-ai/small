package persona

import (
	"reflect"
	"sort"
	"strings"
	"testing"
	"testing/fstest"
)

func TestParsePersona(t *testing.T) {
	p, err := parsePersona("catton.md", []byte("---\nname: catton\ndescription: 二次元\n---\n\n你是猫瞳。\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := Persona{Name: "catton", Description: "二次元", SystemPrompt: "你是猫瞳。"}
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
