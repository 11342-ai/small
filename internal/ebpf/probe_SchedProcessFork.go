package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/binary"
	"errors"
	"fmt"

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
		default:
			continue // C 侧加了新 kind 而 Go 侧没跟上,丢掉比乱解安全
		}
	}
}
