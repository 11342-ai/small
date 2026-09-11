package k8s

import (
	"context"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	metricsclient "k8s.io/metrics/pkg/client/clientset/versioned"
)

const (
	// defaultCallTimeout 单次API调用兜底超时:不设client-go的全局timeout
	// 因为那个日志是流式读（同 provider 层的取舍），全局超时会误杀长读，改为每次调用套ctx超时
	defaultCallTimeout = 30 * time.Second
	// logCallTimeout 日志读单独的预算，比元数据调用宽 3 倍。为什么不共用：日志是流式大对象
	// （最多 1MiB 正文，还可能在服务端攒），与"取一个对象/一页列表"不是一个量级；共用 30s 会把
	// "日志读得慢"判成读失败，记成 required_failed（进而进缺失声明的客观基准、压置信度）——
	// 那是把环境慢当成证据缺。数值是拍的：够覆盖慢链路上 1MiB 的量级，又不至于把卡死拖太久。
	// 真正的"空闲超时"（读到一半不动了就掐、一直有流量就一直等）留作后续，见 §15。
	logCallTimeout = 90 * time.Second
	// defaultEvidenceMaxToken 证据包 token 预算缺省值(对齐 32k 上下文的 1/4)
	DefaultEvidenceMaxToken = 8192
	// defaultLogTailLines 单容器日志默认取尾部行数（故障现场在尾部）。
	defaultLogTailLines = 200
	// defaultLogLimitBytes 单容器日志默认字节上限（超出保尾部并标记截断）。
	defaultLogLimitBytes = 64 << 10
	// defaultEventLimit 事件默认条数上限（Warning 优先保留）。
	defaultEventLimit = 50
	// defaultEventSince 事件默认时间窗口。
	defaultEventSince = time.Hour
	// maxLogReadBytes 单次日志读取的硬上限，防异常大日志把内存拉爆（再从中取尾部）。
	maxLogReadBytes = 1 << 20
)

// Config 采集层配置，由组合根注入（缺省值在 New/newWithClients 内补齐）。
type Config struct {
	// KubeConfig kubeconfig 文件路径（组合根给缺省 ~/.kube/config）。
	KubeConfig string
	// Context 目标 kube context；空表示用 kubeconfig 的 current-context。
	Context string
	// Dir 本次运行的产物目录（证据包与报告落盘处，组合根按会话 id 建，如 ~/.small/k8s/<sid>）。
	Dir string
	// Timeout 单次 API 调用兜底超时；<=0 用 defaultCallTimeout。
	Timeout time.Duration
	// EvidenceMaxTokens 证据包 token 预算；<=0 用 DefaultEvidenceMaxTokens，显式负值不裁。
	EvidenceMaxTokens int
}

// Collector 采集层模块对象：持有 typed clientset 与 metrics 客户端，方法按证据类别细分。
type Collector struct {
	cfg     Config
	core    kubernetes.Interface
	metrics metricsclient.Interface
}

func New(cfg Config) (*Collector, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	rules.ExplicitPath = cfg.KubeConfig
	overrides := &clientcmd.ConfigOverrides{}
	if cfg.Context != "" {
		overrides.CurrentContext = cfg.Context
	}
	rest, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides).ClientConfig()
	if err != nil {
		return nil, err
	}
	rest.UserAgent = "small-k8s-diag"
	core, err := kubernetes.NewForConfig(rest)
	if err != nil {
		return nil, err
	}
	mc, err := metricsclient.NewForConfig(rest)
	if err != nil {
		return nil, err
	}
	return newWithClients(cfg, core, mc), nil
}

func newWithClients(cfg Config, core kubernetes.Interface, mc metricsclient.Interface) *Collector {
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultCallTimeout
	}
	if cfg.EvidenceMaxTokens == 0 {
		cfg.EvidenceMaxTokens = DefaultEvidenceMaxToken
	}
	return &Collector{cfg: cfg, core: core, metrics: mc}
}

// callCtx 元数据类调用（取对象、列列表）的超时：每次调用一个独立预算，由调用方 defer cancel。
func (c *Collector) callCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return c.callCtxFor(ctx, c.cfg.Timeout)
}

// callCtxFor 同上但用指定预算——目前只有日志读走另一套（logCallTimeout）。分开是为了让
// "哪类调用该等多久"在代码里看得见，而不是散在各处各写一个 context.WithTimeout。
// 传 0/负值按"配置里的 Timeout → 包内默认"逐级回落：零预算等于 ctx 一出生就过期，
// 那种失败最难查（看起来像"API 挂了"），所以这里不留这个口子。
func (c *Collector) callCtxFor(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if d <= 0 {
		d = c.cfg.Timeout
	}
	if d <= 0 {
		d = defaultCallTimeout
	}
	return context.WithTimeout(ctx, d)
}
