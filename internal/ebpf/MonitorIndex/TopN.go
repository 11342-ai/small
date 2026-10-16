package monitorindex

import "sync"

// TopN 按 key 计数;Take 取走即换新表,一个窗口一张表。
type TopN struct {
	mu sync.Mutex
	m  map[string]int64
}

func NewTopN() *TopN { return &TopN{m: make(map[string]int64)} }

func (t *TopN) Add(key string) {
	t.mu.Lock()
	t.m[key]++
	t.mu.Unlock()
}

// Take 取走本窗口的表并换一张新的:读完即清,不需要额外的定时清理。
func (t *TopN) Take() map[string]int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	old := t.m
	t.m = make(map[string]int64)
	return old
}
