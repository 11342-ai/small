package main

import (
	"cmp"
	"context"
	monitorindex "ebpfprobe/MonitorIndex"
	"fmt"
	"os"
	"slices"
	"time"

	"golang.org/x/sys/unix"
)

// ForkConsume 只吃 fork 通道:事件类型和窗口各归各的,别拿 fork 的结构去解 exit。
func ForkConsume(ctx context.Context, sub <-chan []byte, name string, act *monitorindex.Active) {
	rw := monitorindex.NewRate(monitorindex.DefaultWindowSec)
	go TimeTicker(ctx, time.Second, rw, name)

	top := monitorindex.NewTopN()
	go TopTicker(ctx, time.Second, top, name)

	for {
		select {
		case <-ctx.Done():
			return
		case e := <-sub:
			var f forkEvent
			if f.Decode(e) != nil {
				continue // 坏包丢这一条,不算链路失败
			}
			rw.Add(int64(f.Ts)) // 内核时间戳才是事件真正发生的时刻
			act.Fork(f.IsThread == 1)
			top.Add(cstr(f.ParentComm[:])) // key 用创建者 comm:种类少且稳定,pid 会复用
		}
	}
}

// ExitConsume 只吃 exit 通道。注意退出事件的时间戳字段叫 EndTs,没有 Ts。
func ExitConsume(ctx context.Context, sub <-chan []byte, name string, act *monitorindex.Active) {
	rw := monitorindex.NewRate(monitorindex.DefaultWindowSec)
	go TimeTicker(ctx, time.Second, rw, name)

	span := monitorindex.NewLifeSpan(shortLifeNs)
	go LifeTicker(ctx, time.Second, span, name)

	for {
		select {
		case <-ctx.Done():
			return
		case e := <-sub:
			var x exitEvent
			if x.Decode(e) != nil {
				continue
			}
			rw.Add(int64(x.EndTs))
			span.Record(int64(x.LifeTime), x.GroupDead == 1)
			act.Exit(x.GroupDead == 1)
		}
	}
}

// 指标1:创建/退出速率。基准=稳态每秒 fork/exit 次数;触发=上一个完整秒相对前 5 个完整秒突增。

// nowNs 取 CLOCK_MONOTONIC 的纳秒值——与 C 侧 bpf_ktime_get_ns() 同一时基。
// 不能用 time.Now():那是墙钟,和事件里的内核时间戳不同源,窗口会错位。
func nowNs() int64 {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts); err != nil {
		return 0 // 拿不到就不推进窗口,只影响读侧刷新,不影响计数
	}
	return ts.Nano()
}

func TimeTicker(ctx context.Context, interval time.Duration, rw *monitorindex.Rate, name string) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			rw.Advance(nowNs()) // 先对齐到当前秒,否则空闲那一秒会一直报旧值
			last, base, ok := rw.Burst()
			if ok && last/base > monitorindex.BurstRatio {
				fmt.Printf("指标1 : %s 速率突增: 上一秒 %.0f 次, 前 5 秒均值 %.2f 次/秒\n", name, last, base)
			}
		}
	}
}

// 指标2 ：短命进程比例与生命周期
// 基准：正常 worker 存活时间。触发：lifetime 低于阈值且占比升高
// 流式：按 lifetime 分桶，或只输出短命样本。解决：看创建开销是否来自短生命周期进程。

// shortLifeNs 判"短命"的阈值:先按 100ms 试,跑一段看分布再定。
const shortLifeNs = int64(100 * time.Millisecond)

func LifeTicker(ctx context.Context, interval time.Duration, span *monitorindex.LifeSpan, name string) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			short, total := span.Take() // 取走并清零,一个 tick 就是一个窗口
			if total < monitorindex.ShortLifeMinSample {
				continue // 样本太少,占比没有意义(1/1 恒等于 100%)
			}
			if ratio := float64(short) / float64(total); ratio > monitorindex.ShortLifeRatio {
				fmt.Printf("指标2 : %s 短命进程占比 %.2f (%d/%d)\n", name, ratio, short, total)
			}
		}
	}
}

// 指标3 : 活跃进程数。
// 基准：稳定并发数。触发：持续上升或大幅波动。
// 流式：周期输出 size 和净增。解决：判断创建退出是否失衡、是否造成 PID 压力。

const (
	activeTickSec   = time.Second // 输出间隔
	rebaseEvery     = 30          // 每多少拍用 /proc 的真实值重锚一次,抵消丢包造成的漂移
	activeDeltaWarn = 10          // 本窗口净增的绝对值超过它才打印,抑制稳态噪声
)

func ActiveTicker(ctx context.Context, interval time.Duration, act *monitorindex.Active) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	if n := procCount(); n >= 0 {
		act.Rebase(n) // 先锚一次,否则第一次重锚前打印的是"相对启动的净差"
	}
	prev := act.Net()

	for i := 1; ; i++ {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cur := act.Net()
			delta := cur - prev
			prev = cur
			if delta > activeDeltaWarn || delta < -activeDeltaWarn {
				fmt.Printf("指标3 : 活跃进程 %d, 本窗口净增 %+d\n", cur, delta)
			}
			if i%rebaseEvery == 0 {
				if n := procCount(); n >= 0 {
					act.Rebase(n)
					prev = n
				}
			}
		}
	}
}

// procCount 数 /proc 下的纯数字目录。内核只给线程组组长建目录,
// 所以这个数正好对应"非线程创建 / 整组退出"的口径。
func procCount() int64 {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return -1 // 读不到就不重锚
	}
	var n int64
	for _, e := range ents {
		if name := e.Name(); e.IsDir() && name[0] >= '0' && name[0] <= '9' {
			n++
		}
	}
	return n
}

// 指标4 : 父子/comm 创建来源。
// 基准：正常 parent_comm 分布。触发：某个 comm 创建速率异常。
// 流式：按 parent_comm 聚合，每秒输出 top 创建者。解决：定位谁在 fork。
// 注意：fork 事件里没有子任务的 tgid，所以这里只能按创建者聚合，
// "区分 TGID/PID" 得看 C 侧是否补了 child_tgid（bpftrace 侧可直接读 child->tgid）。

const topShow = 3 // 每秒展示前几名

func TopTicker(ctx context.Context, interval time.Duration, top *monitorindex.TopN, name string) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m := top.Take() // 取走即清,一个 tick 一个窗口
			if len(m) == 0 {
				continue
			}
			rank := make([]string, 0, len(m))
			for k := range m {
				rank = append(rank, k)
			}
			slices.SortFunc(rank, func(a, b string) int { return cmp.Compare(m[b], m[a]) })

			fmt.Printf("指标4 : %s top 创建者:", name)
			for i, k := range rank {
				if i == topShow {
					break
				}
				fmt.Printf("%s=%d", k, m[k])
			}
			fmt.Println()
		}
	}
}
