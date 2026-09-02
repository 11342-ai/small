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
	"small/internal/gui"
	"small/internal/kb"
	"small/internal/memory"
	"small/internal/persona"
	"small/internal/provider"
	"small/internal/session"
	"small/internal/tool"
	"small/internal/tool/builtin"
	"small/internal/trace"
	"small/internal/workflow"
)

func main() {
	// 会话 id：--session 指定则恢复/续聊该会话；缺省生成时间戳 id 开新会话。
	sessionID := flag.String("session", "", "会话 ID（缺省创建新会话）")
	// 人格：仅对**新建会话**生效；恢复会话时以会话内记录的 meta 为准（一个对话一个人格）。
	personaName := flag.String("persona", "", "对话人格（缺省 default；可用人格见 internal/persona/personas/）")
	// GUI 界面（gui.md）：--gui 起浏览器 app-server（对话流式 + markdown 展示），
	// CLI 主路径原样保留（GUI 与 CLI 是平行 I/O 层）；监听地址走 config.yml gui_addr
	// （缺省 127.0.0.1:8090，配置来源单一对齐其他目录/预算项）。
	guiFlag := flag.Bool("gui", false, "启动 GUI 界面（浏览器 app-server，对话 + markdown 展示）")
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
	kbStore, err := kb.New(cfg.KbDir)
	if err != nil {
		log.Fatalf("kb store: %v", err)
	}
	// 工作流分支（workflow.md §3）：embed 资产解析（坏文件 = 开发错误 fail fast），
	// 分支清单渲染进 base 提示词（契约层），模型按触发条件自动进入对应分支。
	wfMgr, err := workflow.Load()
	if err != nil {
		log.Fatalf("load workflows: %v", err)
	}
	reg := tool.New()
	// 文件类工具（tool-extend.md A 档）：工作区根经 detectRoot 注入（路径 policy，见 §5）。
	fileRoot := detectRoot()
	// exec 工具最小安全版（演进序短期第二步）：白名单 + 超时（每步确认已迁至 agent
	// 权限横切层，exec 在权限表中为 Ask，见 Zoo/model/policy.md）。
	if err := builtin.RegisterBuiltins(reg, builtin.Deps{
		Mem:  mem,
		Kb:   kbStore,
		File: &builtin.FileConfig{Root: fileRoot},
		Exec: &builtin.ExecConfig{
			Allow:   execAllow,
			Timeout: 30 * time.Second,
		},
		// 文档解析工具（tool-lit.md）：缓存根经 config.cache_dir 注入（缺省 ~/.small/cache）。
		Cache: &builtin.LitConfig{Root: cfg.CacheDir},
	}); err != nil {
		log.Fatalf("register builtin tools: %v", err)
	}
	// 系统提示三段式装配（契约层→人格层→记忆层）收敛到 persona.Compose，main 只提供素材不手拼
	// （见 Zoo/model/persona.md §4）。角色句（"你是一个简洁的助手…"）已移入 personas/default.md：
	// 选别的人格时不继承"简洁"约束。组合根拼字符串即可，agent 循环零改动。
	base := "可用工具：echo（原样返回文本）、exec（执行白名单内只读命令，每次需用户确认）、plan（维护多步任务的分步执行清单）、file_read（读工作区文件，支持 offset/limit 窗口化）、file_list（列目录/找文件）、file_tree（目录树速览工作区结构）、doc_search（工作区关键词搜索）、file_write（写工作区文件，整体覆盖，每次需用户确认）、file_edit（按字符串替换编辑工作区文件，每次需用户确认）、propose_file_write（提议写文件，确认后才落地）、propose_file_edit（提议编辑文件，确认后才落地）、web_fetch（抓取网页转文本）、memory_search（检索长期记忆）、memory_get（读取记忆块）、memory_save（记住新事实）、kb_tree（知识库结构树/域视图查询）、kb_refs（知识库引用报告，删除知识点前必查）、kb_check（知识库巡检）、kb_write（把知识点写入知识库，自动生成 frontmatter 与认知深度）、doc_parse（用 lit 解析 PDF/Word 等文档到缓存，返回路径与摘要）、doc_read（读解析产物，窗口化）、doc_clean（清洗产物噪声：分页符/水印重复/页码行）、file_diff（比对两个文本文件内容差异：段落定位 + 段内字/词细标并回显原文；ignore 可组合 space/punct/symbol 忽略空白/标点/特殊字符，只看实质差异）。" +
		"回答涉及先前决策、偏好、待办或项目事实时，先调用 memory_search 检索；" +
		"用户提供链接并希望了解其内容时，用 web_fetch 读取；" +
		"仅当用户明确要求记住某事时，才调用 memory_save 写入长期记忆；" +
		"涉及知识库的结构/归属/引用关系时用 kb_tree/kb_refs，删除或整理知识点前先 kb_refs 查影响面、改完用 kb_check 巡检；" +
		"用户要求把知识点记入知识库时，先向用户确认所属域（新域需批准）再调 kb_write；" +
		"用户要求解析 PDF/Word 等文档时用 doc_parse，解析后先 doc_clean 清洗噪声再 doc_read 细读；整理文档前先询问用户在原文件直接整理还是新开文件；" +
		"核对两个版本/处理前后文档（如原版 vs 整理产物）时用 file_diff 比对，只需实质内容差异时带 ignore 组合（space/punct/symbol）；" +
		"访问工作区外路径（如 ~/Pdf）前，先向用户说明要访问的目录/文件并征得同意，确认后再访问；系统敏感目录（/etc /proc /usr 等）一律不访问。"
	// 工作流分支清单（workflow.md §3.3）：注入"可用工作流分支"段，模型按触发条件
	// 自动进入对应分支（如 PDF 解析任务 → pdf 分支），稳定处理而非临场发挥。
	base += wfMgr.RenderBranch()
	memBlock := ""
	if boot := loadBootstrapMemory(cfg.MemoryDir); boot != "" {
		memBlock = "<memory>\n" + boot + "\n</memory>"
	}
	// compose 三段式装配唯一入口：/persona 重定人格时复用（组合根闭包持有素材）。
	compose := func(pp persona.Persona) string { return persona.Compose(base, pp, memBlock) }
	prompt := compose(p)
	fmt.Printf("人格: %s\n", p.Name)

	// GUI 分支（gui.md）：--gui 时起浏览器 app-server 后退出 main（不走 CLI 循环）。
	// GUI 下 Ask 确认无 stdin 交互——工具 confirm 与命令 Confirm 一律拒绝（guard 回灌
	// "未获确认"，模型会提示用户写操作请用 CLI）；doc_* 全 Pass 不受影响。
	if *guiFlag {
		// GUI 命令注册表（gui.md §4.4 v3）：复用 CLI cmdXxx 构造函数，语义与 CLI 一致。
		// Ask 命令（/clear）无确认 → 拒绝；/pdf 注入消息经 CommandFunc 翻译走 agent。
		guiCmd := command.New()
		guiCmd.Confirm = func(string) bool { return false }
		guiCmd.Register(cmdHelp(guiCmd))
		guiCmd.Register(cmdSession(id))
		guiCmd.Register(cmdTools(reg))
		guiCmd.Register(cmdPdf())
		guiSrv := gui.New(gui.Config{
			Addr:      cfg.GUIAddr,
			FileRoot:  fileRoot,
			CacheRoot: cfg.CacheDir,
			Command: func(ctx context.Context, input string) (bool, string, string, error) {
				handled, output, err := guiCmd.Dispatch(ctx, input)
				if !handled {
					return false, "", "", nil
				}
				if errors.Is(err, errInject) {
					return true, "", output, nil // /pdf：注入消息走 agent
				}
				if err != nil {
					return true, "", "", err
				}
				return true, output, "", nil
			},
		})
		// GUI 也落盘工具调用轨迹（与 CLI 同一 <sid>.trace.jsonl）：SSE 实时推送 +
		// trace 写盘双通道（gui.md §4.2 补充）。
		guiTr := trace.New(filepath.Join(cfg.SessionDir, id+".trace.jsonl"))
		guiToolObs := func(ev agent.ToolCallEvent) {
			guiSrv.OnTool(ev)
			appendTrace(guiTr, ev, id)
		}
		guiAgent := buildAgent(client, reg, store, cfg, prompt, id, msgs,
			guiToolObs, func(string) bool { return false }, guiSrv.OnReply)
		guiSrv.Attach(guiAgent)
		// 依赖 guiAgent 的命令（闭包运行时才解引用 guiCmd，先注册后填充等价）。
		guiCmd.Register(cmdClear(guiAgent))
		guiCmd.Register(cmdPersona(guiAgent, mgr, store, id, compose, &p, &metaNeeded))
		fmt.Printf("GUI: http://%s （Ctrl+C 退出）\n", cfg.GUIAddr)
		log.Fatalf("gui server: %v", guiSrv.ListenAndServe())
	}

	// 工具调用轨迹（观测元数据，独立于回灌历史）：跟随会话写 <sid>.trace.jsonl，
	// 与 session 同目录、同生命周期。agent 不感知 trace——写盘动作包装成 observer 注入
	// （见 Zoo/model/trace.md）。
	tr := trace.New(filepath.Join(cfg.SessionDir, id+".trace.jsonl"))
	// 工具调用实时展示 + 轨迹落盘：逐条打印名称/入参/结果（截断摘要，防长结果刷屏）。
	cliToolObs := func(ev agent.ToolCallEvent) {
		fmt.Printf("→ %s(%s)\n", ev.Name, truncate(ev.Args, 120))
		mark := ""
		if ev.Result.IsError {
			mark = " [失败]"
		}
		fmt.Printf("  ↳ %s%s\n", truncate(ev.Result.Data, 200), mark)
		appendTrace(tr, ev, id)
	}
	a := buildAgent(client, reg, store, cfg, prompt, id, msgs, cliToolObs, confirmName, nil)

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
	cmdReg.Register(cmdPdf())

	// 4. 多轮对话循环：stdin 逐行输入，"exit" 退出。
	//    Agent.Run 每轮追加历史并推进一轮；持久化由 agent 在 Run 成功时自动落盘。
	fmt.Println("开始多轮对话（输入 exit 退出，/help 查看命令）：")
	scanner := bufio.NewScanner(os.Stdin)
	ctx := context.Background()
	for {
		// 输入提示符（cli.md §11）：每次轮到用户输入主命令时打印，任务完成后
		// 回到等待态同样显示——readline 式提示，用户明确知道当前可输入。
		fmt.Print(">>> ")
		if !scanner.Scan() {
			break
		}
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
			if errors.Is(err, errInject) {
				// 命令注入（workflow.md §4.1）：/pdf 等命令把消息注入 agent 循环，
				// 落到下方 Run 路径（含 meta 定型与 plan/proposals ctx）。
				if output == "" {
					continue // 注入消息为空则忽略（防御，正常不会发生）
				}
				input = output
			} else {
				if err != nil {
					fmt.Printf("%v\n", err)
				} else if output != "" {
					fmt.Println(output)
				}
				continue
			}
		}
		// 新建会话定型：首条消息前写 meta（定型点 = 第一条消息，见 model/cli.md §6）。
		if metaNeeded {
			if err := store.WriteMeta(id, session.Header{Meta: session.HeaderMeta{Persona: p.Name}}); err != nil {
				log.Fatalf("write session meta: %v", err)
			}
			metaNeeded = false
		}
		// 计划清单（plan.md §3）+ 提议暂存（tool-fs.md §4.5）：Run 级，每轮新建经 ctx 注入；
		// agent 零改动。提议的确认在 Run 结束后由组合根处理（下方"提议落地"段）。
		propStore := builtin.NewProposedStore()
		runCtx := builtin.WithPlan(ctx, builtin.NewPlanStore())
		runCtx = builtin.WithProposals(runCtx, propStore)
		result, err := a.Run(runCtx, input)
		if err != nil {
			log.Fatalf("agent: %v", err)
		}
		if result.Thinking != "" {
			fmt.Printf("thinking: %s\n", result.Thinking)
		}
		fmt.Printf("assistant: %s\n\n", result.Reply)
		// 提议落地（权限横切最小形态，tool-fs.md §4.5）：Run 结束后检查待确认提议——
		// 展示摘要 + 读用户输入，确认后 ApplyProposed（原子写），拒绝即丢弃。模型不感知该交互。
		if pend := propStore.Pending(); len(pend) > 0 {
			fileCfg := &builtin.FileConfig{Root: fileRoot}
			fmt.Printf("检测到 %d 条待确认改动：\n", len(pend))
			for _, e := range pend {
				kind := "写入"
				if e.Kind == "edit" {
					kind = "编辑"
				}
				fmt.Printf("#%d %s %s\n%s\n应用？[y/N] ", e.ID, kind, e.Path, truncate(e.Content, 200))
				line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
				ans := strings.ToLower(strings.TrimSpace(line))
				if ans == "y" || ans == "yes" {
					if err := builtin.ApplyProposed(fileCfg, e); err != nil {
						fmt.Printf("  ↳ 落地失败: %v\n", err)
					} else {
						fmt.Printf("  ↳ 已应用 #%d（%s）\n", e.ID, e.Path)
					}
				} else {
					fmt.Printf("  ↳ 已拒绝 #%d\n", e.ID)
				}
			}
			propStore.Clear()
		}
	}
	if err := scanner.Err(); err != nil {
		log.Fatalf("read stdin: %v", err)
	}
}

