package k8s

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var annotationWhitelist = []string{
	"deployment.kubernetes.io/revision",
	"kubernetes.io/change-cause",
	"kubectl.kubernetes.io/restartedAt",
	"prometheus.io/scrape",
	"prometheus.io/port",
	"prometheus.io/path",
}

// pickAnnotations 按白名单挑选注解（无命中返回 nil，省掉空对象）。
func pickAnnotations(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	var out map[string]string
	for _, k := range annotationWhitelist {
		if v, ok := m[k]; ok {
			if out == nil {
				out = map[string]string{}
			}
			out[k] = v
		}
	}
	return out
}

// rfc3339 格式化元数据时间（零值返回空串，避免 JSON 里出现 "0001-01-01T00:00:00Z" 噪声）。
func rfc3339(t metav1.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Time.Format(time.RFC3339)
}

// rfc3339Ptr 同上，用于可选时间字段（deletionTimestamp 等）。
func rfc3339Ptr(t *metav1.Time) string {
	if t == nil {
		return ""
	}
	return rfc3339(*t)
}

// fmtCPU 把 CPU 数量转成人读形式（核心数或毫核："1" / "250m"）。
func fmtCPU(q resource.Quantity) string {
	if q.IsZero() {
		return ""
	}
	m := q.MilliValue()
	if m%1000 == 0 {
		return strconv.FormatInt(m/1000, 10)
	}
	return strconv.FormatInt(m, 10) + "m"
}

// fmtMemory 把内存数量转成 MiB（不足 1MiB 用字节），免去模型做单位换算。
func fmtMemory(q resource.Quantity) string {
	if q.IsZero() {
		return ""
	}
	b := q.Value()
	mib := float64(b) / (1 << 20)
	if mib < 1 {
		return strconv.FormatInt(b, 10) + "B"
	}
	s := strconv.FormatFloat(mib, 'f', 1, 64)
	return strings.TrimSuffix(s, ".0") + "Mi"
}

// fmtResourceList 格式化 resources（cpu/memory 走人读单位，其余原样）。
func fmtResourceList(rl corev1.ResourceList) map[string]string {
	if len(rl) == 0 {
		return nil
	}
	out := make(map[string]string, len(rl))
	for name, q := range rl {
		switch name {
		case corev1.ResourceCPU:
			out[string(name)] = fmtCPU(q)
		case corev1.ResourceMemory, corev1.ResourceEphemeralStorage:
			out[string(name)] = fmtMemory(q)
		default:
			out[string(name)] = q.String()
		}
	}
	return out
}

// percent 计算用量占限值的百分比（限值未设返回 nil）：用 MilliValue 统一 CPU 与内存，
// 比值不受单位影响；保留 1 位小数。
func percent(usage, limit resource.Quantity) *float64 {
	if limit.IsZero() {
		return nil
	}
	l := float64(limit.MilliValue())
	if l == 0 {
		return nil
	}
	p := math.Round(float64(usage.MilliValue())/l*1000) / 10
	return &p
}

// probeView 把探针转成摘要（无探针返回 nil）。
func probeView(kind string, p *corev1.Probe) *ProbeView {
	if p == nil {
		return nil
	}
	return &ProbeView{
		Type:                kind,
		Handler:             probeHandler(p),
		InitialDelaySeconds: p.InitialDelaySeconds,
		PeriodSeconds:       p.PeriodSeconds,
		TimeoutSeconds:      p.TimeoutSeconds,
		FailureThreshold:    p.FailureThreshold,
		SuccessThreshold:    p.SuccessThreshold,
	}
}

// probeHandler 渲染探针动作（"怎么探"是 Probe Failed 归因的关键信息）。
func probeHandler(p *corev1.Probe) string {
	switch {
	case p.HTTPGet != nil:
		scheme := string(p.HTTPGet.Scheme)
		if scheme == "" {
			scheme = "HTTP"
		}
		return fmtHTTPProbe(scheme, p.HTTPGet.Path, p.HTTPGet.Port.String())
	case p.TCPSocket != nil:
		return "tcp " + p.TCPSocket.Port.String()
	case p.Exec != nil:
		return "exec [" + strings.Join(p.Exec.Command, " ") + "]"
	case p.GRPC != nil:
		return "grpc " + strconv.Itoa(int(p.GRPC.Port))
	}
	return ""
}

// fmtHTTPProbe 拼 HTTP 探针描述（host 可选）。
func fmtHTTPProbe(scheme, path, port string) string {
	if path == "" {
		path = "/"
	}
	return "http " + scheme + " " + path + ":" + port
}

// envFromRefs 描述 envFrom 来源（配置缺失类启动失败常源于此），不取具体值避免泄露内容。
func envFromRefs(sources []corev1.EnvFromSource) []string {
	var out []string
	for _, s := range sources {
		var ref string
		switch {
		case s.ConfigMapRef != nil:
			ref = "configMapRef/" + s.ConfigMapRef.Name
		case s.SecretRef != nil:
			ref = "secretRef/" + s.SecretRef.Name
		default:
			continue
		}
		out = append(out, ref)
	}
	return out
}

