# temp/ 独立验证探针，不参与 Go 构建。
# 依赖：clang（已装 18.1.3）、bpftool（已装）、内核带 BTF（/sys/kernel/btf/vmlinux 存在）
BPFTOOL := bpftool
CLANG   := clang
TARGET  := bpf

.PHONY: all gen clean FORCE

all: min_open.o

# vmlinux.h 依赖 FORCE 每次重生成：上次失败残留的 0 字节空文件会让 make 误判已生成而跳过
vmlinux.h: FORCE
	$(BPFTOOL) btf dump file /sys/kernel/btf/vmlinux format c > $@

FORCE:

# -Wno-missing-declarations：屏蔽 vmlinux.h 里 BTF 重建产生的无害孤立声明
min_open.o: min_open.bpf.c vmlinux.h
	$(CLANG) -O2 -g -target $(TARGET) -Wall -Wno-missing-declarations -c $< -o $@

gen: vmlinux.h

clean:
	rm -f min_open.o 







