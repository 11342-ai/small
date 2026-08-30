package builtin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"small/internal/kb"
	"small/internal/tool"
)

// writeKbFile 建目录并写知识库文件（测试辅助，mtime 间隔保证惰性刷新感知）。
func writeKbFile(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
	time.Sleep(2 * time.Millisecond)
}

// execTool 直接执行工具（构造依赖进 run 闭包，脱离 agent 循环做集成验证）。
func execTool(t *testing.T, tk tool.Tool, args string) tool.Result {
	t.Helper()
	res, err := tk.Execute(context.Background(), json.RawMessage(args))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	return res
}

func newTestKb(t *testing.T, files map[string]string) *kb.Store {
	t.Helper()
	root := t.TempDir()
	for rel, content := range files {
		writeKbFile(t, root, rel, content)
	}
	store, err := kb.New(root)
	if err != nil {
		t.Fatalf("kb.New: %v", err)
	}
	return store
}

func TestKbTree(t *testing.T) {
	store := newTestKb(t, map[string]string{
		"Java/事务.md": `---
domains: [java, 数据一致性]
---
# 事务
## 原理
Spring 事务基于 AOP。
`,
		"数据/一致性.md": `---
domains: [mysql, kafka, redis, 数据一致性]
---
# 数据一致性
## 定义
缓存与数据库的一致问题。
## 失效
无过期时间时延迟双删失效。
`,
	})
	tk := KbTree(store)
	// 顶层全览：含文件根与段落、深度标注与域。
	res := execTool(t, tk, `{}`)
	if res.IsError {
		t.Fatalf("kb_tree 顶层查询不应失败: %+v", res)
	}
	for _, want := range []string{
		"Java/事务.md (L3) [java, 数据一致性]",
		"Java/事务.md#原理 (L3)",
		"数据/一致性.md (L4) [mysql, kafka, redis, 数据一致性]",
		"数据/一致性.md#失效 (L4)",
	} {
		if !strings.Contains(res.Data, want) {
			t.Errorf("kb_tree 输出缺 %q，实际:\n%s", want, res.Data)
		}
	}
	// 域过滤：只看 redis 域。
	res = execTool(t, tk, `{"domain":"redis"}`)
	if !strings.Contains(res.Data, "数据/一致性.md") || strings.Contains(res.Data, "Java/事务.md") {
		t.Errorf("domain 过滤失效，实际:\n%s", res.Data)
	}
	// 起点下钻。
	res = execTool(t, tk, `{"ref":"数据/一致性.md"}`)
	if strings.Contains(res.Data, "Java/事务.md") || !strings.Contains(res.Data, "数据/一致性.md#失效") {
		t.Errorf("ref 下钻失效，实际:\n%s", res.Data)
	}
	// 不存在的 ref：业务失败回灌。
	res = execTool(t, tk, `{"ref":"不存在.md"}`)
	if !res.IsError || !strings.Contains(res.Data, "节点不存在") {
		t.Errorf("应报节点不存在，实际 %+v", res)
	}
}

func TestKbRefs(t *testing.T) {
	store := newTestKb(t, map[string]string{
		"Java/事务.md": `# 事务
## 边界
自调用会失效，参考[一致性](../数据/一致性.md#定义)。
`,
		"数据/一致性.md": `# 数据一致性
## 定义
缓存与数据库的一致问题。
`,
	})
	tk := KbRefs(store)
	res := execTool(t, tk, `{"ref":"数据/一致性.md#定义"}`)
	if res.IsError {
		t.Fatalf("kb_refs 不应失败: %+v", res)
	}
	if !strings.Contains(res.Data, "Java/事务.md#边界") {
		t.Errorf("反链应含 Java/事务.md#边界，实际:\n%s", res.Data)
	}
	// 必填校验。
	res = execTool(t, tk, `{}`)
	if !res.IsError || !strings.Contains(res.Data, "ref 必填") {
		t.Errorf("应报 ref 必填，实际 %+v", res)
	}
}

func TestKbCheck(t *testing.T) {
	store := newTestKb(t, map[string]string{
		"x.md":     "# X\n见[丢失](../不存在.md#标题)\n",
		"clean.md": "# Clean\n## 原理\na\n",
	})
	tk := KbCheck(store)
	res := execTool(t, tk, `{}`)
	if res.IsError {
		t.Fatalf("kb_check 不应失败: %+v", res)
	}
	if !strings.Contains(res.Data, "broken_link") {
		t.Errorf("应报断链，实际:\n%s", res.Data)
	}
}

func TestKbWrite(t *testing.T) {
	root := t.TempDir()
	store, err := kb.New(root)
	if err != nil {
		t.Fatalf("kb.New: %v", err)
	}
	tk := KbWrite(store)
	res := execTool(t, tk, `{"topic":"Java为什么跨平台","content":"## 原理\nJVM 将字节码翻译为平台机器码。\n## 失效\n无 JVM 的平台无法运行。","domains":["java"],"file":"Java/跨平台"}`)
	if res.IsError {
		t.Fatalf("kb_write 不应失败: %+v", res)
	}
	if !strings.Contains(res.Data, "Java/跨平台.md") {
		t.Errorf("应返回写入路径，实际 %q", res.Data)
	}
	n, ok := store.Node("Java/跨平台.md")
	if !ok {
		t.Fatalf("写入后节点不可查")
	}
	if n.Level != 4 || len(n.Domains) != 1 || n.Domains[0] != "java" {
		t.Fatalf("写入后节点元数据不符：%+v", n)
	}
	// 必填校验。
	res = execTool(t, tk, `{"content":"x"}`)
	if !res.IsError {
		t.Errorf("缺 topic 应报参数错误，实际 %+v", res)
	}
}

func TestKbTreeEmpty(t *testing.T) {
	store, err := kb.New(t.TempDir())
	if err != nil {
		t.Fatalf("kb.New: %v", err)
	}
	res := execTool(t, KbTree(store), `{}`)
	if !res.IsError || !strings.Contains(res.Data, "知识库为空") {
		t.Errorf("空知识库应报业务失败，实际 %+v", res)
	}
}
