package main

import (
	"bytes"
	"context"
)

const taskCommLen = 16 // 对应 C 的 TASK_COMM_LEN

type Event interface {
	Encode() ([]byte, error)                     // 编码
	Decode([]byte) error                         // 解码
	KindName() string                            //事件类型 | 与那个管道息息相关
	Publish(*Bus, context.Context, []byte) error // 帮助把那个事件推入到通道中
	Consume(*Bus) (<-chan []byte, error)         // 帮助把那个通道展示出来
}

const (
	kindForkValue uint32 = 0 // C: SELF_PROC_EVENT_FORK
	kindExitValue uint32 = 1 // C: SELF_PROC_EVENT_EXIT

	kindSysEnterValue uint32 = 3 // C: SELF_PROC_SYS_ENTER
	kindSysExitValue  uint32 = 4 // C: SELF_PROC_SYS_EXIT

	kindForkName = "fork" // 总线 key,也是打印前缀
	kindExitName = "exit"

	kindSysEnterName = "sys_enter"
	kindSysExitName  = "sys_exit"
)

// 编译期间断言
var _ (Event) = (*forkEvent)(nil)

// cstr 把 C 的定长 char[] 转成 Go string:按第一个 \0 截断
func cstr(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		return string(b[:i])
	}
	return string(b)
}
