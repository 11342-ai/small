package tool

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
)

// fakeTool 构造一个可注入任意 Spec 与执行行为的测试桩工具。
func fakeTool(name string) *Func {
	return NewFunc(
		Spec{Name: name, Description: "desc-" + name, Parameters: json.RawMessage(`{}`)},
		func(_ context.Context, args json.RawMessage) (Result, error) {
			var in struct{ V string }
			if err := json.Unmarshal(args, &in); err != nil {
				return Result{Data: "bad args", IsError: true}, nil
			}
			return Result{Data: name + ":" + in.V}, nil
		},
	)
}

func TestRegistry_RegisterGet(t *testing.T) {
	reg := New()
	if err := reg.Register(fakeTool("echo")); err != nil {
		t.Fatalf("register: %v", err)
	}

	got, ok := reg.Get("echo")
	if !ok {
		t.Fatal("Get: want found")
	}
	if spec := got.Spec(); spec.Name != "echo" || spec.Description != "desc-echo" {
		t.Fatalf("Spec() = %+v, want echo/desc-echo", spec)
	}
}

func TestRegistry_RegisterErrors(t *testing.T) {
	t.Run("nil tool", func(t *testing.T) {
		if err := New().Register(nil); err == nil {
			t.Fatal("want error, got nil")
		}
	})

	t.Run("empty name", func(t *testing.T) {
		if err := New().Register(fakeTool("")); err == nil {
			t.Fatal("want error, got nil")
		}
	})

	t.Run("duplicate", func(t *testing.T) {
		reg := New()
		if err := reg.Register(fakeTool("dup")); err != nil {
			t.Fatalf("first register: %v", err)
		}
		if err := reg.Register(fakeTool("dup")); err == nil {
			t.Fatal("want error, got nil")
		}
	})
}

func TestRegistry_RegisterAll(t *testing.T) {
	t.Run("registers all", func(t *testing.T) {
		reg := New()
		if err := reg.RegisterAll(fakeTool("a"), fakeTool("b"), fakeTool("c")); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		for _, name := range []string{"a", "b", "c"} {
			if _, ok := reg.Get(name); !ok {
				t.Errorf("tool %q not registered", name)
			}
		}
	})

	t.Run("aborts on duplicate, partial state kept", func(t *testing.T) {
		reg := New()
		if err := reg.Register(fakeTool("dup")); err != nil {
			t.Fatalf("first register: %v", err)
		}
		if err := reg.RegisterAll(fakeTool("ok"), fakeTool("dup"), fakeTool("never")); err == nil {
			t.Fatal("want duplicate error, got nil")
		}
		if _, ok := reg.Get("ok"); !ok {
			t.Error("tools before the failure stay registered")
		}
		if _, ok := reg.Get("never"); ok {
			t.Error("tools after the failure must not be registered")
		}
	})
}

func TestRegistry_GetMissing(t *testing.T) {
	reg := New()
	if _, ok := reg.Get("nope"); ok {
		t.Fatal("Get: want not found")
	}
}

func TestRegistry_ListStableOrder(t *testing.T) {
	reg := New()
	for _, name := range []string{"zeta", "alpha", "mid"} {
		if err := reg.Register(fakeTool(name)); err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
	}

	want := []string{"alpha", "mid", "zeta"} // 按名称升序
	for i := 0; i < 2; i++ {                 // 重复调用结果确定
		specs := reg.List()
		got := make([]string, len(specs))
		for j, s := range specs {
			got[j] = s.Name
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("List() #%d = %v, want %v", i, got, want)
		}
	}
}

func TestFunc_Execute(t *testing.T) {
	tool := fakeTool("echo")

	t.Run("valid args", func(t *testing.T) {
		res, err := tool.Execute(context.Background(), json.RawMessage(`{"v":"hi"}`))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.IsError || res.Data != "echo:hi" {
			t.Fatalf("Result = %+v, want Data=echo:hi IsError=false", res)
		}
	})

	t.Run("invalid args is business failure", func(t *testing.T) {
		res, err := tool.Execute(context.Background(), json.RawMessage(`{not-json`))
		if err != nil {
			t.Fatalf("want nil error, got %v", err)
		}
		if !res.IsError || res.Data == "" {
			t.Fatalf("Result = %+v, want IsError=true with message", res)
		}
	})
}
