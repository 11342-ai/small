package builtin

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// TestGetCurrentTimeTool_Execute 验证返回格式：YYYY-MM-DD HH-MM-SS（连字符分隔）。
func TestGetCurrentTimeTool_Execute(t *testing.T) {
	res, err := GetCurrentTime().Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.IsError {
		t.Fatalf("want success, got error: %s", res.Data)
	}
	// 用 time.Parse 校验格式与合法性（非法格式/非法日期都会报错）。
	if _, err := time.Parse("2006-01-02 15-04-05", res.Data); err != nil {
		t.Errorf("time = %q, not in YYYY-MM-DD HH-MM-SS format: %v", res.Data, err)
	}
	// 与当前时间相差不应超过 1 分钟（工具应返回"现在"）。
	got, _ := time.ParseInLocation("2006-01-02 15-04-05", res.Data, time.Local)
	if d := time.Since(got); d > time.Minute || d < -time.Minute {
		t.Errorf("time = %v, deviates from now by %v", got, d)
	}
}
