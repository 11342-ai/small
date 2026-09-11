package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"small/internal/agent"
	"small/internal/command"
	"small/internal/k8s"
	"small/internal/persona"
	"small/internal/policy"
	"small/internal/session"
	"small/internal/tool"
)

// 命令实现（组合根展示层的具体命令）。
// 与 main.go 分离：main 只保留注册清单（一览命令全集），命令逻辑收敛到具名构造函数。
// 依赖按项目惯例"结构协作对象显式入参"，由 main 装配时逐个传入（见 Zoo/model/cli.md §3）。

// cmdHelp 显示可用命令。
func cmdHelp(cmdReg *command.Registry) command.CommandSpec {
	return command.CommandSpec{
		Name: "/help", Usage: "显示可用命令",
		Run: func(context.Context, []string) (string, error) { return cmdReg.HelpText(), nil },
	}
}

// cmdExit 退出（通过哨兵错误让组合根跳出循环，命令系统不感知 I/O）。
func cmdExit() command.CommandSpec {
	return command.CommandSpec{
		Name: "/exit", Usage: "退出",
		Run: func(context.Context, []string) (string, error) { return "", errExit },
	}
}

// cmdSession 显示当前会话 id。
func cmdSession(id string) command.CommandSpec {
	return command.CommandSpec{
		Name: "/session", Usage: "显示当前会话 id",
		Run: func(context.Context, []string) (string, error) { return id, nil },
	}
}

// cmdTools 列出可用工具（来自注册表，与模型看到的一致）。
func cmdTools(reg *tool.Registry) command.CommandSpec {
	return command.CommandSpec{
		Name: "/tools", Usage: "列出可用工具",
		Run: func(context.Context, []string) (string, error) {
			var b strings.Builder
			for _, s := range reg.List() {
				b.WriteString(s.Name)
				b.WriteString("\n")
			}
			return strings.TrimSuffix(b.String(), "\n"), nil
		},
	}
}

// cmdPdf 显式进入 pdf 工作流分支（Zoo/model/workflow.md §4.1 命令通道）：
// 校验文档路径参数后，把一条"按 pdf 工作流处理该文档"的用户消息注入 agent 循环
// （Run 成功返回注入消息 + 哨兵 errInject，main 循环检测后当作用户输入送 Run，
// 走正常路径含 plan/proposals ctx 与持久化）。命令本身无副作用（Perm Pass）。
func cmdPdf() command.CommandSpec {
	return command.CommandSpec{
		Name: "/pdf", Usage: "显式进入 pdf 工作流处理文档：/pdf <文档路径>",
		Run: func(_ context.Context, args []string) (string, error) {
			if len(args) == 0 || strings.TrimSpace(args[0]) == "" {
				return "", errors.New("用法：/pdf <文档路径>（如 /pdf ~/Pdf/报告.pdf）")
			}
			path := strings.Join(args, " ") // 路径可能含空格，按原始参数重新拼接
			msg := "请按 pdf 工作流处理文档：" + path + "（用户已通过 /pdf 命令显式进入 pdf 分支）"
			return msg, errInject
		},
	}
}

// cmdDiag 显式进入 k8s 诊断分支：/diag <namespace>/<pod>。
// 与 /pdf 同款注入机制（errInject）：命令只做参数校验与"目标可达性预检"，诊断本身由 agent
// 按 k8s-diag 分支执行（见 Zoo/model/k8s-diagnosis.md §6）。
// coll 为 nil 表示集群未接入（采集器构造失败时组合根传 nil）：直接拒绝，别让模型去猜。
func cmdDiag(coll *k8s.Collector) command.CommandSpec {
	return command.CommandSpec{
		Name: "/diag", Usage: "诊断 Pod 异常：/diag <namespace>/<pod>",
		Run: func(ctx context.Context, args []string) (string, error) {
			if coll == nil {
				return "", errors.New("K8s 未接入：检查 kubeconfig（config.yml 的 kube_config，缺省 ~/.kube/config）后重启")
			}
			if len(args) == 0 || strings.TrimSpace(args[0]) == "" {
				return "", errors.New("用法：/diag <namespace>/<pod>（如 /diag default/web-0）")
			}
			target := strings.TrimSpace(args[0])
			ns, pod, ok := strings.Cut(target, "/")
			if !ok || ns == "" || pod == "" {
				return "", fmt.Errorf("参数格式应为 <namespace>/<pod>，收到 %q", target)
			}
			// 预检：目标读不到就别烧一轮模型调用（拼错名字 / RBAC 不足都会走到这里）。
			// 用短超时，避免命令侧卡住交互。
			probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			if _, err := coll.Pod(probeCtx, ns, pod); err != nil {
				return "", fmt.Errorf("读不到目标 Pod %s/%s：%v", ns, pod, err)
			}
			msg := "请按 k8s-diag 工作流诊断 Pod " + ns + "/" + pod +
				"（用户已通过 /diag 命令显式进入 k8s-diag 分支：先用 k8s_evidence 一次拿全证据，" +
				"再按收集→分析→验证推进，最后用 k8s_report 提交报告）"
			return msg, errInject
		},
	}
}

// cmdClear 清空当前会话（删除会话文件）。破坏性命令，Perm=Ask 需确认。
func cmdClear(a *agent.Agent) command.CommandSpec {
	return command.CommandSpec{
		Name: "/clear", Usage: "清空当前会话（删除会话文件）", Perm: policy.Ask,
		Run: func(_ context.Context, _ []string) (string, error) {
			if err := a.Reset(); err != nil {
				return "", fmt.Errorf("clear session: %w", err)
			}
			return "会话已清空（重新开始）", nil
		},
	}
}

// cmdPersona 切换人格（仅无消息会话可用，定型点 = 第一条消息，见 Zoo/model/cli.md §6）。
// p/metaNeeded 传指针：命令执行时会更新"当前人格"与"是否待定型"，main 循环据此写 meta。
func cmdPersona(a *agent.Agent, mgr *persona.Manager, store *session.Store, id string,
	compose func(persona.Persona) string, p *persona.Persona, metaNeeded *bool) command.CommandSpec {
	return command.CommandSpec{
		Name: "/persona", Usage: "切换人格（仅无消息会话可用）",
		Run: func(_ context.Context, args []string) (string, error) {
			if len(a.History()) != 0 {
				return "", errors.New("会话已定型，无法切换人格（人格创建时定型，见 Zoo/model/persona.md）")
			}
			if len(args) == 0 {
				return "", errors.New("用法：/persona <name>（未命中时错误信息附可用人格列表）")
			}
			np, err := mgr.Get(args[0])
			if err != nil {
				return "", err
			}
			*p = np
			a.SetSystemPrompt(compose(np))
			if !*metaNeeded {
				// 已有 meta 的空会话（改版前创建）：立即重写；新建会话由首条消息定型。
				if err := store.WriteMeta(id, session.Header{Meta: session.HeaderMeta{Persona: np.Name}}); err != nil {
					return "", fmt.Errorf("write session meta: %w", err)
				}
			}
			return "人格已切换为 " + np.Name, nil
		},
	}
}

// confirmName 执行前确认（组合根注入的 Ask 权限回调）：显示命令名并读 y/N。
func confirmName(name string) bool {
	fmt.Printf("确认执行 %s？[y/N] ", name)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	ans := strings.ToLower(strings.TrimSpace(line))
	return ans == "y" || ans == "yes"
}
