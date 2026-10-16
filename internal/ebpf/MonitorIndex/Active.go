package monitorindex

import "sync/atomic"

// Active 用 fork/exit 净差衡量活跃进程数。
// 两侧口径必须落在同一个文件里:创建侧只算非线程,退出侧只算整组退出,
// 少过滤一边,线程事件就会让净差单向漂移。
type Active struct{ net atomic.Int64 }

func NewActive() *Active { return &Active{} }

// Fork 记一次创建:isThread 为真的不算新进程。
func (a *Active) Fork(isThread bool) {
	if isThread {
		return
	}
	a.net.Add(1)
}

// Exit 记一次退出:只有整组退出(group_dead)才算进程消失。
func (a *Active) Exit(groupDead bool) {
	if !groupDead {
		return
	}
	a.net.Add(-1)
}

func (a *Active) Net() int64 { return a.net.Load() }

// Rebase 用 /proc 数出来的真实进程数重锚,抹掉 ringbuf 丢包带来的累计漂移。
func (a *Active) Rebase(n int64) { a.net.Store(n) }
