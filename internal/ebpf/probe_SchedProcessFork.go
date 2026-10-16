package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"unsafe"

	"github.com/cilium/ebpf/ringbuf"
)

//go:generate clang -O2 -g -target bpf -Wall -Wno-missing-declarations -c C/ProcessObservation.bpf.c -o C/ProcessObservation.o

//go:embed C/ProcessObservation.o
var ProcessObservation []byte

// 程序名 = .o 里的函数符号名(C 侧函数名),不是 SEC 里的 section 名。
// C 侧改了函数名,这里要同步改,否则 coll.Programs 取不到。
const (
	progSchedProcessForkGroup = "sched"
	progSchedProcessForkName  = "sched_process_fork"
	progSchedProcessFork      = "tracepoint__sched__sched_process_fork"

	progSchedProcessExitGroup = "sched"
	progSchedProcessExitName  = "sched_process_exit"
	progSchedProcessExit      = "tracepoint__sched__sched_process_exit"

	// ringbufMapName 必须和 C 侧 SEC(".maps") 的变量名一致
	ringbufMapName = "process_observation"

	// RawProgSchedProcessFork 是 raw 版 fork 程序的符号名,挂载走 AttachRawTP
	RawProgSchedProcessFork = "raw_tp__sched_process_fork"
)

// = = = = = = \\

// 编译期间断言
var _ (Event) = (*forkEvent)(nil)
var _ (Event) = (*exitEvent)(nil)

// = = = = = = \\

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

// = = = = = = = \\

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

// = = = = = = = \\

// Run 阻塞读 ringbuf,按 kind 就地解出具体事件,再交给总线的生产者。
// 不做回调注入:哪种事件走哪个生产者,看这个 switch 就够。
func Run(ctx context.Context, rd *ringbuf.Reader, bus *Bus) error {
	defer rd.Close()

	// Read 是阻塞的,ctx 取消时靠 Close 打断它(之后 Read 返回 ringbuf.ErrClosed)
	go func() {
		<-ctx.Done()
		rd.Close()
	}()

	for {
		rec, err := rd.Read()
		if err != nil {
			if errors.Is(err, ringbuf.ErrClosed) {
				return nil // 是我们自己关的,属于正常退出
			}
			return fmt.Errorf("读取 ringbuf 失败: %w", err)
		}

		raw := rec.RawSample
		if len(raw) < 4 {
			continue // 连 kind 都不够长,坏包丢这一条就行
		}
		switch binary.NativeEndian.Uint32(raw[:4]) {
		case kindForkValue:
			var e forkEvent
			if err := binary.Read(bytes.NewReader(raw), binary.NativeEndian, &e); err != nil {
				continue // 坏包丢这一条,不算链路失败
			}
			byte_struct, err := e.Encode()
			if err != nil {
				return err
			}
			if err := bus.Publish(ctx, byte_struct, e.KindName()); err != nil {
				return err // 投递只在 ctx 取消时失败,正好当作退出信号
			}
		case kindExitValue:
			var e exitEvent
			if err := binary.Read(bytes.NewReader(raw), binary.NativeEndian, &e); err != nil {
				continue
			}
			byte_struct, err := e.Encode()
			if err != nil {
				return err
			}
			if err := bus.Publish(ctx, byte_struct, e.KindName()); err != nil {
				return err // 投递只在 ctx 取消时失败,正好当作退出信号
			}
		case kindSysEnterValue:
			var e SysEnter
			if err := binary.Read(bytes.NewReader(raw), binary.NativeEndian, &e); err != nil {
				continue
			}
			byte_struct, err := e.Encode()
			if err != nil {
				return err
			}
			if err := bus.Publish(ctx, byte_struct, e.KindName()); err != nil {
				return err
			}
		case kindSysExitValue:
			var e SysExit
			if err := binary.Read(bytes.NewReader(raw), binary.NativeEndian, &e); err != nil {
				continue
			}
			byte_struct, err := e.Encode()
			if err != nil {
				return err
			}
			if err := bus.Publish(ctx, byte_struct, e.KindName()); err != nil {
				return err
			}
		default:
			continue // C 侧加了新 kind 而 Go 侧没跟上,丢掉比乱解安全
		}
	}
}
