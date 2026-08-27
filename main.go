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
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"small/internal/agent"
	"small/internal/command"
	"small/internal/config"
	"small/internal/memory"
	"small/internal/persona"
	"small/internal/provider"
	"small/internal/session"
	"small/internal/tool"
	"small/internal/tool/builtin"
	"small/internal/trace"
)

func main() {
	// 会话 id：--session 指定则恢复/续聊该会话；缺省生成时间戳 id 开新会话。
	sessionID := flag.String("session", "", "会话 ID（缺省创建新会话）")
	// 人格：仅对**新建会话**生效；恢复会话时以会话内记录的 meta 为准（一个对话一个人格）。
	personaName := flag.String("persona", "", "对话人格（缺省 default；可用人格见 internal/persona/personas/）")
	flag.Parse()
	// 位置参数防护：多余参数几乎都是 flag 拼写错误（如 `-- persona` 中间多空格，`persona`
	// 会变成位置参数被静默忽略、用户误以为生效）。直接报错暴露，提示正确写法，而不是静默降级。
	if args := flag.Args(); len(args) > 0 {
		log.Fatalf("unexpected arguments: %v（flag 与参数之间勿加空格，正确写法如 --persona catton）", args)
	}

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

	// 人格选择（一个对话一个人格，见 Zoo/model/persona.md）：
	// 优先级 = 会话头行 meta（已定型）> --persona flag（仅真正的新建会话）> 默认人格。
	// 判定"是否新建"看头行而不是消息条数：创建后未聊过的会话（只有头行）也是"已定型"，
	// 恢复时同样以 meta 为准，flag 不覆盖。
	mgr, err := persona.Load()
	if err != nil {
		log.Fatalf("load persona: %v", err)
	}
	p := mgr.Default()
	if *personaName != "" {
		p, err = mgr.Get(*personaName)
		if err != nil {
			log.Fatalf("%v", err) // 未命中：错误信息已附可用人格列表
		}
	}
	// metaNeeded 标记"新建会话尚未定型"：meta 延后到首条消息写入（见 Zoo/model/cli.md §6）。
	// 定型点从"创建时刻"挪到"第一条消息"：/persona 可在无消息窗口内重定人格。
	metaNeeded := false
	if h, err := store.Meta(id); err != nil {
		log.Fatalf("read session meta: %v", err)
	} else if h.Meta.Persona != "" {
		// 会话已定型：meta 优先。
		if hp, err := mgr.Get(h.Meta.Persona); err == nil {
			if *personaName != "" && *personaName != hp.Name {
				fmt.Printf("会话已绑定人格 %q，忽略 --persona %q\n", hp.Name, *personaName)
			}
			p = hp
		} else {
			// 会话记录的人格本地不存在（如人格文件被删）：回退，不阻塞续聊。
			fmt.Printf("会话人格 %q 不存在，回退到默认人格\n", h.Meta.Persona)
		}
	} else if len(msgs) == 0 {
		// 真正的新建会话（无头行且无消息）：flag/默认 待定型，首条消息时写 meta。
		metaNeeded = true
	}
	// else：旧文件（有消息、无头行，改版前创建的会话）——回退 flag/默认，不回填 meta（兼容最简）。

	client := provider.New(cfg)
	mem, err := memory.New(cfg.MemoryDir)
	if err != nil {
		log.Fatalf("memory store: %v", err)
	}
	reg := tool.New()
	// exec 工具最小安全版（演进序短期第二步）：白名单 + 超时 + 每步确认，fail-closed；
	// 配置在组合根显式注入（见 Zoo/model/tool-extend.md B 档）。
	if err := builtin.RegisterBuiltins(reg, builtin.Deps{
		Mem: mem,
		Exec: &builtin.ExecConfig{
			Allow:   execAllow,
			Timeout: 30 * time.Second,
			Confirm: func(cmd string, args []string) bool {
				fmt.Printf("确认执行 exec：%s %s？[y/N] ", cmd, strings.Join(args, " "))
				line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
				ans := strings.ToLower(strings.TrimSpace(line))
				return ans == "y" || ans == "yes"
			},
		},
	}); err != nil {
		log.Fatalf("register builtin tools: %v", err)
	}
	// 系统提示三段式装配（契约层→人格层→记忆层）收敛到 persona.Compose，main 只提供素材不手拼
	// （见 Zoo/model/persona.md §4）。角色句（"你是一个简洁的助手…"）已移入 personas/default.md：
	// 选别的人格时不继承"简洁"约束。组合根拼字符串即可，agent 循环零改动。
	base := "可用工具：echo（原样返回文本）、exec（执行白名单内只读命令，每次需用户确认）、memory_search（检索长期记忆）、memory_get（读取记忆块）、memory_save（记住新事实）。" +
		"回答涉及先前决策、偏好、待办或项目事实时，先调用 memory_search 检索；" +
		"仅当用户明确要求记住某事时，才调用 memory_save 写入长期记忆。"
	memBlock := ""
	if boot := loadBootstrapMemory(cfg.MemoryDir); boot != "" {
		memBlock = "<memory>\n" + boot + "\n</memory>"
	}
	// compose 三段式装配唯一入口：/persona 重定人格时复用（组合根闭包持有素材）。
	compose := func(pp persona.Persona) string { return persona.Compose(base, pp, memBlock) }
	prompt := compose(p)
	fmt.Printf("人格: %s\n", p.Name)
	// 工具调用轨迹（观测元数据，独立于回灌历史）：跟随会话写 <sid>.trace.jsonl，
	// 与 session 同目录、同生命周期。agent 不感知 trace——写盘动作包装成 observer 注入
	// （见 Zoo/model/trace.md）。
	tr := trace.New(filepath.Join(cfg.SessionDir, id+".trace.jsonl"))
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
		// 工具调用实时展示 + 轨迹落盘：逐条打印名称/入参/结果（截断摘要，防长结果刷屏）。
		// trace 写失败属次要失败（观测数据），只记日志不打断对话。
		agent.WithToolObserver(func(ev agent.ToolCallEvent) {
			fmt.Printf("→ %s(%s)\n", ev.Name, truncate(ev.Args, 120))
			mark := ""
			if ev.Result.IsError {
				mark = " [失败]"
			}
			fmt.Printf("  ↳ %s%s\n", truncate(ev.Result.Data, 200), mark)
			if err := tr.Append(trace.Entry{
				TS: time.Now(), Session: id, Round: ev.Round,
				Name: ev.Name, Args: ev.Args, Data: ev.Result.Data,
				IsError: ev.Result.IsError, DurationMs: ev.Duration.Milliseconds(),
			}); err != nil {
				log.Printf("trace: %v", err)
			}
		}),
	)

	// 3. 命令系统：平行于 tool 骨架（用户触发 vs 模型触发），壳在 internal/command，
	//    命令实现收敛在 main_commands.go 的具名构造函数，此处保持注册清单——
	//    一眼看全命令全集（见 Zoo/model/cli.md）。
	cmdReg := command.New()
	// Ask 权限确认回调（nil-safe，fail-closed：不注入则 Ask 命令一律拒绝）。
	cmdReg.Confirm = confirmName
	cmdReg.Register(cmdHelp(cmdReg))
	cmdReg.Register(cmdExit())
	cmdReg.Register(cmdSession(id))
	cmdReg.Register(cmdTools(reg))
	cmdReg.Register(cmdClear(a))
	cmdReg.Register(cmdPersona(a, mgr, store, id, compose, &p, &metaNeeded))

	// 4. 多轮对话循环：stdin 逐行输入，"exit" 退出。
	//    Agent.Run 每轮追加历史并推进一轮；持久化由 agent 在 Run 成功时自动落盘。
	fmt.Println("开始多轮对话（输入 exit 退出，/help 查看命令）：")
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
		// 命令分发："/" 开头交给命令系统（未知命令报错但不退出）。
		if handled, output, err := cmdReg.Dispatch(ctx, input); handled {
			if errors.Is(err, errExit) {
				break
			}
			if err != nil {
				fmt.Printf("%v\n", err)
				continue
			}
			if output != "" {
				fmt.Println(output)
			}
			continue
		}
		// 新建会话定型：首条消息前写 meta（定型点 = 第一条消息，见 model/cli.md §6）。
		if metaNeeded {
			if err := store.WriteMeta(id, session.Header{Meta: session.HeaderMeta{Persona: p.Name}}); err != nil {
				log.Fatalf("write session meta: %v", err)
			}
			metaNeeded = false
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

// errExit 退出信号：/exit 命令通过哨兵错误让组合根跳出循环（命令系统不感知 I/O）。
var errExit = errors.New("exit")

// execAllow exec 工具默认白名单（只读命令，保守起步；配置化留给 cli.md §5"动态化位"）。
var execAllow = []string{"ls", "cat", "grep", "head", "tail", "echo", "date", "pwd", "whoami"}

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

// truncate 超长文本按 rune 截断为摘要，供工具调用实时展示防刷屏（尾部注明已截断）。
func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…（已截断）"
}
