#include "vmlinux.h"
#include <bpf/bpf_core_read.h>
#include <bpf/bpf_helpers.h>

struct {
  __uint(type, BPF_MAP_TYPE_RINGBUF);
  __uint(max_entries, 1 << 20);
} process_observation SEC(".maps");

enum proc_event_kind {
  SELF_PROC_EVENT_FORK = 0,
  SELF_PROC_EVENT_EXIT = 1,
  RAW_SELF_PROC_EVENT_FORK = 2,
};

struct fork_event {
  __u32 kind;
  __kernel_pid_t
      parent_tgid; // 出生时刻父进程的 tgid(当前任务即父),与 ps 的 PPID 同源
  __kernel_pid_t
      parent_tid; // 父线程的 tid,来自 ctx->parent_pid;与 parent_tgid 是两套口径
  __kernel_pid_t child_id;    // 新任务的 tid
  __kernel_pid_t child_tgid;  // 新任务的 tgid:与 child_id 相等说明是新进程
  __u32 uid;
  __u8 is_thread;
  u64 ts;
  char parent_comm[TASK_COMM_LEN];
  char child_comm[TASK_COMM_LEN];
};

// 线格式契约:必须与 Go 侧 forkEvent 逐字节一致(events.go 的 forkEventSize)。
// 任意一侧改了结构体而另一侧没跟上,各自的编译期断言都会把它挡住。
_Static_assert(sizeof(struct fork_event) == 72,
               "fork_event 布局变了,同步改 Go 侧 forkEventSize 与字段顺序");

struct exit_event {
  __u32 kind;
  __kernel_pid_t pid;  // 退出者的tid
  __kernel_pid_t tgid; // 退出者的 tgid:pid == tgid 说明退出的是那个组长
  __u32 uid;
  __kernel_pid_t
      parent_tgid; // 退出时刻父进程的 tgid(real_parent),与 ps 的 PPID 同源
  u64 end_ts;
  u64 life_time;
  unsigned exit_code; // 正常退出码(仅当 term_sig == 0 时有意义)
  __u32 term_sig;     // 0 = 正常退出;非 0 = 被该信号终止
  bool group_dead;    // 用来判断那个是线程退出还是进程退出
  char comm[TASK_COMM_LEN];
};