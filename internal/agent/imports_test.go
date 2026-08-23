package agent

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestProviderImportIsolatedToAdapter 固化"隔离点"纪律（踩坑 #9）：
// agent 包内只有 adapter.go 允许 import provider。provider 不 import agent，
// 构不成循环依赖，编译器不会拦截——只能用测试把软纪律变成合并门槛的一部分，
// 未来新增 import provider 的文件时本测试立即变红。
func TestProviderImportIsolatedToAdapter(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	fset := token.NewFileSet()
	var violators []string
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue // 测试文件可自由 import provider（不参与生产依赖图）
		}
		f, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, imp := range f.Imports {
			if strings.Trim(imp.Path.Value, `"`) == "small/internal/provider" {
				violators = append(violators, name)
			}
		}
	}
	if want := []string{"adapter.go"}; !reflect.DeepEqual(violators, want) {
		t.Errorf("import provider 的文件 = %v，隔离点要求只允许 %v", violators, want)
	}
}
