// 组合根：显式组装依赖，直观展示依赖顺序与解耦结构。
//
//	main → internal/agent → internal/provider
//	                  ↘  internal/config
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
)

func main() {
	// 1. 加载配置（唯一一次读取环境变量，随后以 struct 整体注入）。
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	// 2. 装配：provider.Client → 适配器 → Agent，依赖全部在组合根注入。
	//    WithThinking(true)：开启思考模式，让 Result.Thinking 携带推理过程。
	client := provider.New(cfg)
	a := agent.New(
		agent.NewProviderChat(client, cfg.Model, agent.WithThinking(true)),
		agent.WithSystemPrompt("你是一个简洁的助手，回答尽量控制在三句话以内。"),
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
