#define _GNU_SOURCE
#include <unistd.h>
#include <sys/syscall.h>
#include <linux/futex.h>
#include <fcntl.h>
#include <stdio.h>
#include <time.h>

int main(void) {
    printf("pid=%d\n", getpid());
    fflush(stdout);

    int flutex_word = 0;
    char buf[1];
    struct timespec ts = {0, 20000000}; // 20ms，约 50 次/秒  | 20000000就是20ms
    for (;;) {
        // 设定一个真实存在的文件路径
        int fd = syscall(SYS_openat, AT_FDCWD, "/home/cxr/temp.txt", O_RDWR, 0);
        if (fd >= 0) {
            syscall(SYS_read, fd, buf, 1);
            syscall(SYS_write, fd, "x", 1);
            syscall(SYS_close, fd);
        }
        syscall(SYS_futex, &flutex_word, FUTEX_WAKE, 1, NULL, NULL, 0);
        syscall(SYS_nanosleep, &ts, NULL);
    }
}