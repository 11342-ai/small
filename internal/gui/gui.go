// Package gui 浏览器 app-server 展示层（--gui 可选入口，Zoo/model/gui.md）。
//
// 形态：Go net/http 起本地 server + embed 单页（前端手写，不引框架）——
// 解决两个真实痛点：对话中文乱码（终端无解，浏览器 UTF-8 天然解决）与
// markdown 可视化阅读（doc_parse 产物的配套展示）。范围焊死：输入 + 展示 +
// 命令切换，其他不做；mp4 只留后期位（/view 识别视频扩展名返回 video 页）。
//
// 依赖方向：main → gui → agent/session/tool/config（展示层，组合根装配注入），
// 不反向依赖。agent 的回复流式增量经 WithReplyObserver 透出（adapter.replyObs），
// 工具事件经 WithToolObserver 透出——单请求串行（前端发送后禁用按钮直到 done）。
package gui

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"small/internal/agent"
)

//go:embed index.html
var indexHTML []byte

// CommandFunc GUI 命令处理器（gui.md §4.4 v3）：组合根注入，复用 CLI 命令注册表
// （cmdReg.Dispatch）——main 负责把 errInject 翻译成 inject 消息，gui 不感知哨兵。
type CommandFunc func(ctx context.Context, input string) (handled bool, output, inject string, err error)

// Config GUI server 配置（组合根注入）。
type Config struct {
	// Addr 监听地址（config.yml gui_addr，缺省 127.0.0.1:8090；仅本机服务）。
	Addr string
	// FileRoot 工作区根（/view 路径解析的基准之一）。
	FileRoot string
	// CacheRoot 文档解析缓存目录（/view 可展示 doc_parse 产物）。
	CacheRoot string
	// Command 命令处理器（nil 时 / 消息当普通对话）：识别后直接响应，
	// 注入消息（/pdf）走 agent。见 CommandFunc。
	Command CommandFunc
}

// Server GUI server：持有 agent 与当前 SSE 事件写入器。
// mu 串行对话：agent 回调（OnReply/OnTool）写"当前响应"，单请求保证不串。
type Server struct {
	cfg Config
	a   *agent.Agent
	mu  sync.Mutex // 串行对话（回调写当前响应，前端发送后禁用按钮直到 done）
	cur func(ev, data string)
}

// New 构造空 server（agent 由组合根构造后经 Attach 绑定）。
func New(cfg Config) *Server {
	return &Server{cfg: cfg}
}

// Attach 绑定 agent（组合根构造 agent 后调用，回调经 OnReply/OnTool 落到当前响应）。
func (s *Server) Attach(a *agent.Agent) { s.a = a }

// OnReply agent 流式回复增量回调（agent.WithReplyObserver 传入，gui.md §4.1）。
func (s *Server) OnReply(seg string) { s.emit("reply", seg) }

// OnTool agent 工具事件回调（agent.WithToolObserver 传入，展示工具调用过程）。
func (s *Server) OnTool(ev agent.ToolCallEvent) {
	s.emit("tool", fmt.Sprintf("%s: %s", ev.Name, truncate(ev.Result.Data, 120)))
}

func (s *Server) emit(ev, data string) {
	if s.cur != nil {
		s.cur(ev, data)
	}
}

// ListenAndServe 起 server：/（单页）、/chat（对话流）、/view（markdown 展示）、
// /history（历史恢复，gui.md §4.6）。
func (s *Server) ListenAndServe() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/chat", s.handleChat)
	mux.HandleFunc("/view", s.handleView)
	mux.HandleFunc("/history", s.handleHistory)
	return http.ListenAndServe(s.cfg.Addr, mux)
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(indexHTML)
}

