#include "vmlinux.h"
#include <bpf/bpf_core_read.h>
#include <bpf/bpf_helpers.h>
#include "Channel.c"

char LICENSE[] SEC("license") = "Dual BSD/GPL";

// 预期实现的指标 : syscall次数 | 错误码 | 耗时 | 调用类型 | PID/TGID
// 针对的调用 : read/write/openat/futex
// 这个的核心应该是计数 : 直接 ++ 或者 --； 而且，最好是隔着一段时间再传输一次，做一个定时任务
// 错误码分布这个问题 ，也是可以直接计数，不过得先查询文档或者是说只处理/记录其中可以模拟的几种
// 对于 p99 这类东西，我们应该可以实现一个类似分桶的机制，只保留固定长度的数组，或大顶堆，维护某一个层级大概的大小

SEC("tracepoint/raw_syscalls/sys_enter")
int tracepoint__raw_syscalls__sys_enter(struct trace_event_raw_sys_enter *ctx) {

    if (target_cgroup_id && target_cgroup_id != bpf_get_current_cgroup_id()) 
        return 0;

    u32 temp = ctx->id;
    if (temp != 0 && temp != 1 && temp != 257 && temp != 202 && temp != 35)
        return 0;

    // 只记进入时刻,供 exit 侧相减算耗时。键用 pid_tgid:同一线程同一时刻只会
    // 有一次调用在途,不会串号。这里不送 ringbuf,慢事件由 exit 侧按阈值决定。
    u64 key = bpf_get_current_pid_tgid();
    struct syscall_start s = {.start_ns = bpf_ktime_get_ns(), .id = temp};
    bpf_map_update_elem(&start_time, &key, &s, BPF_ANY);

    return 0;
}

// 先算计数和耗时，在进行事件本身的reverse
// 这个为的是防止reverse失败后，把耗时和计数挡住了
SEC("tracepoint/raw_syscalls/sys_exit")
int tracepoint__raw_syscalls__sys_exit(struct trace_event_raw_sys_exit *ctx) {
    if (target_cgroup_id && target_cgroup_id != bpf_get_current_cgroup_id()) 
        return 0;

    u32 temp_id = ctx->id;
    u64 key = bpf_get_current_pid_tgid();
    __s64 ret = ctx->ret;

    // 计数只覆盖关注的调用。nanosleep(35) 是基准靶子,不计入,否则总调用量被它
    // 占大头,错误率会被稀释。(enter 侧按 id 建键,这里不必再按 id 早退,直接
    // 靠下面的 lookup 命中与否判断即可,免得出现"enter 记了、exit 不删"的漏键。)
    if (temp_id == 0 || temp_id == 1 || temp_id == 257 || temp_id == 202) {
        u64 *c = bpf_map_lookup_elem(&call_count, &temp_id);
        if (c) (*c)++;
        if (ret < 0) {
            u32 errno_ = (u32)(-ret);
            u64 *ec = bpf_map_lookup_elem(&err_count, &errno_);
            if (ec) (*ec)++;
        }
    }

    // 耗时 = 本次返回时刻 - enter 侧记下的进入时刻。取到即删,否则 map 会随调用
    // 无限膨胀;删掉也保证同一个键不会串到下一次调用。
    struct syscall_start *s = bpf_map_lookup_elem(&start_time, &key);
    if (!s)
        return 0;
    u64 delta = bpf_ktime_get_ns() - s->start_ns;
    bpf_map_delete_elem(&start_time, &key);

    // 只有慢调用才占通道,快调用到此为止
    if (delta < slow_ns)
        return 0;

    struct sys_exit *e = bpf_ringbuf_reserve(&process_observation, sizeof(*e), 0);
    if (!e) 
        return 0;

    e->kind = SELF_PROC_SYS_EXIT;
    e->id = temp_id;
    e->pid = key & 0xFFFFFFFF;
    e->tgid = key >> 32;  // 线程组id
    e->ret = ret;
    e->ts = delta;
    bpf_get_current_comm(e->comm, sizeof(e->comm));

    bpf_ringbuf_submit(e, 0);

    return 0;
}