// errExit 退出信号：/exit 命令通过哨兵错误让组合根跳出循环（命令系统不感知 I/O）。
var errExit = errors.New("exit")

// errInject 命令注入信号（workflow.md §4.1）：/pdf 等命令成功后返回注入消息 + 该哨兵，
// 组合根把注入消息当作用户输入送 agent（走正常 Run 路径，含 plan/proposals ctx 与持久化）。
var errInject = errors.New("inject-agent-message")

// execAllow exec 工具默认白名单（只读命令起步；find/cp 为 PDF 管理等动作所需，
// exec 本身是 Ask 每步确认，cp 等写操作有确认门兜底，pdf-workflow.md §6）。
var execAllow = []string{"ls", "cat", "grep", "head", "tail", "echo", "date", "pwd", "whoami", "find", "cp"}

// buildAgent 装配 agent（CLI/GUI 共用，gui.md §4.2）：注入提示词/会话/预算/轮次/权限表。
// toolObs（工具事件）与 confirm（Ask 确认）由调用方传——CLI 用打印+trace / stdin 确认，
// GUI 用 SSE 推送 / 一律拒绝；replyObs 仅 GUI 传（流式回复增量，CLI 为 nil 零回归）。
func buildAgent(client provider.Completer, reg *tool.Registry, store *session.Store, cfg *config.Config,
	prompt, id string, msgs []session.Message,
	toolObs agent.ToolObserver, confirm func(string) bool, replyObs func(string)) *agent.Agent {
	return agent.New(
		agent.NewProviderChat(client, cfg.Model, reg.List(),
			agent.WithThinking(true),
		),
		reg,
		store,
		agent.WithSystemPrompt(prompt),
		agent.WithSession(id),
		agent.WithHistory(agent.FromSession(msgs)),
		agent.WithTokenBudget(cfg.MaxTokens),
		// 工具轮次上限（pdf-workflow.md §5）：config.yml max_tool_rounds，缺省 20，
		// 显式 0 = 不限（agent 用兜底上限防死循环）。
		agent.WithMaxToolRounds(cfg.MaxToolRounds),
		// 权限横切层（policy.md §3.2）：注入工具权限表 + Ask 确认回调。
		agent.WithToolPermissions(builtin.ToolPermissions),
		agent.WithToolConfirm(confirm),
		agent.WithToolObserver(toolObs),
		agent.WithReplyObserver(replyObs),
	)
}

