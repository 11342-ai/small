// 组合根：显式组装依赖，直观展示依赖顺序与解耦结构。
//
//	main → internal/agent → internal/session
//	                  ↘  internal/provider
//	                  ↘  internal/config
//	                  ↘  internal/tool（agent 工具循环依赖；工具实现在组合根注册）
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"small/internal/agent"
	"small/internal/config"
	"small/internal/provider"
	"small/internal/session"
	"small/internal/tool"
	"small/internal/tool/builtin"
)

func main() {
	// 会话 id：--session 指定则恢复/续聊该会话；缺省生成时间戳 id 开新会话。
	sessionID := flag.String("session", "", "会话 ID（缺省创建新会话）")
	flag.Parse()

	// 1. 加载配置（唯一一次读取环境变量/配置文件，随后以 struct 整体注入）。
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	// 2. 装配：provider.Client → 适配器 → Agent，依赖全部在组合根注入。
	//    会话仓库（session 部件）由 config 提供目录；历史从磁盘恢复（新会话为空）。
	store, err := session.New(cfg.SessionDir)
	if err != nil {
		log.Fatalf("session store: %v", err)
	}
	id := *sessionID
	if id == "" {
		id = time.Now().Format("20060102-150405")
	}
	fmt.Printf("会话 ID: %s（存储目录 %s）\n", id, cfg.SessionDir)

	msgs, err := store.Load(id)
	if err != nil {
		log.Fatalf("load session %q: %v", id, err)
	}

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
		store,
		agent.WithSystemPrompt("你是一个简洁的助手，回答尽量控制在三句话以内。可用工具：echo（原样返回文本）。"),
		agent.WithSession(id),
		agent.WithHistory(agent.FromSession(msgs)),
		agent.WithTokenBudget(cfg.MaxTokens),
	)

	// 3. 多轮对话循环：stdin 逐行输入，"exit" 退出。
	//    Agent.Run 每轮追加历史并推进一轮；持久化由 agent 在 Run 成功时自动落盘。
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
