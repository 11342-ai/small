package main

import (
	"bytes"
	"fmt"
	"log"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/ringbuf"
	"github.com/cilium/ebpf/rlimit"
)

// LoadProgs 读 ELF + load 进内核:一个 .o 只做一次,挂载多个点也共用它
// ELF 是受到编码后的二进制；那个 path 是相对路径
func LoadProgs(ELF []byte, path string) (*ebpf.Collection, error) {
	if err := rlimit.RemoveMemlock(); err != nil {
		log.Println("提示: 未能放开 memlock rlimit(新内核可忽略): ", err)
	}
	spec, err := ebpf.LoadCollectionSpecFromReader(bytes.NewReader(ELF))
	if err != nil {
		return nil, fmt.Errorf("解析内置的 %s 失败: %w", path, err)
	}
	coll, err := ebpf.NewCollection(spec)
	if err != nil {
		return nil, fmt.Errorf("load %s 失败: %w", path, err)
	}
	return coll, nil
}

// Attach_ELF 挂载到 tracepoint:group/name 是挂载点,progName 是 .o 里的函数符号名
func Attach_ELF(Coll *ebpf.Collection, group, name, progName string) (link.Link, error) {
	prog, ok := Coll.Programs[progName]
	if !ok {
		Coll.Close()
		return nil, fmt.Errorf("没找到程序 %s,实际有 %v(检查 C 侧函数名是否被改过)", progName, programNames(Coll))
	}
	tp, err := link.Tracepoint(group, name, prog, nil)
	if err != nil {
		Coll.Close()
		return nil, fmt.Errorf("attach %s:%s 失败: %w", group, name, err)
	}
	return tp, nil
}

// raw 版本的 attach_ELF，没有 group 的那种
func AttachRawTP(coll *ebpf.Collection, name, progName string) (link.Link, error) {
	prog, ok := coll.Programs[progName]
	if !ok {
		return nil, fmt.Errorf("没找到程序 %s,实际有 %v", progName, programNames(coll))
	}
	rtp, err := link.AttachRawTracepoint(link.RawTracepointOptions{Name: name, Program: prog})
	if err != nil {
		return nil, fmt.Errorf("attach raw tracepoint %s 失败: %w", name, err)
	}
	return rtp, nil
}

// programNames 列出 .o 里实际的程序名(即 C 侧函数符号名),名字对不上时报出来最好使
func programNames(coll *ebpf.Collection) []string {
	names := make([]string, 0, len(coll.Programs))
	for name := range coll.Programs {
		names = append(names, name)
	}
	return names
}

// NewEventReader 取 ringbuf 的读端:资源获取独立一处,失败属启动期错误,由调用方处理
func NewEventReader(coll *ebpf.Collection, mapName string) (*ringbuf.Reader, error) {
	rd, err := ringbuf.NewReader(coll.Maps[mapName])
	if err != nil {
		return nil, fmt.Errorf("建立 ringbuf reader 失败: %w", err)
	}
	return rd, nil
}
