package builtin

import (
	"testing"

	"small/internal/memory"
	"small/internal/tool"
)

func TestRegisterBuiltins(t *testing.T) {
	reg := tool.New()
	if err := RegisterBuiltins(reg, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := reg.Get("echo"); !ok {
		t.Error("echo should be registered")
	}
	// mem 为 nil：不注册记忆工具（退化）。
	if _, ok := reg.Get("memory_search"); ok {
		t.Error("memory_search should not be registered without memory store")
	}
}

func TestRegisterBuiltins_WithMemory(t *testing.T) {
	mem, err := memory.New(t.TempDir())
	if err != nil {
		t.Fatalf("memory.New: %v", err)
	}
	reg := tool.New()
	if err := RegisterBuiltins(reg, mem); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, name := range []string{"echo", "get_current_time", "memory_search", "memory_get", "memory_save"} {
		if _, ok := reg.Get(name); !ok {
			t.Errorf("%s should be registered", name)
		}
	}
}
