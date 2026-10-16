#include "vmlinux.h"
#include <bpf/bpf_core_read.h>
#include <bpf/bpf_helpers.h>

struct {
  __uint(type, BPF_MAP_TYPE_RINGBUF);
  __uint(max_entries, 1 << 20);
} process_observation SEC(".maps");

// 按照 errno 计数
struct {
    __uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
    __uint(max_entries, 512); // errno值域有限
    __type(key, u32);
    __type(value, u64);
} err_count SEC(".maps");

// 按 syscall id 计数，拿总量（也顺带给了每个调用各自的量）
struct {
    __uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
    __uint(max_entries, 512); // syscall 号也在几百以内
    __type(key, u32);
    __type(value, u64);
} call_count SEC(".maps");

enum proc_event_kind {
  SELF_PROC_EVENT_FORK = 0,
  SELF_PROC_EVENT_EXIT = 1,
  RAW_SELF_PROC_EVENT_FORK = 2, // 目前对应的函数已经丢用，保留下来是为了学习用的
  SELF_PROC_SYS_ENTER = 3,
  SELF_PROC_SYS_EXIT = 4,
};

// = = = //

// 最小存活时长过滤：只在 exit 侧有意义（存活时长只有退出时才知道），fork
// 侧不参与；0 = 不过滤
const volatile unsigned long long min_duration_ns = 0;

extern struct task_struct *bpf_task_from_pid(s32 pid) __ksym;
extern void bpf_task_release(struct task_struct *p) __ksym;

// = = = = = //

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

// 线格式契约:必须与 Go 侧 exitEvent 逐字节一致。
// 任意一侧改了结构体而另一侧没跟上,各自的编译期断言都会把它挡住。
_Static_assert(sizeof(struct exit_event) == 72,
               "exit_event 布局变了,同步改 Go 侧 exitEvent 与字段顺序");

// ==== //

// 目标 cgroup 的 id：路径固定，号再电脑重启一次就要重新分配，所以这里仅仅占位
// 用户态加载，可以直接覆盖掉
const volatile unsigned long long target_cgroup_id = 21692;

// 阈值，默认 1ms，加载时可覆盖
const volatile unsigned long long slow_ns = 1000000;

struct sys_enter{
    __u32 kind;
    __u32 id ; // 系统调用号
    __u32 pid; // 线程号
    __u32 tgid; //线程组编号
    char comm[TASK_COMM_LEN]; // 线程名称
};

struct sys_exit {
    __u32 kind;
    __u32 id ; // 系统调用号
    __u32 pid; // 线程号
    __u32 tgid; //线程组编号
    u64 ts;
    __s64 ret; // 调用的错误码头(>=0 是正常的; <0 就是代表错误)
    char comm[TASK_COMM_LEN]; // 线程名称
};

_Static_assert(sizeof(struct sys_enter) == 32,
               "sys_enter 布局变了,同步改 Go 侧 SysEnter");
_Static_assert(sizeof(struct sys_exit) == 48,
               "sys_exit 布局变了,同步改 Go 侧 SysExit");

struct syscall_start {
    __u64 start_ns;
    __u32 id;
};

struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, 10240);
    __type(key, __u64);
    __type(value, struct syscall_start);
} start_time SEC(".maps");