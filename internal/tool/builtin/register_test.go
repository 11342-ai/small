package builtin

import (
	"testing"

	"small/internal/kb"
	"small/internal/memory"
	"small/internal/policy"
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
	// mem/exec/file/kb/cache 均为 nil：记忆/exec/文件/知识库/文档解析工具不注册（退化）。
	for _, name := range []string{"memory_search", "exec", "file_read", "file_list", "doc_search", "kb_tree", "kb_refs", "kb_check", "kb_write", "doc_parse", "doc_read", "doc_clean"} {
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

func TestRegisterBuiltins_WithKB(t *testing.T) {
	store, err := kb.New(t.TempDir())
	if err != nil {
		t.Fatalf("kb.New: %v", err)
	}
	reg := tool.New()
	if err := RegisterBuiltins(reg, Deps{Kb: store}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, name := range []string{"kb_tree", "kb_refs", "kb_check", "kb_write"} {
		if _, ok := reg.Get(name); !ok {
			t.Errorf("%s should be registered with Kb store", name)
		}
	}
	if _, ok := reg.Get("memory_search"); ok {
		t.Error("memory_search should not be registered without Mem")
	}
}

func TestRegisterBuiltins_WithFile(t *testing.T) {
	reg := tool.New()
	if err := RegisterBuiltins(reg, Deps{File: &FileConfig{Root: t.TempDir()}}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, name := range []string{"file_read", "file_list", "doc_search", "file_tree", "propose_file_write", "propose_file_edit", "file_edit", "file_write", "file_diff"} {
		if _, ok := reg.Get(name); !ok {
			t.Errorf("%s should be registered with File config", name)
		}
	}
}

func TestRegisterBuiltins_WithCache(t *testing.T) {
	reg := tool.New()
	if err := RegisterBuiltins(reg, Deps{Cache: &LitConfig{Root: t.TempDir()}}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, name := range []string{"doc_parse", "doc_read", "doc_clean"} {
		if _, ok := reg.Get(name); !ok {
			t.Errorf("%s should be registered with Cache config", name)
		}
	}
	if _, ok := reg.Get("file_read"); ok {
		t.Error("file_read should not be registered without File")
	}
}

func TestRegisterBuiltins_PermissionTableComplete(t *testing.T) {
	// 注册全部内置工具后：权限表必须全量覆盖（register 内校验，防新增工具漏登记裸奔）。
	reg := tool.New()
	deps := Deps{File: &FileConfig{Root: t.TempDir()}, Exec: &ExecConfig{Allow: []string{"echo"}}, Cache: &LitConfig{Root: t.TempDir()}}
	if err := RegisterBuiltins(reg, deps); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, name := range []string{"echo", "plan", "get_current_time", "web_fetch", "file_read",
		"file_list", "file_tree", "doc_search", "propose_file_write", "propose_file_edit",
		"memory_search", "memory_get", "memory_save", "exec", "file_write", "file_edit",
		"kb_tree", "kb_refs", "kb_check", "kb_write", "doc_parse", "doc_read", "doc_clean",
		"file_diff"} {
		if ToolPermissions[name] == "" {
			t.Errorf("%s 未登记权限", name)
		}
	}
	// Ask 语义：写/执行类工具必须 Ask（不是 Pass）。
	for _, name := range []string{"exec", "file_write", "file_edit", "kb_write"} {
		if ToolPermissions[name] != policy.Ask {
			t.Errorf("%s 应为 Ask，got %s", name, ToolPermissions[name])
		}
	}
	// Pass 语义：文档解析工具（只读 + 写受控缓存目录）与只读比对工具不得误登记为 Ask。
	for _, name := range []string{"doc_parse", "doc_read", "doc_clean", "file_diff"} {
		if ToolPermissions[name] != policy.Pass {
			t.Errorf("%s 应为 Pass，got %s", name, ToolPermissions[name])
		}
	}
}
