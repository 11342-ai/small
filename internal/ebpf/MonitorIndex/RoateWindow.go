// Package monitorindex 放监控指标的计算部件。
package monitorindex

// Rate 是按秒分桶的滑动窗口速率计。
// 约定:传入的时间一律是纳秒,且与事件同源(内核 CLOCK_MONOTONIC),否则分桶会错位。

const (
	DefaultWindowSec = 10  // 窗口长度(秒):Burst 要往回看 6 个完整秒,不能小于 7
	BurstRatio       = 2   // 突增阈值:上一个完整秒 / 前 5 个完整秒均值
	rateMinBase      = 2.0 // 前 5 秒均值低于此值就不判定——基数太小,比值没有意义
)

type Rate struct {
	buckets []int64
	idx     int
	lastSec int64
	size    int
	init    bool // lastSec 的零值就是合法的"第 0 秒",所以要另用一个标志表示"还没初始化"
}

func NewRate(seconds int) *Rate {
	return &Rate{buckets: make([]int64, seconds), size: seconds}
}

// Add 记一次事件,now 是事件发生时刻(纳秒)。跨秒时先把窗口推进到它所在的秒。
func (r *Rate) Add(now int64) {
	sec := now / 1e9
	if !r.init {
		r.init, r.lastSec, r.idx = true, sec, 0
	} else if sec != r.lastSec {
		r.shift(sec)
	}
	r.buckets[r.idx]++
}

// Advance 把窗口推进到 now 所在的秒。读侧(ticker)每拍必须调一次:
// 窗口只在 Add 时推进的话,没有事件的那一秒读数会一直粘在上一次的值上。
func (r *Rate) Advance(now int64) {
	sec := now / 1e9
	if !r.init {
		r.init, r.lastSec, r.idx = true, sec, 0
		return
	}
	if sec != r.lastSec {
		r.shift(sec)
	}
}

// shift 把 idx 推进到 sec 所在的秒,途经的桶(含中间被跳过的整秒)一并清零。
func (r *Rate) shift(sec int64) {
	gap := sec - r.lastSec
	if gap <= 0 {
		return // 乱序/同秒:当作落在当前桶,不动窗口
	}
	if gap >= int64(r.size) {
		for i := range r.buckets {
			r.buckets[i] = 0
		}
		r.idx = 0
	} else {
		for i := int64(0); i < gap; i++ {
			r.idx = (r.idx + 1) % r.size
			r.buckets[r.idx] = 0
		}
	}
	r.lastSec = sec
}

// Burst 用"上一个完整秒"跟"它之前 5 个完整秒的均值"比,两段区间不重叠,免得自己稀释自己。
// 当前桶是还没走完的一秒,不参与判定。ok=false 表示基数太低(base 太小),此时比值没有意义。
func (r *Rate) Burst() (last, base float64, ok bool) {
	n := r.size
	last = float64(r.buckets[(r.idx-1+n)%n])
	var sum int64
	for i := 2; i <= 6; i++ {
		sum += r.buckets[(r.idx-i+n)%n]
	}
	base = float64(sum) / 5
	return last, base, base >= rateMinBase
}