// envRefs 描述容器 env 的"名与来源"，不落明文值（值可能是机密，而排查只需要知道它指哪儿）。
// 渲染形态：
//
//	NAME                                   直接在 spec 里写常量值
//	NAME<-configMapRef/app-config          valueFrom.configMapKeyRef
//	NAME<-secretRef/app-secret             valueFrom.secretKeyRef
//	NAME<-fieldRef/metadata.namespace      downward API（Pod 自身字段）
//	NAME<-resourceFieldRef/limits.memory   downward API（容器资源值）
//
// 用途：CreateContainerConfigError（引用的 cm/secret 不存在或缺 key）类启动失败，
// 光看事件只能知道"缺 key"，要对着 spec 才能确认是哪个变量引用的。
func envRefs(envs []corev1.EnvVar) []string {
	var out []string
	for _, e := range envs {
		if src := envValueSource(e.ValueFrom); src != "" {
			out = append(out, e.Name+"<-"+src)
			continue
		}
		out = append(out, e.Name)
	}
	return out
}

// envValueSource 把 valueFrom 渲染成来源串（无 valueFrom 返回空串）。
func envValueSource(v *corev1.EnvVarSource) string {
	if v == nil {
		return ""
	}
	switch {
	case v.ConfigMapKeyRef != nil:
		return "configMapRef/" + v.ConfigMapKeyRef.Name
	case v.SecretKeyRef != nil:
		return "secretRef/" + v.SecretKeyRef.Name
	case v.FieldRef != nil:
		return "fieldRef/" + v.FieldRef.FieldPath
	case v.ResourceFieldRef != nil:
		return "resourceFieldRef/" + v.ResourceFieldRef.Resource
	}
	return ""
}

// --- 亲和性 ---

// affinityView 转亲和性摘要（无配置返回 nil，不占字段）。
func affinityView(a *corev1.Affinity) *AffinityView {
	if a == nil {
		return nil
	}
	v := &AffinityView{}
	if n := a.NodeAffinity; n != nil {
		// required 那层多套了一层 NodeSelector（它才是 term 列表的持有者）。
		var required []string
		if s := n.RequiredDuringSchedulingIgnoredDuringExecution; s != nil {
			required = nodeTermsText(s.NodeSelectorTerms)
		}
		v.NodeAffinity = rulesOrNil(required,
			preferredNodeTermsText(n.PreferredDuringSchedulingIgnoredDuringExecution))
	}
	if p := a.PodAffinity; p != nil {
		v.PodAffinity = rulesOrNil(
			podTermsText(p.RequiredDuringSchedulingIgnoredDuringExecution),
			preferredPodTermsText(p.PreferredDuringSchedulingIgnoredDuringExecution))
	}
	if p := a.PodAntiAffinity; p != nil {
		v.PodAntiAffinity = rulesOrNil(
			podTermsText(p.RequiredDuringSchedulingIgnoredDuringExecution),
			preferredPodTermsText(p.PreferredDuringSchedulingIgnoredDuringExecution))
	}
	if v.NodeAffinity == nil && v.PodAffinity == nil && v.PodAntiAffinity == nil {
		return nil
	}
	return v
}

// rulesOrNil 两边都空就不产出规则对象（避免 JSON 里出现 {"node_affinity":{}} 这种空壳）。
func rulesOrNil(required, preferred []string) *AffinityRules {
	if len(required) == 0 && len(preferred) == 0 {
		return nil
	}
	return &AffinityRules{Required: required, Preferred: preferred}
}

// nodeTermsText 渲染硬性节点选择条件。多个 term 之间是 OR（满足其一即可），
// 故一条一个元素；term 内部是 AND，在字符串里显式写出连接词。
func nodeTermsText(terms []corev1.NodeSelectorTerm) []string {
	var out []string
	for _, t := range terms {
		out = append(out, nodeTermText(t))
	}
	return out
}

// preferredNodeTermsText 渲染软性节点偏好（带 weight：权重高的先满足）。
func preferredNodeTermsText(terms []corev1.PreferredSchedulingTerm) []string {
	var out []string
	for _, t := range terms {
		out = append(out, fmt.Sprintf("weight=%d: %s", t.Weight, nodeTermText(t.Preference)))
	}
	return out
}

// nodeTermText 渲染一个节点选择 term 的表达式（term 内是 AND）。
func nodeTermText(t corev1.NodeSelectorTerm) string {
	var parts []string
	for _, e := range t.MatchExpressions {
		parts = append(parts, fmt.Sprintf("%s %s %v", e.Key, e.Operator, e.Values))
	}
	for _, f := range t.MatchFields {
		parts = append(parts, fmt.Sprintf("field %s %s %v", f.Key, f.Operator, f.Values))
	}
	if len(parts) == 0 {
		// API 语义：空的 term 不匹配任何节点（不是"匹配全部"），写反会得出相反的结论。
		return "(空 term：不匹配任何节点)"
	}
	return strings.Join(parts, " AND ")
}

// podTermsText 渲染硬性 Pod 亲和/反亲和条件（一个 term 一个元素）。
func podTermsText(terms []corev1.PodAffinityTerm) []string {
	var out []string
	for _, t := range terms {
		out = append(out, podTermText(t))
	}
	return out
}

