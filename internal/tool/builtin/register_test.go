package builtin

import (
	"testing"

	"small/internal/tool"
)

func TestRegisterBuiltins(t *testing.T) {
	reg := tool.New()
	if err := RegisterBuiltins(reg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := reg.Get("echo"); !ok {
		t.Error("echo should be registered")
	}
}
