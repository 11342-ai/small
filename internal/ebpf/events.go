package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"unsafe"
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

	kindForkName = "fork" // 总线 key,也是打印前缀
	kindExitName = "exit"
)

// 编译期间断言
var _ (Event) = (*forkEvent)(nil)
var _ (Event) = (*exitEvent)(nil)

type forkEvent struct {
	Kind       uint32
	ParentTGID int32 // __kernel_pid_t 是 s32
	ParentTID  int32
	ChildID    int32 // 新任务的 tid
	ChildTGID  int32 // 新任务的 tgid:与 ChildID 相等即新进程
	UID        uint32
	IsThread   uint8   // 1=线程,0=新进程;与 ChildTGID != ChildID 同义,方便直接用
	_          [7]byte // C 侧 u64 ts 的 8 字节对齐洞,is_thread 占 1 字节,还要吃掉剩下 7 字节
	Ts         uint64
	ParentComm [taskCommLen]byte
	ChildComm  [taskCommLen]byte
}

// 线格式契约:必须与 C 侧 struct fork_event 逐字节一致(72 字节)。
// C 侧结构体改了而这里没跟上,下面两行会直接编译失败——比运行时解出乱数据好查得多。
const forkEventSize = 72

var _ [forkEventSize - unsafe.Sizeof(forkEvent{})]byte
var _ [unsafe.Sizeof(forkEvent{}) - forkEventSize]byte

func (f *forkEvent) Encode() ([]byte, error) {
	return json.Marshal(f)
}

func (f *forkEvent) Decode(data []byte) error {
	return json.Unmarshal(data, f)
}

func (f *forkEvent) KindName() string {
	return kindForkName
}

func (e forkEvent) String() string {
	return fmt.Sprintf("fork  ParentComm=%s(ParentTID=%d,uid=%d) -> ChildComm=%s(ChildID=%d ChildTGID=%d) ts=%d is_thread=%v",
		cstr(e.ParentComm[:]), e.ParentTID, e.UID, cstr(e.ChildComm[:]), e.ChildID, e.ChildTGID, e.Ts, e.IsThread == 1)
}

func (e *forkEvent) Publish(bus *Bus, ctx context.Context, data []byte) error {
	err := bus.Publish(ctx, data, e.KindName())
	if err != nil {
		return fmt.Errorf("forkEvent 投递出错: %w", err)
	}
	return nil
}

func (e *forkEvent) Consume(bus *Bus) (<-chan []byte, error) {
	c, ok := bus.Subscribe(e.KindName())
	if ok != nil {
		return nil, fmt.Errorf("不存在这名为 %s 的通道: %s", e.KindName(), ok)
	}
	return c, nil
}

type exitEvent struct {
	Kind       uint32
	Pid        int32
	Tgid       int32
	UID        uint32
	ParentTGID int32
	_          uint32 // C 侧 u64 ts 的 8 字节对齐洞,吃掉这 4 字节
	EndTs      uint64
	LifeTime   uint64
	ExitCode   uint32 // C 的 unsigned
	TermSig    uint32
	GroupDead  uint8 // C 的 bool 是 1 字节
	Comm       [taskCommLen]byte
}

func (f *exitEvent) Encode() ([]byte, error) {
	return json.Marshal(f)
}

func (f *exitEvent) Decode(data []byte) error {
	return json.Unmarshal(data, f)
}

func (f *exitEvent) KindName() string {
	return kindExitName
}

func (e exitEvent) String() string {
	return fmt.Sprintf("exit  Comm=%s(tid=%d tgid=%d ppid=%d) 存活 %.3f ms code=%d sig=%d group_dead=%v",
		cstr(e.Comm[:]), e.Pid, e.Tgid, e.ParentTGID, float64(e.LifeTime)/1e6,
		e.ExitCode, e.TermSig, e.GroupDead == 1)
}

func (e *exitEvent) Publish(bus *Bus, ctx context.Context, data []byte) error {
	err := bus.Publish(ctx, data, e.KindName())
	if err != nil {
		return fmt.Errorf("forkEvent 投递出错: %w", err)
	}
	return nil
}

func (e *exitEvent) Consume(bus *Bus) (<-chan []byte, error) {
	c, ok := bus.Subscribe(e.KindName())
	if ok != nil {
		return nil, fmt.Errorf("不存在这名为 %s 的通道: %s", e.KindName(), ok)
	}
	return c, nil
}

// cstr 把 C 的定长 char[] 转成 Go string:按第一个 \0 截断
func cstr(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		return string(b[:i])
	}
	return string(b)
}
