// Package builtin 内置工具集合，扁平子包放置（同 provider/retry 的方式）。
package builtin

import (
	"context"
	"encoding/json"

	"small/internal/tool"
)

// Echo 返回"原样回显"示例工具，用于演示工具调用链路的完整形态：
// 声明（JSON Schema）→ 参数解码 → 执行 → 业务失败标记。
// 参数非法属系统边界，按约定以 IsError 结果返回，而非 error。
func Echo() tool.Tool {
	return tool.NewFunc(
		tool.Spec{
			Name:        "echo",
			Description: "原样返回输入的文本，用于演示工具调用链路。",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"message": {"type": "string", "description": "要回显的文本"}
				},
				"required": ["message"]
			}`),
		},
		func(_ context.Context, args json.RawMessage) (tool.Result, error) {
			var in struct {
				Message string `json:"message"`
			}
			if err := json.Unmarshal(args, &in); err != nil {
				return tool.Result{Data: "参数错误: " + err.Error(), IsError: true}, nil
			}
			return tool.Result{Data: in.Message}, nil
		},
	)
}
