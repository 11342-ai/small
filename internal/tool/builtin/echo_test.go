package builtin

import (
	"context"
	"encoding/json"
	"testing"
)

func TestEcho_Spec(t *testing.T) {
	tool := Echo()
	spec := tool.Spec()
	if spec.Name != "echo" {
		t.Fatalf("Name = %q, want echo", spec.Name)
	}
	if len(spec.Parameters) == 0 {
		t.Fatal("Parameters: want non-empty JSON Schema")
	}
	var schema map[string]any
	if err := json.Unmarshal(spec.Parameters, &schema); err != nil {
		t.Fatalf("Parameters not valid JSON Schema: %v", err)
	}
}

func TestEcho_Execute(t *testing.T) {
	tool := Echo()

	t.Run("round trip", func(t *testing.T) {
		res, err := tool.Execute(context.Background(), json.RawMessage(`{"message":"hello"}`))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.IsError || res.Data != "hello" {
			t.Fatalf("Result = %+v, want Data=hello IsError=false", res)
		}
	})

	t.Run("missing field echoes empty", func(t *testing.T) {
		// 不做运行时 schema 校验（YAGNI）：缺失字段解码为空串，正常回显。
		res, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.IsError || res.Data != "" {
			t.Fatalf("Result = %+v, want Data=empty IsError=false", res)
		}
	})

	t.Run("malformed args is business failure", func(t *testing.T) {
		res, err := tool.Execute(context.Background(), json.RawMessage(`{oops`))
		if err != nil {
			t.Fatalf("want nil error, got %v", err)
		}
		if !res.IsError || res.Data == "" {
			t.Fatalf("Result = %+v, want IsError=true with message", res)
		}
	})
}
