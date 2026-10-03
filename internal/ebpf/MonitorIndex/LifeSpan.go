package monitorindex

import "sync/atomic"

const (
	ShortLifeRatio     = 0.5 // 短命占比阈值:占位值,等样本分布出来再定
	ShortLifeMinSample = 5   // 窗口内至少这么多样本才判定:1/1、2/2 报 100% 是噪声
)

// LifeSpan 统计退出样本里的"短命占比"。
// 两个计数器跨 goroutine(消费侧写、ticker 侧读),所以用 atomic;
// Take 取走即清零,天然就是一个窗口,不需要额外的定时清理。
type LifeSpan struct {
	maxNs int64 // 判"短命"的阈值(纳秒)
	short atomic.Int64
	total atomic.Int64
}

func NewLifeSpan(maxNs int64) *LifeSpan { return &LifeSpan{maxNs: maxNs} }

// Record 记一个退出样本。groupDead 为 false 的是线程退出,不计入——
// 线程池 churn 的存活时长普遍极短,混进来会让"短命进程占比"失真。
func (s *LifeSpan) Record(lifeNs int64, groupDead bool) {
	if !groupDead {
		return
	}
	s.total.Add(1)
	if lifeNs < s.maxNs {
		s.short.Add(1)
	}
}

// Take 取走本窗口的计数并清零。
func (s *LifeSpan) Take() (short, total int64) {
	return s.short.Swap(0), s.total.Swap(0)
}
