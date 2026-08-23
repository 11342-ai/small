// 组合根：显式组装依赖，直观展示依赖顺序与解耦结构。
//
//	main → internal/agent → internal/provider
//	                  ↘  internal/config
//	                  ↘  internal/tool（agent 工具循环依赖；工具实现在组合根注册）
package main

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"strings"

	"small/internal/agent"
	"small/internal/config"
	"small/internal/provider"
	"small/internal/tool"
	"small/internal/tool/builtin"
)

func main() {
	// 1. 加载配置（唯一一次读取环境变量，随后以 struct 整体注入）。
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	// 2. 装配：provider.Client → 适配器 → Agent，依赖全部在组合根注入。
	//    WithThinking(true)：开启思考模式，让 Result.Thinking 携带推理过程。
	//    内置工具清单由 builtin.RegisterBuiltins 集中注册（新增工具不改这里），
	//    adapter 收冻结的声明（reg.List()），agent 收注册表执行调用。
	client := provider.New(cfg)
	reg := tool.New()
	if err := builtin.RegisterBuiltins(reg); err != nil {
		log.Fatalf("register builtin tools: %v", err)
	}
	a := agent.New(
		agent.NewProviderChat(client, cfg.Model, reg.List(),
			agent.WithThinking(true),
		),
		reg,
		agent.WithSystemPrompt("你是一个简洁的助手，回答尽量控制在三句话以内。可用工具：echo（原样返回文本）。"),
	)

	// 3. 多轮对话循环：stdin 逐行输入，"exit" 退出。
	//    Agent.Run 每轮追加历史并推进一轮，多轮上下文由 Agent 内部维护。
	fmt.Println("开始多轮对话（输入 exit 退出）：")
	scanner := bufio.NewScanner(os.Stdin)
	ctx := context.Background()
	for scanner.Scan() {
		input := strings.TrimSpace(scanner.Text())
		if input == "" {
			continue
		}
		if input == "exit" {
			break
		}
		result, err := a.Run(ctx, input)
		if err != nil {
			log.Fatalf("agent: %v", err)
		}
		if result.Thinking != "" {
			fmt.Printf("thinking: %s\n", result.Thinking)
		}
		fmt.Printf("assistant: %s\n\n", result.Reply)
	}
	if err := scanner.Err(); err != nil {
		log.Fatalf("read stdin: %v", err)
	}
}
