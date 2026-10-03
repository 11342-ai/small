package k8s

// 运行期失败归因：启动期的能力判定（见 Zoo/model/k8s-diagnosis.md §16）只回答"开局能不能用"；
// 开局之后集群断开，工具回灌给模型的原文是传输层错误——"dial tcp 192.168.49.2:8443:
// connect: connection refused"。这句话里没有"集群没了"这个结论，模型得自己推断；推断错了，
// 就会把"取不到证据"读成"Pod 没问题"（降级不可见）。这里把连接类失败翻成一句有结论的短句，
// 是"降级必须可见"在运行期的延续，与 §16 的启动期判定互补。
//
// 形态：只给结论短语（`集群不可达（connection refused）`），不带"接下来该怎么做"——建议语按受众
// 在调用方各拼一次：`/diag` 用 IsConnFailure 判断后给用户补一句，模型侧由 base 提示词统一交代
// （见 §17.3）。这样同一句短语可以同时进工具回灌与证据包 notes，不必为两个通道养两套文案。
//
// 边界：只认传输层（连不上 / 超时）。4xx 业务错误（pods not found、Forbidden）原样回灌——
// 它们的原文已经说清了，再包一层反而丢掉细节。但"原样"不等于不脱敏：凡是要把错误原文带出去的场合
// 都过 RedactCredentials——kubeconfig 的 server 允许 `user:pass@host` 写法，而 net/http 只把 password
// 掩成 `***`，用户名会随错误原文露出来（见 Zoo/model/k8s-diagnosis.md §17.6）。

import (
	"context"
	"errors"
	"net"
	"regexp"
	"strings"
)

// credInURL 匹配 URL 里的 userinfo（`://user[:pass]@`）：只抹凭据，保留 scheme/host/port/path。
// 只认 "scheme://" 形态，普通文本里的 @（如 `pods "web@0" not found`）不会被误伤。
var credInURL = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.\-]*://)[^/@\s]*@`)

// RedactCredentials 把文本里 URL 的 userinfo 抹成 `***`（`https://user:pass@h/x` → `https://***@h/x`）。
// 用在所有会把错误原文带出去的地方：工具回灌、启动告警、`/diag` 拒绝文案。
func RedactCredentials(s string) string {
	if s == "" || !strings.Contains(s, "://") {
		return s
	}
	return credInURL.ReplaceAllString(s, "${1}***@")
}

// connFailure 连接类失败的定性。connOK 表示"不属于这一类"，调用方按原始错误回灌。
type connFailure int

const (
	connOK connFailure = iota
	connUnreachable
	connTimeout
)

// classifyConn 把传输层错误归成"不可达 / 超时"，并给出一个短原因串（进文案的括号）。
// 判据顺序：先 errors.Is/As（结构化，不依赖错误文本），再回落到关键子串——client-go 把错误
// 包了 url.Error/net.OpError 好几层，字符串兜底最省事也最不容易漏。
func classifyConn(err error) (connFailure, string) {
	if err == nil {
		return connOK, ""
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return connTimeout, "context deadline exceeded"
	}
	// DNS 先判：解析失败（含 NXDOMAIN）用 dnsErr.Err 当原因，比统一写"i/o timeout"准确。
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		if dnsErr.Err != "" {
			return connUnreachable, dnsErr.Err
		}
		return connUnreachable, "dns lookup failed"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return connTimeout, "i/o timeout"
	}
	msg := err.Error()
	// 顺序有讲究：先超时后"dial tcp"——拨号超时的原文同时含两者，不能归成"连接被拒"。
	for _, m := range []struct {
		sub  string
		kind connFailure
	}{
		{"i/o timeout", connTimeout},
		{"context deadline exceeded", connTimeout},
		{"connection refused", connUnreachable},
		{"no route to host", connUnreachable},
		{"network is unreachable", connUnreachable},
		{"no such host", connUnreachable},
		{"dial tcp", connUnreachable},
	} {
		if strings.Contains(msg, m.sub) {
			return m.kind, m.sub
		}
	}
	return connOK, ""
}

// ExplainError 把采集错误渲染成回灌文案：连接类失败给结论短语（`集群不可达（connection refused）`），
// 其余原样返回 error 文本。调用方保留既有的"<动作>失败: "前缀，这里只管错误那一段。
//
// 括号里留的是分类依据（connection refused / i/o timeout / no such host 这类），不保留完整原文：
// 地址与端口回答的是"连的是哪个集群"，那件事由启动输出的 context 与证据包的 target.context 交代，
// 而模型要的是"这次没取到数据"这个结论（见 Zoo/model/k8s-diagnosis.md §17.2）。
func ExplainError(err error) string {
	if err == nil {
		return ""
	}
	switch kind, reason := classifyConn(err); kind {
	case connUnreachable:
		return "集群不可达（" + RedactCredentials(reason) + "）"
	case connTimeout:
		return "调用超时（" + RedactCredentials(reason) + "）"
	default:
		return RedactCredentials(err.Error())
	}
}

// IsConnFailure 判断是否连接类失败（不可达 / 超时）。给"要不要补一句用户向建议"的调用方用：
// 业务错误（Pod 不存在、权限不足）套同一句建议会给出走不通的下一步，所以建议不能无条件拼。
func IsConnFailure(err error) bool {
	kind, _ := classifyConn(err)
	return kind != connOK
}
