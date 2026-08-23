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
	"path/filepath"
	"strings"
	"time"

	"small/internal/agent"
	"small/internal/config"
	"small/internal/memory"
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
	mem, err := memory.New(cfg.MemoryDir)
	if err != nil {
		log.Fatalf("memory store: %v", err)
	}
	reg := tool.New()
	if err := builtin.RegisterBuiltins(reg, mem); err != nil {
		log.Fatalf("register builtin tools: %v", err)
	}
	// 系统提示 = 基础行为 + 记忆启动注入（MEMORY.md 常驻层，见 Zoo/model/memory.md §7）。
	// 组合根拼字符串即可，agent 循环零改动。
	prompt := "你是一个简洁的助手，回答尽量控制在三句话以内。" +
		"可用工具：echo（原样返回文本）、memory_search（检索长期记忆）、memory_get（读取记忆块）、memory_save（记住新事实）。" +
		"回答涉及先前决策、偏好、待办或项目事实时，先调用 memory_search 检索；" +
		"仅当用户明确要求记住某事时，才调用 memory_save 写入长期记忆。"
	if boot := loadBootstrapMemory(cfg.MemoryDir); boot != "" {
		prompt += "\n\n<memory>\n" + boot + "\n</memory>"
	}
	a := agent.New(
		agent.NewProviderChat(client, cfg.Model, reg.List(),
			agent.WithThinking(true),
		),
		reg,
		store,
		agent.WithSystemPrompt(prompt),
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

// bootstrapLimit MEMORY.md 启动注入的上限（字符数）：防常驻 token 膨胀。
// 完整内容仍可通过 memory_search 检索（设计文档 §7）。
const bootstrapLimit = 2000

// loadBootstrapMemory 读取 MEMORY.md 作为启动注入内容；文件不存在或不可读
// 返回空（无常驻记忆不阻塞启动），超限按字符截断并注明（只截注入副本，不动文件本体）。
func loadBootstrapMemory(dir string) string {
	data, err := os.ReadFile(filepath.Join(dir, "MEMORY.md"))
	if err != nil {
		return ""
	}
	s := strings.TrimSpace(string(data))
	if s == "" {
		return ""
	}
	// 按 rune 截断而非字节：字节切分可能把中文字符拦腰截断成非法 UTF-8。
	if runes := []rune(s); len(runes) > bootstrapLimit {
		s = string(runes[:bootstrapLimit]) + "\n（已截断，完整内容可用 memory_search 检索）"
	}
	return s
}
