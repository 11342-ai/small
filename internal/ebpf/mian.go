package main

import (
	"context"
	monitorindex "ebpfprobe/MonitorIndex"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	// 1. 读 ELF + load 进内核:一个 .o 只做一次
	coll, err := LoadProgs(ProcessObservation, "C/ProcessObservation.o")
	if err != nil {
		fail("%+v", err)
	}
	defer coll.Close()

	// 2. 一个挂载点一次 attach,各自拿自己的 link
	forkLink, err := Attach_ELF(coll, progSchedProcessForkGroup, progSchedProcessForkName, progSchedProcessFork)
	if err != nil {
		fail("%+v", err)
	}
	defer forkLink.Close()

	exitLink, err := Attach_ELF(coll, progSchedProcessExitGroup, progSchedProcessExitName, progSchedProcessExit)
	if err != nil {
		fail("挂载失败: %+v", err)
	}
	defer exitLink.Close()

	fmt.Println("已挂载 sched_process_fork(raw tp) / sched_process_exit(tp),开始打印事件(Ctrl-C 退出)")

	// ctx 只用于打断阻塞的 ringbuf 读循环;<-Done 之后读循环返回,defer 依次 detach、卸载
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 3. 总线 + 消费者:装配期先声明通道(建立只在这里发生),订阅只负责取
	NewBus(defaultBufSize)
	for _, kind := range []string{kindForkName, kindExitName} {
		MessageChannel.Declare(kind)
	}

	fork_ch, err := MessageChannel.Subscribe(kindForkName)
	if err != nil {
		fail("%+v", err)
	}
	exit_ch, err := MessageChannel.Subscribe(kindExitName)
	if err != nil {
		fail("%+v", err)
	}
	// 活跃进程计数被两个消费者共用(一个加、一个减),所以由组合根持有再注入
	act := monitorindex.NewActive()
	go ForkConsume(ctx, fork_ch, "fork", act)
	go ExitConsume(ctx, exit_ch, "exit", act)
	go ActiveTicker(ctx, activeTickSec, act)

	rd, err := NewEventReader(coll, ringbufMapName)
	if err != nil {
		fail("%+v", err)
	}
	if err := Run(ctx, rd, &MessageChannel); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintf(os.Stderr, "读取事件失败: %v\n", err)
	}

	fmt.Println("\n已退出")
}

// fail 打印后直接退出。注意它会跳过 defer,即不显式 detach;
// 但进程退出时内核会关掉这些 fd,效果等同 —— 只在"马上就死"的错误路径上用。
// 这个一般是添加那个错误信息的，format一定要 %+v 的形式，防止那个错误信息被省略了；特别是那个 ebpf 常常会省略错误信息，导致看不到关键行
func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "错误: "+format+"\n", args...)
	os.Exit(1)
}
