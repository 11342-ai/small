package k8s

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/version"
	"k8s.io/client-go/discovery"
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
	// defaultPreflightTimeout 启动期探测的总预算（一次 Preflight 的所有请求共享它）。
	// 为什么是这一档：连接被拒是立即失败，只有"黑洞地址"（丢包 / ACL 静默丢弃）才会等满，
	// 所以它就是启动最坏多花的时间——够一次 /version 往返，又不至于让人以为程序卡死。
	// 不进 config.yml（见 Zoo/model/k8s-diagnosis.md §16.3）。
	defaultPreflightTimeout = 2 * time.Second
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
	// metricsGroupVersion 指标能力的判据版本：必须与我们的客户端实际调用的版本一致
	// （metricsclient 生成自 metrics.k8s.io/v1beta1）。集群若只服务别的版本（如 v1），
	// 我们的调用同样用不了，判"不可用"才是正确结论——所以这里不取组的 preferredVersion。
	metricsGroupVersion = "metrics.k8s.io/v1beta1"
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
	// PreflightTimeout 启动期探测的总预算；<=0 用 defaultPreflightTimeout。
	// 不进 config.yml（探测是启动期一次性开销，不值得多一个配置面）；留这个字段是为了
	// 让单测能把超时压到几十毫秒（见 Zoo/model/k8s-diagnosis.md §16.3）。
	PreflightTimeout time.Duration
}

// Collector 采集层模块对象：持有 typed clientset 与 metrics 客户端，方法按证据类别细分。
type Collector struct {
	cfg     Config
	core    kubernetes.Interface
	metrics metricsclient.Interface
	// caps 启动期探测结果（Preflight 写入，注册层/提示词层读 MetricsUsable）。
	// 启动期单线程写、之后只读，故不加锁。
	caps *Capabilities
	// apiServer 实际访问的 apiserver 地址（New 里取 rest.Host）：与 cfg.Context 一起回答
	// "连的是哪个集群的哪个端点"，进产物（target.api_server）与启动输出（见 §17.6）。
	apiServer string
}

func New(cfg Config) (*Collector, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	rules.ExplicitPath = cfg.KubeConfig
	// context 归一化：kube_context 未配置时取 kubeconfig 的 current-context 写回 cfg.Context。
	// 为什么在构造期做：Target.Context（证据包/报告）直接取 cfg.Context，不归一化的话默认场景是空串，
	// 回放时分辨不出证据来自哪个集群（原意图见 Zoo/model/k8s-diagnosis.md §9、§16.3 末）。
	// 读文件失败不在这里报错——交给下面的 ClientConfig() 给出准确的失败原因。
	if cfg.Context == "" {
		if raw, err := rules.Load(); err == nil {
			cfg.Context = raw.CurrentContext
		}
	}
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
	coll := newWithClients(cfg, core, mc)
	// 端点地址与 context 同处归一化：都进产物，回放时才能回答"这份证据来自哪个集群的哪个端点"。
	coll.apiServer = rest.Host
	return coll, nil
}

func newWithClients(cfg Config, core kubernetes.Interface, mc metricsclient.Interface) *Collector {
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultCallTimeout
	}
	if cfg.EvidenceMaxTokens == 0 {
		cfg.EvidenceMaxTokens = DefaultEvidenceMaxToken
	}
	if cfg.PreflightTimeout <= 0 {
		cfg.PreflightTimeout = defaultPreflightTimeout
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

// --- 启动期探测（preflight） ---

// Availability 能力可用性三态。未知不等于不可用：未知按可用处理（fail-open），
// 避免"探测本身不准"把本来可用的能力裁掉（见 Zoo/model/k8s-diagnosis.md §16.2）。
type Availability int

const (
	// CapUnknown 未探测到：探测本身报错（超时、权限、后端 503）——按可用处理。
	CapUnknown Availability = iota
	// CapAvailable 明确可用。
	CapAvailable
	// CapUnavailable 明确不可用（API 组/版本不存在）。
	CapUnavailable
)

// Capabilities 启动期探测结果：给组合根做用户可见输出，也给注册层/提示词层做裁剪。
type Capabilities struct {
	// Context 实际生效的 kube context（New 归一化后的 cfg.Context）。
	Context string
	// APIServer 实际访问的 apiserver 地址（供启动输出展示"连的是哪个端点"）。
	APIServer string
	// ServerVersion apiserver 的 git 版本（仅用于告知用户）。
	ServerVersion string
	// Metrics metrics.k8s.io 指标能力的可用性。
	Metrics Availability
}

// Preflight 启动期探测：判集群可达 + 探可选能力。结果留在 Collector 上（注册层/提示词层读 MetricsUsable）。
//
// error 只表示"集群不可达 / 鉴权失败"这类致命情形——调用方按既有 nil 退化（不注册工具）；
// 可选能力缺失不算 error，进 Capabilities 交给调用方裁剪。
// 只在启动期调用一次：单线程写、之后只读，故不加锁。
func (c *Collector) Preflight(ctx context.Context) (*Capabilities, error) {
	// 一次探测的所有请求共享同一个预算（不是每个请求各一份）。
	pctx, cancel := context.WithTimeout(ctx, c.cfg.PreflightTimeout)
	defer cancel()
	ver, err := serverVersion(pctx, c.core.Discovery())
	if err != nil {
		return nil, fmt.Errorf("connect apiserver（context=%s）: %w", c.cfg.Context, err)
	}
	caps := &Capabilities{
		Context:       c.cfg.Context,
		APIServer:     c.apiServer,
		ServerVersion: ver.GitVersion,
		Metrics:       probeMetrics(pctx, c.core.Discovery()),
	}
	c.caps = caps
	return caps, nil
}

// MetricsUsable 指标工具是否该注册：只有"明确不可用"才为 false。
// 未知与未探测都按可用处理——fail-open，同时让"没调过 Preflight"的调用方与既有测试行为不变。
func (c *Collector) MetricsUsable() bool {
	return c.caps == nil || c.caps.Metrics != CapUnavailable
}

// probeMetrics 判指标能力：只探我们客户端真正要用的那个版本（理由见 metricsGroupVersion 注释）。
// 三态由错误类型区分：NotFound 才是"明确缺失"（可裁），其余（503、超时、权限）都算"未探测到"。
func probeMetrics(ctx context.Context, d discovery.DiscoveryInterface) Availability {
	_, err := serverResourcesForGroupVersion(ctx, d, metricsGroupVersion)
	switch {
	case err == nil:
		return CapAvailable
	case apierrors.IsNotFound(err):
		return CapUnavailable
	default:
		return CapUnknown
	}
}

// serverVersion / serverResourcesForGroupVersion 优先走带 ctx 的变体（2s 预算要靠它才生效）：
// DiscoveryInterface 只承诺无 ctx 版本，真实客户端与 fake 都实现了 ...WithContext；
// 断言失败时退化为无 ctx 调用——探测仍能跑，只是不受预算约束（不为此再包一层接口）。
func serverVersion(ctx context.Context, d discovery.DiscoveryInterface) (*version.Info, error) {
	if dc, ok := d.(discovery.DiscoveryInterfaceWithContext); ok {
		return dc.ServerVersionWithContext(ctx)
	}
	return d.ServerVersion()
}

func serverResourcesForGroupVersion(ctx context.Context, d discovery.DiscoveryInterface, gv string) (*metav1.APIResourceList, error) {
	if dc, ok := d.(discovery.DiscoveryInterfaceWithContext); ok {
		return dc.ServerResourcesForGroupVersionWithContext(ctx, gv)
	}
	return d.ServerResourcesForGroupVersion(gv)
}
