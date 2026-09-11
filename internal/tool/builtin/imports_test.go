package builtin

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// TestProductionImportsStayWithinLeaves 固化 builtin 的依赖纪律：生产代码只允许 import
// stdlib 与叶子部件（tool/policy/memory/kb/k8s）；不得反向依赖 agent/session/provider/config——
// 否则会破坏依赖单向（main → agent → tool）与隔离点。
// 与 agent/imports_test.go 同款思路：构不成循环依赖时编译器不会拦截，
// 只能用测试把软纪律变成合并门槛的一部分；测试文件不参与生产依赖图，
// 可自由 import（本包 memory_test.go 的端到端测试就 import agent 做集成验证）。
func TestProductionImportsStayWithinLeaves(t *testing.T) {
	forbidden := []string{
		"small/internal/agent",
		"small/internal/session",
		"small/internal/provider",
		"small/internal/config",
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	fset := token.NewFileSet()
	var violators []string
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue // 测试文件可自由 import（不参与生产依赖图）
		}
		f, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, imp := range f.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			for _, banned := range forbidden {
				if path == banned {
					violators = append(violators, name)
				}
			}
		}
	}
	if len(violators) > 0 {
		t.Errorf("builtin 生产代码反向依赖 %v，违反依赖单向纪律", violators)
	}
}
