#include <asm-generic/errno-base.h>
#define _GNU_SOURCE
#include <sys/stat.h>
#include <unistd.h>
#include <sys/syscall.h>
#include <linux/futex.h>
#include <fcntl.h>
#include <stdio.h>
#include <errno.h>
#include <time.h>

int main(void) {
    const char *cg = "/sys/fs/cgroup/obs";
    if (mkdir(cg, 0755) != 0 && errno != EEXIST) {
        perror("mkdir");
        return 1;
    }

    // 把自己放在组中
    char procs[256];
    snprintf(procs, sizeof(procs), "%s/cgroup.procs", cg);
    int fd = open(procs, O_WRONLY);
    if (fd < 0) {
        perror("open cgroup.procs");
        return 1;
    }

    char pid[32];
    int n = snprintf(pid, sizeof pid, "%d", getpid());
    if (write(fd, pid, n) != n) {
        perror("write");
        close(fd);
        return 1;
    }
    close(fd);

    // cgroup id 即目录的 inode 号，写进 map 给 BPF 比对
    struct stat st;
    if (stat(cg, &st) == 0)
        printf("cgroup id = %lu\n", (unsigned long)st.st_ino);

    printf("pid=%d\n", getpid());
    fflush(stdout);

    int flutex_word = 0;
    char buf[1];
    struct timespec ts = {0, 20000000}; // 20ms，约 50 次/秒  | 20000000就是20ms
    static int step = 0;
    for (;;) {
        // 设定一个真实存在的文件路径
        int fd = syscall(SYS_openat, AT_FDCWD, "/home/cxr/temp.txt", O_RDWR, 0);
        if (fd >= 0) {
            syscall(SYS_read, fd, buf, 1);
            syscall(SYS_write, fd, "x", 1);
            syscall(SYS_close, fd);
        }

        switch (step++ % 6) {
        case 0:
            syscall(SYS_openat, AT_FDCWD, "/home/cxr/definitely-not-here", O_RDONLY, 0);
            break;
        case 1:
            syscall(SYS_read, -1, buf, 1);
            break;
        case 2:
            // 用坏路径指针触发 EFAULT：拷贝路径时立刻校验，不像 read(fd 0) 那样先阻塞等输入
            syscall(SYS_openat, AT_FDCWD, (void *)1, O_RDONLY, 0);
            break;
        case 3:
            syscall(SYS_openat, AT_FDCWD, "/home/cxr", O_WRONLY, 0);
            break;
        case 4:
            syscall(SYS_openat, AT_FDCWD, "/home/cxr/temp.txt",O_CREAT | O_EXCL | O_RDWR, 0644);
            break;
        case 5 :
            syscall(SYS_futex, &flutex_word, 0xffff, 0, NULL, NULL, 0);
            break;
        }

        syscall(SYS_futex, &flutex_word, FUTEX_WAKE, 1, NULL, NULL, 0);
        syscall(SYS_nanosleep, &ts, NULL);
    }
}