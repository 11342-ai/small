#include "vmlinux.h"
#include <bpf/bpf_core_read.h>
#include <bpf/bpf_helpers.h>
#include "Channel.c"

SEC("raw_tracepoint/sched_process_fork")
int raw_tp__sched_process_fork(struct bpf_raw_tracepoint_args *ctx) {
    struct task_struct *parent = (struct task_struct *)ctx->args[0];
    struct task_struct *child = (struct task_struct *)ctx->args[1];

    struct fork_event *e =
        bpf_ringbuf_reserve(&process_observation, sizeof(*e), 0);
    if (!e)
        return 0;

    e->kind = RAW_SELF_PROC_EVENT_FORK;
    e->parent_tgid = BPF_CORE_READ(parent, tgid);
    e->parent_tid = BPF_CORE_READ(parent, pid);
    e->child_id = BPF_CORE_READ(child, pid);
    e->uid = bpf_get_current_uid_gid() & 0xFFFFFFFF;
    e->is_thread = BPF_CORE_READ(child, tgid) != e->child_id;
    e->ts = bpf_ktime_get_ns();
    BPF_CORE_READ_STR_INTO(&e->parent_comm, parent, comm);
    BPF_CORE_READ_STR_INTO(&e->child_comm, child, comm);
    bpf_ringbuf_submit(e, 0);

    return 0;
}