package builtin

import (
	"context"
	"encoding/json"
	"time"

	"small/internal/tool"
)

// GetCurrentTime 构造获取当前本地时间的工具（get_current_time）。
// 无参数、纯查询。Description 刻意强调"模型无实时时间感知"，
// 引导模型在一切涉及日期/时刻的判断前自觉调用——不依赖 system prompt 提示。
func GetCurrentTime() tool.Tool {
	return tool.NewFunc(
		tool.Spec{
			Name:        "get_current_time",
			Description: "获取当前本地时间，格式：YYYY-MM-DD HH-MM-SS（如 2026-08-23 15-30-45）。模型不具备实时时间感知，回答涉及具体日期、时间、星期、今天/明天/昨天、相对时间（如\"三天后\"）、时效性判断或生成带时间戳的记录时，必须先调用本工具获取准确时间，不要凭记忆猜测当前时刻。有时可以用这个来把控那个自己连续工作时间",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {}
			}`),
		},
		func(_ context.Context, _ json.RawMessage) (tool.Result, error) {
			// 连字符分隔的时分秒（15-30-45），与用户约定的展示格式一致。
			return tool.Result{Data: time.Now().Format("2006-01-02 15-04-05")}, nil
		},
	)
}
