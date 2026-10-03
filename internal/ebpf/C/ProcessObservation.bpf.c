#include "vmlinux.h"
#include <bpf/bpf_core_read.h>
#include <bpf/bpf_helpers.h>
#include "Channel.c"

char LICENSE[] SEC("license") = "Dual BSD/GPL";

// 挂载点 : tracepoint:sched:sched_waking + tracepoint:sched:sched_switch :
// 进程上下文切换与 CPU 调度延迟 挂载点 :
// sched:sched_process_fork、sched:sched_process_exit : 进程创建与退出

// 最小存活时长过滤：只在 exit 侧有意义（存活时长只有退出时才知道），fork
// 侧不参与；0 = 不过滤
const volatile unsigned long long min_duration_ns = 0;

extern struct task_struct *bpf_task_from_pid(s32 pid) __ksym;
extern void bpf_task_release(struct task_struct *p) __ksym;

// 预计实现的指标:创建次数 / 父子关系 / comm
// 线程/进程:child_id 与 child_tgid 不等即新线程,相等即新进程;is_thread 是给用户态的便捷位。
// raw 版直接读 child->tgid,普通版靠 bpf_task_from_pid
SEC("tracepoint/sched/sched_process_fork")
int tracepoint__sched__sched_process_fork(
    struct trace_event_raw_sched_process_fork *ctx) {
  // 主要是将那个时间塞到那个 map 中
  // 对于后面统计某一个时间段内，那个进程fork的次数，倒是可以用那个 ts 为滑动窗口 我们这个只是为了将那个fork事件，拿出来进行一个统计而已

  struct fork_event *e =
      bpf_ringbuf_reserve(&process_observation, sizeof(*e), 0);
  if (!e)
    return 0;

  __kernel_pid_t parent_tid = ctx->parent_pid;
  __kernel_pid_t child_tid = ctx->child_pid;

  e->parent_tgid = bpf_get_current_pid_tgid() >> 32;
  e->uid = bpf_get_current_uid_gid() & 0xFFFFFFFF;
  e->parent_tid = parent_tid;
  e->child_id = child_tid;
  e->kind = SELF_PROC_EVENT_FORK; // 确保这个先不出错先
  e->ts = bpf_ktime_get_ns();
  bpf_get_current_comm(e->parent_comm, sizeof(e->parent_comm));

  // 子任务的 tid ctx 直接给;tgid 要顺着 pid 查回 task_struct 才拿得到。
  // 查不到就退回 child_id,与 is_thread 的兜底口径一致(当作新进程)。
  struct task_struct *child = bpf_task_from_pid(child_tid);
  if (child) {
    e->child_tgid = BPF_CORE_READ(child, tgid);
    e->is_thread = e->child_tgid != child_tid;
    BPF_CORE_READ_STR_INTO(&e->child_comm, child, comm); // 必须在 release 之前
    bpf_task_release(child);
  } else {
    e->child_tgid = child_tid; // 查不到就当作新进程,兜底口径与 is_thread 一致
    e->is_thread = 0;
    __builtin_memset(e->child_comm, 0, sizeof(e->child_comm));
  }
  bpf_ringbuf_submit(e, 0);
  return 0;
}

// 预计实现的观测指标 : 退出次数 / 进程生命周期
// 后期用户态，可以通过那个时间窗口、那个进程生命周期、comm 进行那个观测管理了
SEC("tracepoint/sched/sched_process_exit")
int tracepoint__sched__sched_process_exit(
    struct trace_event_raw_sched_process_exit *ctx) {
  // 生命周期，可以通过那个 end_ts - start_ts 解决

  struct exit_event *e;
  u64 end_ts;
  struct task_struct *task = (struct task_struct *)bpf_get_current_task();
  end_ts = bpf_ktime_get_ns(); // exit时的单调时间
  u64 life_time = end_ts - BPF_CORE_READ(task, start_time);

  if (min_duration_ns && life_time < min_duration_ns)
    return 0; // 先过滤再 reserve,省一次预留

  e = bpf_ringbuf_reserve(&process_observation, sizeof(*e), 0);
  if (!e)
    return 0;

  e->kind = SELF_PROC_EVENT_EXIT;
  __u64 id = bpf_get_current_pid_tgid();
  e->pid = (__kernel_pid_t)id;
  e->tgid = (__kernel_pid_t)(id >> 32);
  e->uid = bpf_get_current_uid_gid() & 0xFFFFFFFF;
  e->end_ts = end_ts;
  e->life_time = life_time;
  e->parent_tgid = BPF_CORE_READ(task, real_parent, tgid);
  unsigned code = BPF_CORE_READ(task, exit_code);
  e->exit_code = (code >> 8) & 0xff; // 展示成人可以读取的样子
  e->term_sig = code & 0x7f;
  e->group_dead = ctx->group_dead;
  bpf_get_current_comm(e->comm, sizeof(e->comm));

  bpf_ringbuf_submit(e, 0);
  return 0;
}