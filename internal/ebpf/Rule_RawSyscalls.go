package main

import (
	"fmt"
	"sort"
	"time"

	"github.com/cilium/ebpf"
)

const (
	cntMax   = 512
	interval = 5 * time.Second
)

type snapshot struct {
	calls [cntMax]uint64
	errs  [cntMax]uint64
}

var errnoNames = map[uint32]string{
	1: "EPERM", 2: "ENOENT", 9: "EBADF", 13: "EACCES",
	14: "EFAULT", 17: "EEXIST", 21: "EISDIR", 22: "EINVAL",
}

func errnoName(e uint32) string {
	if s, ok := errnoNames[e]; ok {
		return s
	}
	return "?"
}

// syscallNames 只列我们观测范围内的调用号(x86_64),其余用 ? 兜底。
// 表小且固定,不引第三方生成代码。
var syscallNames = map[uint32]string{
	0: "read", 1: "write", 35: "nanosleep", 202: "futex", 257: "openat",
}

func syscallName(id uint32) string {
	if s, ok := syscallNames[id]; ok {
		return s
	}
	return "?"
}

// readArray 读一张 per-CPU array,并对各 CPU 求和
// map 未写过的键返回0
func readArray(m *ebpf.Map, dst *[cntMax]uint64) {
	n := int(m.MaxEntries())
	for k := uint32(0); k < uint32(n) && k < cntMax; k++ {
		var per []uint64
		if err := m.Lookup(&k, &per); err != nil {
			continue
		}
		var sum uint64
		for _, v := range per {
			sum += v
		}
		dst[k] = sum
	}
}

func report(callM, errM *ebpf.Map, prev *snapshot) {
	cur := snapshot{}
	readArray(callM, &cur.calls)
	readArray(errM, &cur.errs)

	var dCalls, dErrs uint64
	for i := 0; i < cntMax; i++ {
		dCalls += cur.calls[i] - prev.calls[i]
		dErrs += cur.errs[i] - prev.errs[i]
	}

	old := prev.errs
	*prev = cur
	if dCalls == 0 {
		return
	}

	rate := float64(dCalls) / interval.Seconds()
	ratio := float64(dErrs) / float64(dCalls)
	fmt.Printf("syscall %.0f 次/秒，错误率 %.1f%%（%d/%d）\n", rate, ratio*100, dErrs, dCalls)

	type item struct {
		errno uint32
		delta uint64
	}
	var list []item
	for i := 0; i < cntMax; i++ {
		if d := cur.errs[i] - old[i]; d > 0 {
			list = append(list, item{uint32(i), d})
		}
	}

	sort.Slice(list, func(a, b int) bool { return list[a].delta > list[b].delta })
	for i, e := range list {
		if i >= 5 {
			break
		}
		fmt.Printf("  errno %d(%s) %d 次\n", e.errno, errnoName(e.errno), e.delta)
	}
}
