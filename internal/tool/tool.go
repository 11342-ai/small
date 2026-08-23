package tool

import (
	"context"
	"encoding/json"
)

// Tool 工具能力接口：声明 + 执行。
// 实现方是具体工具（内置工具 / 未来 MCP 适配器 / 测试桩），靠接口承载多态，
// 替换实现无需改动消费方（agent 循环）代码。
type Tool interface {
	// Spec 返回静态声明（装配期固定，模型上下文主要消耗在此）。
	Spec() Spec
	// Execute 给定原始 JSON 参数执行动作，返回规范化结果。
	Execute(ctx context.Context, args json.RawMessage) (Result, error)
}

// Func 将"声明 + 执行函数"打包成 Tool，降低手写样板（用法对应 http.HandlerFunc）。
type Func struct {
	spec Spec
	run  func(context.Context, json.RawMessage) (Result, error)
}

// NewFunc 构造基于函数的工具。run 负责参数解码与执行。
func NewFunc(spec Spec, run func(context.Context, json.RawMessage) (Result, error)) *Func {
	return &Func{spec: spec, run: run}
}

// Spec 实现 Tool。
func (f *Func) Spec() Spec { return f.spec }

// Execute 实现 Tool，委托给构造时注入的执行函数。
func (f *Func) Execute(ctx context.Context, args json.RawMessage) (Result, error) {
	return f.run(ctx, args)
}

// 编译期断言（"虚实现"）：Func 满足 Tool，签名漂移在编译期报错。
var _ Tool = (*Func)(nil)
