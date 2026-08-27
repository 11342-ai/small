package command

import (
	"context"
	"strings"
	"testing"

	"small/internal/policy"
)

func strCmd(name string, run func(args []string) (string, error)) CommandSpec {
	if run == nil {
		run = func([]string) (string, error) { return "ok", nil }
	}
	return CommandSpec{
		Name: name,
		Run: func(_ context.Context, args []string) (string, error) {
			return run(args)
		},
	}
}

func TestRegister_Validation(t *testing.T) {
	r := New()
	cases := []struct {
		name string
		spec CommandSpec
	}{
		{"空名", CommandSpec{Name: "", Run: strCmd("", nil).Run}},
		{"非斜杠前缀", CommandSpec{Name: "exit", Run: strCmd("exit", nil).Run}},
		{"nil Run", CommandSpec{Name: "/x", Run: nil}},
	}
	for _, c := range cases {
		if err := r.Register(c.spec); err == nil {
			t.Errorf("%s: Register 应报错", c.name)
		}
	}
}

func TestRegister_Duplicate(t *testing.T) {
	r := New()
	if err := r.Register(strCmd("/dup", nil)); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := r.Register(strCmd("/dup", nil)); err == nil {
		t.Fatal("重复注册应报错")
	}
}

func TestDispatch_NonCommand(t *testing.T) {
	r := New()
	handled, out, err := r.Dispatch(context.Background(), "hello")
	if handled || out != "" || err != nil {
		t.Fatalf("非命令输入应返回 handled=false：got handled=%v out=%q err=%v", handled, out, err)
	}
}

func TestDispatch_UnknownCommand(t *testing.T) {
	r := New()
	_ = r.Register(strCmd("/help", nil))
	handled, _, err := r.Dispatch(context.Background(), "/nope")
	if !handled {
		t.Fatal("未知命令应算已处理（不再走对话）")
	}
	if err == nil || !strings.Contains(err.Error(), "未知命令") {
		t.Fatalf("未知命令应报错并附提示，got err=%v", err)
	}
}

func TestDispatch_Args(t *testing.T) {
	r := New()
	var got []string
	_ = r.Register(strCmd("/echo", func(args []string) (string, error) {
		got = args
		return "ok", nil
	}))
	if _, _, err := r.Dispatch(context.Background(), "/echo a b c"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(got) != 3 || got[0] != "a" || got[2] != "c" {
		t.Fatalf("args = %v, want [a b c]", got)
	}
}

func TestDispatch_AskDenied(t *testing.T) {
	r := New()
	ran := false
	spec := strCmd("/clear", nil)
	spec.Perm = policy.Ask
	spec.Run = func(_ context.Context, _ []string) (string, error) {
		ran = true
		return "", nil
	}
	_ = r.Register(spec)
	r.Confirm = func(string) bool { return false } // 用户拒绝

	handled, out, err := r.Dispatch(context.Background(), "/clear")
	if !handled || out != "" || err != nil {
		t.Fatalf("拒绝应视为已处理且无错误：got handled=%v out=%q err=%v", handled, out, err)
	}
	if ran {
		t.Fatal("Ask 拒绝后不应执行命令")
	}
}

func TestDispatch_AskConfirmed(t *testing.T) {
	r := New()
	ran := false
	spec := strCmd("/clear", nil)
	spec.Perm = policy.Ask
	spec.Run = func(_ context.Context, _ []string) (string, error) {
		ran = true
		return "cleared", nil
	}
	_ = r.Register(spec)
	r.Confirm = func(string) bool { return true }

	_, out, err := r.Dispatch(context.Background(), "/clear")
	if err != nil || out != "cleared" {
		t.Fatalf("确认后应执行：out=%q err=%v", out, err)
	}
	if !ran {
		t.Fatal("确认后应执行命令")
	}
}

func TestDispatch_AskFailClosed(t *testing.T) {
	// Confirm 为 nil 时 Ask 命令应拒绝（fail-closed），不执行。
	r := New()
	ran := false
	spec := strCmd("/clear", nil)
	spec.Perm = policy.Ask
	spec.Run = func(_ context.Context, _ []string) (string, error) {
		ran = true
		return "", nil
	}
	_ = r.Register(spec)

	if _, _, err := r.Dispatch(context.Background(), "/clear"); err != nil {
		t.Fatalf("拒绝不应报错，got %v", err)
	}
	if ran {
		t.Fatal("Confirm 为 nil 时 Ask 命令不应执行（fail-closed）")
	}
}

func TestList_Sorted(t *testing.T) {
	r := New()
	_ = r.Register(strCmd("/zeta", nil))
	_ = r.Register(strCmd("/alpha", nil))
	list := r.List()
	if len(list) != 2 || list[0].Name != "/alpha" || list[1].Name != "/zeta" {
		t.Fatalf("List 应按名升序，got %v", list)
	}
}

func TestHelpText(t *testing.T) {
	r := New()
	_ = r.Register(strCmd("/clear", nil))
	if !strings.Contains(r.HelpText(), "/clear") {
		t.Fatalf("HelpText 应包含已注册命令，got %q", r.HelpText())
	}
}
