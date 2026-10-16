package main

import (
	"context"
	monitorindex "ebpfprobe/MonitorIndex"
	_ "embed"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"time"
	"unsafe"
)

//go:generate clang -O2 -g -target bpf -Wall -Wno-missing-declarations -c C/SyscallObservation.bpf.c -o C/SyscallObservation.o

//go:embed C/SyscallObservation.o
var SyscallObservation []byte

// 程序名 = .o 里的函数符号名(C 侧函数名),不是 SEC 里的 section 名。
// C 侧改了函数名,这里要同步改,否则 coll.Programs 取不到。
const (
	progRawSyscallsSysEnterGroup = "raw_syscalls"
	progRawSyscallsSysEnterName  = "sys_enter"
	progRawSyscallsSysEnter      = "tracepoint__raw_syscalls__sys_enter"

	progRawSyscallsSysExitGroup = "raw_syscalls"
	progRawSyscallsSysExitName  = "sys_exit"
	progRawSyscallsSysExit      = "tracepoint__raw_syscalls__sys_exit"
)

// ringbufMapName 不在此重复声明:两条链路共用 Channel.c 的 process_observation,
// 该常量已定义在 probe_SchedProcessFork.go,重复声明会编译期报冲突。

const sysEnterSize = 32

var _ [sysEnterSize - unsafe.Sizeof(SysEnter{})]byte
var _ [unsafe.Sizeof(SysEnter{}) - sysEnterSize]byte

const sysExitSize = 48

var _ [sysExitSize - unsafe.Sizeof(SysExit{})]byte
var _ [unsafe.Sizeof(SysExit{}) - sysExitSize]byte

type SysEnter struct {
	Kind uint32
	Id   uint32
	Pid  uint32
	Tgid uint32
	Comm [taskCommLen]byte
}

type SysExit struct {
	Kind uint32
	Id   uint32
	Pid  uint32
	Tgid uint32
	Ts   uint64
	Ret  int64
	Comm [taskCommLen]byte
}

// = = = = = = = \\

// 编译期间断言:接口实现漏一个方法,这里直接编译失败
var _ (Event) = (*SysEnter)(nil)
var _ (Event) = (*SysExit)(nil)

// = = = = = = = \\

func (e *SysEnter) Encode() ([]byte, error) {
	return json.Marshal(e)
}

func (e *SysEnter) Decode(data []byte) error {
	return json.Unmarshal(data, e)
}

func (e *SysEnter) KindName() string {
	return kindSysEnterName
}

func (e SysEnter) String() string {
	return fmt.Sprintf("sys_enter Comm=%s(tid=%d tgid=%d) syscall=%d",
		cstr(e.Comm[:]), e.Pid, e.Tgid, e.Id)
}

func (e *SysEnter) Publish(bus *Bus, ctx context.Context, data []byte) error {
	if err := bus.Publish(ctx, data, e.KindName()); err != nil {
		return fmt.Errorf("SysEnter 投递出错: %w", err)
	}
	return nil
}

func (e *SysEnter) Consume(bus *Bus) (<-chan []byte, error) {
	c, err := bus.Subscribe(e.KindName())
	if err != nil {
		return nil, fmt.Errorf("不存在这名为 %s 的通道: %w", e.KindName(), err)
	}
	return c, nil
}

// = = = = = = = \\

func (e *SysExit) Encode() ([]byte, error) {
	return json.Marshal(e)
}

func (e *SysExit) Decode(data []byte) error {
	return json.Unmarshal(data, e)
}

func (e *SysExit) KindName() string {
	return kindSysExitName
}

func (e SysExit) String() string {
	return fmt.Sprintf("sys_exit Comm=%s(tid=%d tgid=%d) syscall=%d(%s) latency=%.3fms ret=%d",
		cstr(e.Comm[:]), e.Pid, e.Tgid, e.Id, syscallName(e.Id), float64(e.Ts)/1e6, e.Ret)
}

func (e *SysExit) Publish(bus *Bus, ctx context.Context, data []byte) error {
	if err := bus.Publish(ctx, data, e.KindName()); err != nil {
		return fmt.Errorf("SysExit 投递出错: %w", err)
	}
	return nil
}

func (e *SysExit) Consume(bus *Bus) (<-chan []byte, error) {
	c, err := bus.Subscribe(e.KindName())
	if err != nil {
		return nil, fmt.Errorf("不存在这名为 %s 的通道: %w", e.KindName(), err)
	}
	return c, nil
}

// = = = = = = = \\

// SysEnterConsume 只吃 sys_enter 通道。先只做打印,聚合/限流留待后续。
func SysEnterConsume(ctx context.Context, sub <-chan []byte, name string) {
	for {
		select {
		case <-ctx.Done():
			return
		case data := <-sub:
			var e SysEnter
			if e.Decode(data) != nil {
				continue // 坏包丢这一条,不算链路失败
			}
			// fmt.Printf("[%s] %s\n", name, e.String())
		}
	}
}

// 慢事件的输出预算:每秒最多打这么多条。内核侧已按阈值挡掉快调用,
// 这里再压一层是为了防目标进程持续慢调用(如每轮 sleep)把终端刷满。
const slowLogPerSec = 2

var (
	slowLogSent    atomic.Uint64 // 本秒已打印条数,秒边界清零
	slowLogDropped atomic.Uint64 // 本秒被压掉的条数,清零前补一行汇总
)

func slowLogAllow() bool {
	if slowLogSent.Add(1) > slowLogPerSec {
		slowLogDropped.Add(1)
		return false
	}
	return true
}

// slowLogLimiter 每秒把预算清零,并汇报上一秒压掉了多少条,免得静默丢数据。
func slowLogLimiter(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			slowLogSent.Store(0)
			if d := slowLogDropped.Swap(0); d > 0 {
				fmt.Printf("[sys_exit] 慢事件过多,上一秒压制 %d 条\n", d)
			}
		}
	}
}

// SysExitConsume 吃 sys_exit 通道。现在这条通道只承载慢调用(内核侧按 slow_ns 过滤),
// 所以逐条打印是有意义的;再用 slowLogAllow 做一层每秒预算,超出的压掉并计数。
// 速率统计仍走 TopN + ticker,窗口由 tick 周期界定,不依赖事件里的时间戳。
func SysExitConsume(ctx context.Context, sub <-chan []byte, name string) {
	top := monitorindex.NewTopN()
	go SyscallRateTicker(ctx, syscallRateTick, top, name)
	go slowLogLimiter(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case data := <-sub:
			var e SysExit
			if e.Decode(data) != nil {
				continue // 坏包丢这一条,不算链路失败
			}
			top.Add(syscallRateKey)
			if !slowLogAllow() {
				continue
			}
			fmt.Println(e.String())
		}
	}
}
