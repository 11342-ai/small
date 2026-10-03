package main

import (
	"context"
	"fmt"
	"sync"
)

// defaultBufSize 通道缓冲深度,由调用方在 NewBus 时决定
const defaultBufSize = 32

var MessageChannel Bus

type Bus struct {
	mu   sync.RWMutex
	subs map[string]chan []byte
	buf  int
}

func NewBus(bufSize int) {
	MessageChannel = Bus{subs: make(map[string]chan []byte), buf: bufSize}
}

// Declare 建好某 kind 的通道(已存在则复用)。装配期调用,是通道唯一的出生地。
func (b *Bus) Declare(kind string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.subs[kind]; ok {
		return
	}
	b.subs[kind] = make(chan []byte, b.buf)
}

// Subscribe 只查不建:拿不到说明该 kind 没被 Declare(多半是名字对不上),
// 这个没有解决多个消费者的问题，这个应该要注意
func (b *Bus) Subscribe(kind string) (<-chan []byte, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	c, ok := b.subs[kind]
	if !ok {
		return nil, fmt.Errorf("不存在这样子的一个通道: %s", kind)
	}
	return c, nil
}

// Publish 把事件投进它 kind 对应的通道;通道满则阻塞,直到有空位或 ctx 取消。
// 通道不存在直接报错:静默丢事件比报错难查得多。
func (b *Bus) Publish(ctx context.Context, e []byte, kind string) error {
	b.mu.RLock()
	c, ok := b.subs[kind]
	b.mu.RUnlock()
	if !ok {
		return fmt.Errorf("不存在这样子的一个通道: %s", kind)
	}
	select {
	case c <- e:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