// handleChat 对话流：POST {message} → agent.Run，事件写 text/event-stream
// （reply 增量 / tool 工具事件 / done 完整回复兜底 / error 失败）。
func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var in struct {
		Message string `json:"message"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	msg := strings.TrimSpace(in.Message)
	if msg == "" {
		http.Error(w, "message 为空", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	fl, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	s.cur = func(ev, data string) {
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev, data)
		if fl != nil {
			fl.Flush()
		}
	}
	defer func() { s.cur = nil }()

	// 命令分发（gui.md §4.4 v3）：/ 开头消息交给注入的 CLI 命令处理器（识别后直接
	// 响应，不裸塞给模型）；注入消息（/pdf）继续走 agent。
	if strings.HasPrefix(msg, "/") {
		if inject, reply, handled := s.handleCommand(r.Context(), msg); handled {
			if inject != "" {
				msg = inject // /pdf：注入消息走 agent（同 CLI errInject 语义）
			} else {
				s.emit("done", reply)
				return
			}
		}
	}

	result, err := s.a.Run(r.Context(), msg)
	if err != nil {
		s.emit("error", err.Error())
		return
	}
	// done 兜底：流式后端已逐段推过 reply，done 只作结束标记；非流式降级整段走这里。
	s.emit("done", result.Reply)
}

// handleCommand GUI 命令分发：委托给注入的 CommandFunc（复用 CLI 命令表）。
// 返回 (注入消息, 回复文本, 是否命令)；Command 为 nil 时 / 消息按普通对话处理（防御）。
func (s *Server) handleCommand(ctx context.Context, msg string) (inject, reply string, handled bool) {
	if s.cfg.Command == nil {
		return "", "", false
	}
	handled, out, inj, err := s.cfg.Command(ctx, msg)
	if err != nil {
		return "", "命令失败: " + err.Error(), true
	}
	if !handled {
		return "", "", false
	}
	if inj != "" {
		return inj, "", true
	}
	return "", out, true
}

// handleHistory 返回对话历史（gui.md §4.6）：agent.History() 过滤 user/assistant
// 且跳过空内容（工具调用轮的空回复），前端刷新后据此恢复渲染。
func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	type msg struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	var out []msg
	for _, t := range s.a.History() {
		if t.Role != "user" && t.Role != "assistant" {
			continue
		}
		if strings.TrimSpace(t.Content) == "" {
			continue // 工具调用轮空回复不展示
		}
		out = append(out, msg{Role: t.Role, Content: t.Content})
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(out); err != nil {
		http.Error(w, "编码失败: "+err.Error(), http.StatusInternalServerError)
	}
}

// handleView markdown 展示：/view?path=<路径> → goldmark 渲染 HTML。
// mp4 后期位：识别视频扩展名返回 video 页（gui.md §8）。
func (s *Server) handleView(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimSpace(r.URL.Query().Get("path"))
	abs, err := resolveViewPath(s.cfg.FileRoot, s.cfg.CacheRoot, p)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ext := strings.ToLower(filepath.Ext(abs))
	switch ext {
	case ".mp4", ".webm", ".mov":
		renderVideo(w, p)
		return
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		http.Error(w, "读取失败: "+err.Error(), http.StatusBadRequest)
		return
	}
	html, err := markdownToHTML(data)
	if err != nil {
		http.Error(w, "渲染失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(html)
}

// resolveViewPath /view 路径校验（对齐 file 工具只读语义）：工作区/缓存内放行；
// 工作区外绝对路径须过敏感系统目录黑名单（fail-closed）。相对路径按工作区根解析。
func resolveViewPath(fileRoot, cacheRoot, p string) (string, error) {
	if strings.TrimSpace(p) == "" {
		return "", fmt.Errorf("path 参数缺失")
	}
	var abs string
	if filepath.IsAbs(p) {
		abs = filepath.Clean(p)
	} else if cacheRoot != "" {
		abs = filepath.Clean(filepath.Join(cacheRoot, p))
	} else {
		abs = filepath.Clean(filepath.Join(fileRoot, p))
	}
	// 工作区内或缓存内：放行。
	inRoot := func(root string) bool {
		if root == "" {
			return false
		}
		rel, err := filepath.Rel(root, abs)
		return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
	}
	if inRoot(fileRoot) || inRoot(cacheRoot) {
		return abs, nil
	}
	if isSensitivePath(abs) {
		return "", fmt.Errorf("路径位于敏感系统目录，拒绝展示：%q", p)
	}
	return abs, nil
}

// sensitiveRoots 只读放宽后的敏感系统目录黑名单（对齐 builtin 同名表，防展示系统文件）。
var sensitiveRoots = []string{
	"/etc", "/proc", "/sys", "/usr", "/bin", "/sbin", "/boot", "/dev", "/root", "/var",
}

func isSensitivePath(abs string) bool {
	clean := filepath.Clean(abs)
	for _, s := range sensitiveRoots {
		if clean == s || strings.HasPrefix(clean, s+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// renderVideo mp4 后期位：视频文件返回内嵌播放器页面（gui.md §8，本期预留）。
func renderVideo(w http.ResponseWriter, path string) {
	fmt.Fprintf(w, `<!DOCTYPE html><html><head><meta charset="utf-8"><title>%s</title></head>
<body><video src="/view?path=%s" controls autoplay style="max-width:100%%"></video></body></html>`,
		filepath.Base(path), url.QueryEscape(path))
}

// truncate 长文本按 rune 截断为摘要（工具事件展示防刷屏）。
func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}
