package agent

import (
	"context"
	"errors"
	"strings"

	"small/internal/provider"
)

// providerChat 把 provider 后端适配成 agent.Completer。
// 它负责 Turn ↔ provider.Message / ChatRequest 的翻译，
// 是本包内唯一与 provider DTO 打交道的文件——解耦的隔离点。
//
// 统一策略（探测 + 路由）：Complete 在发请求前用类型断言探测 client
// 是否实现了 provider.Streamer——支持则走流式并拼装完整回复，不支持则
// 降级到非流式。探测必须在发请求前完成；一旦进入流中（拿到 2xx 后）
// 绝不再回退，避免重复已吐出的 token。
//
// 边界约定：
//   - Role/Content 不做白名单校验：合法角色由领域层（agent.Run）保证；
//   - 转换前要求至少一条消息，避免把空消息体发给远端；
//   - 思考过程（reasoning_content）随 Result.Thinking 返回，但不回传模型。
type providerChat struct {
	client   provider.Completer // 最小能力依赖；是否支持流式在运行时探测
	model    string
	thinking bool // 是否显式开启思考模式（thinking: enabled）
}

// AdapterOption 以函数式选项配置适配器。
type AdapterOption func(*providerChat)

// WithThinking 控制请求是否显式开启思考模式（thinking: enabled）。
// 默认关闭，保持原有行为；开启后模型产出 reasoning_content，
// 由 Result.Thinking 返回给调用方。
func WithThinking(enabled bool) AdapterOption {
	return func(p *providerChat) { p.thinking = enabled }
}

// NewProviderChat 构造基于 provider.Completer 的适配器。
// 注入接口而非具体 Client，是"探测 + 降级"的前提：字段必须能容纳能力
// 不同的多种实现。*provider.Client 天然满足 Completer 且支持流式，可直接传入。
func NewProviderChat(client provider.Completer, model string, opts ...AdapterOption) Completer {
	p := &providerChat{client: client, model: model}
	for _, o := range opts {
		o(p)
	}
	return p
}

// Complete 统一入口：探测流式能力并选择路径。
func (p *providerChat) Complete(ctx context.Context, turns []Turn) (Result, error) {
	if len(turns) == 0 {
		return Result{}, errors.New("agent: empty conversation")
	}

	req := &provider.ChatRequest{
		Model:    p.model,
		Messages: make([]provider.Message, 0, len(turns)),
	}
	if p.thinking {
		req.Thinking = provider.ThinkingEnabled()
	}
	for _, t := range turns {
		req.Messages = append(req.Messages, provider.Message{Role: t.Role, Content: t.Content})
	}

	if streamer, ok := p.client.(provider.Streamer); ok {
		return p.completeViaStream(ctx, streamer, req)
	}
	return p.completeViaNonStream(ctx, req)
}

// completeViaStream 走流式：增量 content 拼成回复正文，增量 reasoning 拼成思考过程。
func (p *providerChat) completeViaStream(ctx context.Context, s provider.Streamer, req *provider.ChatRequest) (Result, error) {
	var reply, thinking strings.Builder
	err := s.Stream(ctx, req, provider.StreamCallbacks{
		OnThinking: func(seg string) error { thinking.WriteString(seg); return nil },
		OnContent:  func(seg string) error { reply.WriteString(seg); return nil },
	})
	if err != nil {
		return Result{}, err
	}
	return Result{Reply: reply.String(), Thinking: thinking.String()}, nil
}

// completeViaNonStream 走非流式（降级路径）。
func (p *providerChat) completeViaNonStream(ctx context.Context, req *provider.ChatRequest) (Result, error) {
	resp, err := p.client.Complete(ctx, req)
	if err != nil {
		return Result{}, err
	}
	if len(resp.Choices) == 0 {
		return Result{}, errors.New("agent: provider returned no choices")
	}
	msg := resp.Choices[0].Message
	return Result{Reply: msg.Content, Thinking: msg.ReasoningContent}, nil
}
