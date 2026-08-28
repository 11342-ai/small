package builtin

import (
	"testing"

	"small/internal/memory"
	"small/internal/tool"
)

func TestRegisterBuiltins(t *testing.T) {
	reg := tool.New()
	if err := RegisterBuiltins(reg, Deps{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := reg.Get("echo"); !ok {
		t.Error("echo should be registered")
	}
	if _, ok := reg.Get("plan"); !ok {
		t.Error("plan should be registered (无构造依赖)")
	}
	if _, ok := reg.Get("web_fetch"); !ok {
		t.Error("web_fetch should be registered (无构造依赖)")
	}
	// mem/exec/file 均为 nil：记忆/exec/文件工具不注册（退化）。
	for _, name := range []string{"memory_search", "exec", "file_read", "file_list", "doc_search"} {
		if _, ok := reg.Get(name); ok {
			t.Errorf("%s should not be registered without deps", name)
		}
	}
}

func TestRegisterBuiltins_WithMemory(t *testing.T) {
	mem, err := memory.New(t.TempDir())
	if err != nil {
		t.Fatalf("memory.New: %v", err)
	}
	reg := tool.New()
	if err := RegisterBuiltins(reg, Deps{Mem: mem}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, name := range []string{"echo", "get_current_time", "plan", "memory_search", "memory_get", "memory_save"} {
		if _, ok := reg.Get(name); !ok {
			t.Errorf("%s should be registered", name)
		}
	}
	if _, ok := reg.Get("exec"); ok {
		t.Error("exec should not be registered without Exec config")
	}
}

func TestRegisterBuiltins_WithExec(t *testing.T) {
	reg := tool.New()
	if err := RegisterBuiltins(reg, Deps{Exec: &ExecConfig{Allow: []string{"echo"}}}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := reg.Get("exec"); !ok {
		t.Error("exec should be registered with Exec config")
	}
}

func TestRegisterBuiltins_WithFile(t *testing.T) {
	reg := tool.New()
	if err := RegisterBuiltins(reg, Deps{File: &FileConfig{Root: t.TempDir()}}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, name := range []string{"file_read", "file_list", "doc_search"} {
		if _, ok := reg.Get(name); !ok {
			t.Errorf("%s should be registered with File config", name)
		}
	}
}