// preferredPodTermsText 渲染软性 Pod 亲和/反亲和偏好（带 weight）。
func preferredPodTermsText(terms []corev1.WeightedPodAffinityTerm) []string {
	var out []string
	for _, t := range terms {
		out = append(out, fmt.Sprintf("weight=%d: %s", t.Weight, podTermText(t.PodAffinityTerm)))
	}
	return out
}

// podTermText 渲染一条 Pod 亲和 term：拓扑域 + 目标 Pod 的标签选择器 + 命名空间范围。
// topologyKey 必须留：它定义"同一个域"的边界（hostname 是同一台机器、zone 是一个可用区），
// 没有它，podAntiAffinity 的规则读起来像"随便找台机器"，也没法和节点数量对上。
func podTermText(t corev1.PodAffinityTerm) string {
	parts := []string{"topologyKey=" + t.TopologyKey}
	if s := labelSelectorText(t.LabelSelector); s != "" {
		parts = append(parts, "pod "+s)
	}
	// namespaces 与 namespaceSelector 是并集（API 语义），两者都要写出来。
	if len(t.Namespaces) > 0 {
		parts = append(parts, "namespaces=["+strings.Join(t.Namespaces, ",")+"]")
	}
	if s := labelSelectorText(t.NamespaceSelector); s != "" {
		parts = append(parts, "namespaces matching "+s)
	}
	return strings.Join(parts, ", ")
}

// labelSelectorText 渲染标签选择器（matchLabels 与 matchExpressions 取交集）。
// matchLabels 是 map，必须排序后再拼——否则同一份 spec 每次输出顺序不同，证据包无法逐字对照。
func labelSelectorText(s *metav1.LabelSelector) string {
	if s == nil {
		return ""
	}
	var parts []string
	for k, v := range s.MatchLabels {
		parts = append(parts, k+"="+v)
	}
	sort.Strings(parts)
	for _, e := range s.MatchExpressions {
		parts = append(parts, fmt.Sprintf("%s %s %v", e.Key, e.Operator, e.Values))
	}
	return strings.Join(parts, ",")
}

// volumeSource 描述卷来源（挂载失败类排查用；只留类型与引用名）。
func volumeSource(v corev1.Volume) string {
	switch {
	case v.PersistentVolumeClaim != nil:
		return "persistentVolumeClaim/" + v.PersistentVolumeClaim.ClaimName
	case v.ConfigMap != nil:
		return "configMap/" + v.ConfigMap.Name
	case v.Secret != nil:
		return "secret/" + v.Secret.SecretName
	case v.EmptyDir != nil:
		return "emptyDir"
	case v.HostPath != nil:
		return "hostPath/" + v.HostPath.Path
	case v.Projected != nil:
		return "projected"
	case v.DownwardAPI != nil:
		return "downwardAPI"
	case v.Ephemeral != nil:
		// generic ephemeral 卷：控制器会按 <pod>-<volume> 自动建一块 PVC，是 Pending 的一个来源。
		return "ephemeral"
	}
	return ""
}

// fmtToleration 渲染容忍（Pending 归因要对比 taints 与 tolerations）。
func fmtToleration(t corev1.Toleration) string {
	s := t.Key
	if t.Operator == corev1.TolerationOpEqual && t.Value != "" {
		s += "=" + t.Value
	} else if t.Operator != "" {
		s += " " + string(t.Operator)
	}
	if t.Effect != "" {
		s += ":" + string(t.Effect)
	}
	if t.TolerationSeconds != nil {
		s += " (tolerationSeconds=" + strconv.FormatInt(*t.TolerationSeconds, 10) + ")"
	}
	return s
}

// eventTime 取事件的"最近发生时间"：新版事件用 eventTime，旧版用 lastTimestamp，
// 都没有则回落到 firstTimestamp（排序与时间窗过滤都依赖它）。
func eventTime(e *corev1.Event) time.Time {
	if !e.EventTime.IsZero() {
		return e.EventTime.Time
	}
	if !e.LastTimestamp.IsZero() {
		return e.LastTimestamp.Time
	}
	return e.FirstTimestamp.Time
}

// estimateTokens 字符近似估算 token 数（约 1 字符 ≈ 1/3 token，偏大估算防溢出）：
// 与 agent/compact.go 同一套近似。本包是叶子部件不能 import agent，故此处重写，
// 两处如需调整应同步（仅用于证据包预算裁剪，精度要求不高）。
func estimateTokens(s string) int {
	return len(s)/3 + 1
}

// unsafeNameChars 文件名里不允许出现的字符（防路径注入：目标名来自用户输入）。
var unsafeNameChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// safeName 把 namespace/pod 名清洗成安全的文件名片段（顺带去掉首尾的点与横线，
// 防 "../" 之类的相对路径片段残留）。
func safeName(s string) string {
	s = unsafeNameChars.ReplaceAllString(s, "-")
	s = strings.Trim(s, ".-")
	if len(s) > 100 {
		s = s[:100]
	}
	if s == "" {
		s = "unknown"
	}
	return s
}