// appendTrace 工具调用事件落盘（trace.md）：CLI 与 GUI 共用同一轨迹记录逻辑
// （名称/入参/结果/耗时/轮次，plan 单独标记供对拍）。写失败属次要失败（观测数据），
// 只记日志不打断对话。
func appendTrace(tr *trace.Store, ev agent.ToolCallEvent, id string) {
	typ := "tool"
	if ev.Name == "plan" {
		typ = "plan"
	}
	if err := tr.Append(trace.Entry{
		TS: time.Now(), Session: id, Round: ev.Round,
		Name: ev.Name, Args: ev.Args, Data: ev.Result.Data,
		IsError: ev.Result.IsError, DurationMs: ev.Duration.Milliseconds(),
		Type: typ,
	}); err != nil {
		log.Printf("trace: %v", err)
	}
}

// detectRoot 工作区定位（tool-extend.md §5）：从 CWD 往上找 marker（go.mod 优先、.git 兜底），
// 未命中回退 CWD。文件工具的搜索根（路径 policy 的边界，§7 待决已定案：go.mod → .git → CWD）。
func detectRoot() string {
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	for dir := wd; ; {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return wd // 到文件系统根仍未命中：回退 CWD
		}
		dir = parent
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

// truncate 超长文本按 rune 截断为摘要，供工具调用实时展示防刷屏（尾部注明已截断）。
func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…（已截断）"
}
